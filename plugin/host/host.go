// Package host is what a tool links to reach a provider: the contract types and
// nothing of any cloud. Starting a session starts the provider's own process.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	cloudinv "github.com/james00012/cloud-inventory"
	pb "github.com/james00012/cloud-inventory/plugin/proto"
	"github.com/james00012/cloud-inventory/plugin/provider"
	"github.com/james00012/cloud-inventory/plugin/wire"
)

// Feature is what a session asks to be allowed to do.
type Feature = pb.Feature

const (
	Read   = pb.Feature_FEATURE_READ
	Create = pb.Feature_FEATURE_CREATE
	Delete = pb.Feature_FEATURE_DELETE
)

// Session is a configured provider process.
type Session struct {
	client  *goplugin.Client
	calls   pb.InventoryClient
	granted []Feature
	account string
}

// Options is what a session is started with.
type Options struct {
	// Binary is the provider to run.
	Binary string

	// Features the session asks for. Read is always granted; create and delete
	// have to be asked for, and a session that cannot get all of them fails.
	Features []Feature

	// ExpectAccount is the account the host believes it is pointed at. The
	// provider refuses the session on a mismatch.
	ExpectAccount string

	// Scope is the provider's own scope value, carried as its JSON.
	Scope any
}

// Start runs the provider and configures the session, or fails without one.
func Start(ctx context.Context, opts Options) (*Session, error) {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  wire.Handshake,
		Plugins:          goplugin.PluginSet{wire.Name: &provider.GRPCPlugin{}},
		Cmd:              exec.Command(opts.Binary),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
	})

	connection, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("starting the provider: %w", err)
	}
	raw, err := connection.Dispense(wire.Name)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("reaching the provider: %w", err)
	}

	scope, err := asJSON(opts.Scope)
	if err != nil {
		client.Kill()
		return nil, err
	}

	calls := raw.(pb.InventoryClient)
	answer, err := calls.Configure(ctx, &pb.ConfigureRequest{
		Features:      opts.Features,
		ExpectAccount: opts.ExpectAccount,
		ScopeJson:     scope,
	})
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("configuring the session: %w", err)
	}

	return &Session{client: client, calls: calls, granted: answer.GetGranted(), account: answer.GetAccount()}, nil
}

// Close stops the provider's process.
func (s *Session) Close() { s.client.Kill() }

// Account is the account the provider's credentials belong to.
func (s *Session) Account() string { return s.account }

// Granted is what this session may do.
func (s *Session) Granted() []Feature { return s.granted }

// Resource is what a host receives. The five common fields are typed; the
// cloud's own body arrives only when asked for, and is read through a view.
type Resource struct {
	ID          cloudinv.ID
	Type        string
	DisplayName string
	CreatedAt   time.Time
	Tags        map[string]string

	// FullData is the provider's own body for this resource, or empty.
	FullData []byte
}

// View decodes the cloud's own body into a type that models it. A host that
// wants the cloud's fields supplies that type: the provider's struct cannot
// cross the boundary, only its serialisation can.
func (r Resource) View(into any) error {
	if len(r.FullData) == 0 {
		return fmt.Errorf("%s carries no full data: ask for it on the call", r.ID)
	}
	return json.Unmarshal(r.FullData, into)
}

// DiscoverScopes asks the provider which parts of the cloud it can be pointed at.
func (s *Session) DiscoverScopes(ctx context.Context) ([]string, error) {
	answer, err := s.calls.DiscoverScopes(ctx, &pb.DiscoverScopesRequest{})
	if err != nil {
		return nil, err
	}
	return answer.GetScopes(), nil
}

// List reads every resource in the scope, and reports the parts it could not
// read rather than leaving them out.
func (s *Session) List(ctx context.Context, scope any, withFullData bool) ([]Resource, cloudinv.Incomplete, error) {
	body, err := asJSON(scope)
	if err != nil {
		return nil, cloudinv.Incomplete{}, err
	}

	answer, err := s.calls.List(ctx, &pb.ListRequest{ScopeJson: body, WithFullData: withFullData})
	if err != nil {
		return nil, cloudinv.Incomplete{}, err
	}

	found := make([]Resource, 0, len(answer.GetResources()))
	for _, one := range answer.GetResources() {
		found = append(found, received(one))
	}

	incomplete := cloudinv.Incomplete{Asked: int(answer.GetAsked())}
	for _, gap := range answer.GetGaps() {
		incomplete.Gaps = append(incomplete.Gaps, cloudinv.Gap{
			Scope: gap.GetScope(),
			Fault: faultFrom(gap.GetFault()),
			Err:   errors.New(gap.GetMessage()),
		})
	}
	return found, incomplete, nil
}

// Get reads one resource by id.
func (s *Session) Get(ctx context.Context, id cloudinv.ID, withFullData bool) (Resource, error) {
	answer, err := s.calls.Get(ctx, &pb.GetRequest{Id: string(id), WithFullData: withFullData})
	if err != nil {
		return Resource{}, err
	}
	return received(answer.GetResource()), nil
}

// Create makes one resource from the provider's own spec.
func (s *Session) Create(ctx context.Context, scope any, spec any, withFullData bool) (Resource, error) {
	scopeBody, err := asJSON(scope)
	if err != nil {
		return Resource{}, err
	}
	specBody, err := asJSON(spec)
	if err != nil {
		return Resource{}, err
	}

	answer, err := s.calls.Create(ctx, &pb.CreateRequest{
		ScopeJson: scopeBody, SpecJson: specBody, WithFullData: withFullData,
	})
	if err != nil {
		return Resource{}, err
	}
	return received(answer.GetResource()), nil
}

// Delete removes one resource by id.
func (s *Session) Delete(ctx context.Context, id cloudinv.ID) error {
	_, err := s.calls.Delete(ctx, &pb.DeleteRequest{Id: string(id)})
	return err
}

func received(one *pb.Resource) Resource {
	got := Resource{
		ID:          cloudinv.ID(one.GetId()),
		Type:        one.GetType(),
		DisplayName: one.GetDisplayName(),
		Tags:        one.GetTags(),
		FullData:    one.GetFullData(),
	}
	if made := one.GetCreatedAt(); made != nil {
		got.CreatedAt = made.AsTime()
	}
	return got
}

func asJSON(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("serialising %T: %w", v, err)
	}
	return string(body), nil
}

func faultFrom(f pb.Fault) cloudinv.Fault {
	switch f {
	case pb.Fault_FAULT_THROTTLE:
		return cloudinv.FaultThrottle
	case pb.Fault_FAULT_API_DISABLED:
		return cloudinv.FaultAPIDisabled
	case pb.Fault_FAULT_DENIED:
		return cloudinv.FaultDenied
	case pb.Fault_FAULT_NOT_FOUND:
		return cloudinv.FaultNotFound
	}
	return cloudinv.FaultUnknown
}
