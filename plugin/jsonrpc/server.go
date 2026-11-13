package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/james00012/cloud-inventory/plugin/core"
)

// Serving is what a transport needs from the contract, which is every call the
// protocol offers.
type Serving interface {
	Configure(ctx context.Context, features []core.Feature, session core.Session) ([]core.Feature, string, error)
	DescribeTypes(ctx context.Context) ([]core.TypeInfo, error)
	DiscoverScopes(ctx context.Context) ([]string, error)
	Audit(ctx context.Context) ([]core.AuditEntry, error)
	List(ctx context.Context, scope []byte, withFullData bool, emit func(core.Resource) error) (core.Listing, error)
	Get(ctx context.Context, id cloudinv.ID, withFullData bool) (core.Resource, error)
	Create(ctx context.Context, scope, spec []byte, withFullData bool) (core.Resource, error)
	Delete(ctx context.Context, id cloudinv.ID) error
}

// Server reads one request per line and writes one message per line.
type Server struct {
	service Serving

	mu  sync.Mutex
	out *bufio.Writer
}

func NewServer(service Serving) *Server { return &Server{service: service} }

// Serve reads until the input ends. It writes only protocol messages to out,
// so anything else a provider has to say goes to standard error.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriter(out)
	defer s.out.Flush()

	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for reader.Scan() {
		line := reader.Bytes()
		if len(line) == 0 {
			continue
		}

		var request Request
		if err := json.Unmarshal(line, &request); err != nil {
			s.write(Response{JSONRPC: Version, Error: &Error{Code: CodeParse, Message: err.Error()}})
			continue
		}
		if request.JSONRPC != Version {
			s.write(Response{JSONRPC: Version, ID: request.ID,
				Error: &Error{Code: CodeInvalidRequest, Message: "jsonrpc must be " + Version}})
			continue
		}

		result, err := s.dispatch(ctx, request)
		if request.ID == nil {
			// A notification expects no reply.
			continue
		}
		if err != nil {
			s.write(Response{JSONRPC: Version, ID: request.ID, Error: asError(err)})
			continue
		}
		body, err := json.Marshal(result)
		if err != nil {
			s.write(Response{JSONRPC: Version, ID: request.ID,
				Error: &Error{Code: CodeInternal, Message: err.Error()}})
			continue
		}
		s.write(Response{JSONRPC: Version, ID: request.ID, Result: body})
	}
	return reader.Err()
}

func (s *Server) dispatch(ctx context.Context, request Request) (any, error) {
	switch request.Method {
	case MethodConfigure:
		var params ConfigureParams
		if err := decode(request.Params, &params); err != nil {
			return nil, err
		}
		features := make([]core.Feature, 0, len(params.Features))
		for _, f := range params.Features {
			features = append(features, core.Feature(f))
		}
		granted, account, err := s.service.Configure(ctx, features, core.Session{
			Caller: params.Caller, Run: params.Run, ExpectAccount: params.ExpectAccount,
		})
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(granted))
		for _, f := range granted {
			names = append(names, string(f))
		}
		return ConfigureResult{Granted: names, Account: account}, nil

	case MethodDescribeTypes:
		found, err := s.service.DescribeTypes(ctx)
		if err != nil {
			return nil, err
		}
		return TypesResult{Types: found}, nil

	case MethodAudit:
		entries, err := s.service.Audit(ctx)
		if err != nil {
			return nil, err
		}
		return AuditResult{Entries: entries}, nil

	case MethodDiscoverScopes:
		found, err := s.service.DiscoverScopes(ctx)
		if err != nil {
			return nil, err
		}
		return ScopesResult{Scopes: found}, nil

	case MethodList:
		var params ListParams
		if err := decode(request.Params, &params); err != nil {
			return nil, err
		}
		// Each resource goes out as it is read, so neither side holds a whole
		// listing in memory.
		return s.service.List(ctx, params.Scope, params.WithFullData, func(one core.Resource) error {
			body, err := json.Marshal(one)
			if err != nil {
				return err
			}
			s.write(Request{JSONRPC: Version, Method: MethodResource, Params: body})
			return nil
		})

	case MethodGet:
		var params GetParams
		if err := decode(request.Params, &params); err != nil {
			return nil, err
		}
		return s.service.Get(ctx, cloudinv.ID(params.ID), params.WithFullData)

	case MethodCreate:
		var params CreateParams
		if err := decode(request.Params, &params); err != nil {
			return nil, err
		}
		return s.service.Create(ctx, params.Scope, params.Spec, params.WithFullData)

	case MethodDelete:
		var params DeleteParams
		if err := decode(request.Params, &params); err != nil {
			return nil, err
		}
		return struct{}{}, s.service.Delete(ctx, cloudinv.ID(params.ID))
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "no method " + request.Method}
}

func (s *Server) write(message any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, err := json.Marshal(message)
	if err != nil {
		return
	}
	s.out.Write(body)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func decode(params json.RawMessage, into any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, into); err != nil {
		return &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	return nil
}

func asError(err error) *Error {
	var protocol *Error
	if errors.As(err, &protocol) {
		return protocol
	}
	var refused *core.NotEnabled
	if errors.As(err, &refused) {
		return &Error{Code: CodeNotEnabled, Message: err.Error(), Kind: "notEnabled"}
	}
	return &Error{Code: CodeInternal, Message: fmt.Sprint(err)}
}
