package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/james00012/cloud-inventory/plugin/core"
)

// Session is a provider running as its own process, reached over its standard
// input and output. A host links this and no cloud SDK.
type Session struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   *bufio.Scanner

	wire    func(string, []byte)
	mu      sync.Mutex
	next    int
	granted []string
	account string
}

// Options is what a session is started with.
type Options struct {
	// Binary is the provider to run, and Args anything it needs.
	Binary string
	Args   []string

	// Features the session asks for. Read is always granted; create and delete
	// have to be asked for, and a session that cannot get all of them fails.
	Features []core.Feature

	// ExpectAccount is the account the host believes it is pointed at.
	ExpectAccount string

	// Caller names the tool, which every audit entry records, and Run
	// identifies this run, which everything created carries as a tag.
	Caller string
	Run    string

	// Scope is the provider's own scope value.
	Scope any

	// Stderr receives the provider's logs, which never mix with the protocol.
	Stderr io.Writer

	// Wire, when set, receives a copy of every line in both directions, which
	// is how a test checks the messages against the protocol's schema.
	Wire func(direction string, line []byte)
}

// Start runs the provider and configures the session, or fails without one.
func Start(ctx context.Context, opts Options) (*Session, error) {
	command := exec.CommandContext(ctx, opts.Binary, opts.Args...)
	command.Stderr = opts.Stderr

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting the provider: %w", err)
	}

	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	session := &Session{command: command, stdin: stdin, lines: lines, wire: opts.Wire}

	features := make([]string, 0, len(opts.Features))
	for _, f := range opts.Features {
		features = append(features, string(f))
	}
	scope, err := raw(opts.Scope)
	if err != nil {
		session.Close()
		return nil, err
	}

	var answer ConfigureResult
	if err := session.call(MethodConfigure, ConfigureParams{
		Features: features, ExpectAccount: opts.ExpectAccount,
		Caller: opts.Caller, Run: opts.Run, Scope: scope,
	}, &answer, nil); err != nil {
		session.Close()
		return nil, fmt.Errorf("configuring the session: %w", err)
	}

	session.granted = answer.Granted
	session.account = answer.Account
	return session, nil
}

// Close stops the provider's process.
func (s *Session) Close() error {
	s.stdin.Close()
	return s.command.Wait()
}

func (s *Session) Account() string   { return s.account }
func (s *Session) Granted() []string { return s.granted }

// DescribeTypes reads what the provider declares, which is how a tool learns
// the delete order without knowing the cloud.
func (s *Session) DescribeTypes() ([]core.TypeInfo, error) {
	var answer TypesResult
	if err := s.call(MethodDescribeTypes, nil, &answer, nil); err != nil {
		return nil, err
	}
	return answer.Types, nil
}

// Audit reads back every create and delete this session made.
func (s *Session) Audit() ([]core.AuditEntry, error) {
	var answer AuditResult
	if err := s.call(MethodAudit, nil, &answer, nil); err != nil {
		return nil, err
	}
	return answer.Entries, nil
}

func (s *Session) DiscoverScopes() ([]string, error) {
	var answer ScopesResult
	if err := s.call(MethodDiscoverScopes, nil, &answer, nil); err != nil {
		return nil, err
	}
	return answer.Scopes, nil
}

// List reads the scope. Each resource arrives on its own message, so a caller
// that only wants a few never holds the rest.
func (s *Session) List(scope any, withFullData bool, each func(core.Resource) error) (core.Listing, error) {
	body, err := raw(scope)
	if err != nil {
		return core.Listing{}, err
	}

	var listing core.Listing
	err = s.call(MethodList, ListParams{Scope: body, WithFullData: withFullData}, &listing, func(one core.Resource) error {
		if each != nil {
			return each(one)
		}
		listing.Resources = append(listing.Resources, one)
		return nil
	})
	return listing, err
}

func (s *Session) Get(id cloudinv.ID, withFullData bool) (core.Resource, error) {
	var answer core.Resource
	err := s.call(MethodGet, GetParams{ID: string(id), WithFullData: withFullData}, &answer, nil)
	return answer, err
}

func (s *Session) Create(scope, spec any, withFullData bool) (core.Resource, error) {
	scopeBody, err := raw(scope)
	if err != nil {
		return core.Resource{}, err
	}
	specBody, err := raw(spec)
	if err != nil {
		return core.Resource{}, err
	}

	var answer core.Resource
	err = s.call(MethodCreate, CreateParams{Scope: scopeBody, Spec: specBody, WithFullData: withFullData}, &answer, nil)
	return answer, err
}

func (s *Session) Delete(id cloudinv.ID) error {
	return s.call(MethodDelete, DeleteParams{ID: string(id)}, nil, nil)
}

// call writes one request and reads until its reply, handing each streamed
// resource to stream on the way.
func (s *Session) call(method string, params any, into any, stream func(core.Resource) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.next++
	id := s.next

	body, err := raw(params)
	if err != nil {
		return err
	}
	request, err := json.Marshal(Request{JSONRPC: Version, ID: &id, Method: method, Params: body})
	if err != nil {
		return err
	}
	s.tap("sent", request)
	if _, err := s.stdin.Write(append(request, '\n')); err != nil {
		return fmt.Errorf("writing to the provider: %w", err)
	}

	for s.lines.Scan() {
		line := s.lines.Bytes()
		s.tap("received", line)

		// A message with no id is a streamed resource, not the reply.
		var notification Request
		if err := json.Unmarshal(line, &notification); err == nil && notification.ID == nil && notification.Method == MethodResource {
			if stream == nil {
				continue
			}
			var one core.Resource
			if err := json.Unmarshal(notification.Params, &one); err != nil {
				return err
			}
			if err := stream(one); err != nil {
				return err
			}
			continue
		}

		var response Response
		if err := json.Unmarshal(line, &response); err != nil {
			return fmt.Errorf("reading the provider's reply: %w", err)
		}
		if response.ID == nil || *response.ID != id {
			continue
		}
		if response.Error != nil {
			return response.Error
		}
		if into == nil || len(response.Result) == 0 {
			return nil
		}
		return json.Unmarshal(response.Result, into)
	}
	if err := s.lines.Err(); err != nil {
		return err
	}
	return fmt.Errorf("the provider stopped before answering %s", method)
}

func (s *Session) tap(direction string, line []byte) {
	if s.wire == nil {
		return
	}
	s.wire(direction, append([]byte(nil), line...))
}

func raw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("serialising %T: %w", v, err)
	}
	return body, nil
}
