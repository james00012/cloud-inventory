// Package provider serves one resource type over the plugin contract. It is
// generic over the library's call interfaces, so a cloud's binary is a few
// lines naming its types rather than a server written per resource.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	cloudinv "github.com/james00012/cloud-inventory"
	pb "github.com/james00012/cloud-inventory/plugin/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Resource is one cloud resource type, wired from the per-cloud library. Only
// the calls a type supports need be set.
type Resource[T cloudinv.Resource, Spec any, S any] struct {
	Lister  cloudinv.Lister[T, S]
	Getter  cloudinv.Getter[T]
	Deleter cloudinv.Deleter
	Creator cloudinv.Creator[T, Spec, S]

	// Account answers which account the provider's credentials belong to, so
	// Configure can refuse a session pointed at a different one.
	Account func(ctx context.Context) (string, error)

	// Scopes answers the parts of this cloud a caller may ask for.
	Scopes func(ctx context.Context) ([]string, error)

	// Fault sorts a cloud error into the kinds the contract names.
	Fault func(error) cloudinv.Fault
}

// server holds what Configure granted. A call outside the granted set is
// refused here, before it reaches the cloud.
type server[T cloudinv.Resource, Spec any, S any] struct {
	pb.UnimplementedInventoryServer

	resource   Resource[T, Spec, S]
	granted    map[pb.Feature]bool
	configured bool
}

func (s *server[T, Spec, S]) Configure(ctx context.Context, in *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	account := ""
	if s.resource.Account != nil {
		found, err := s.resource.Account(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading which account these credentials belong to: %w", err)
		}
		account = found
	}

	// Bound to one account: a session pointed at a different one does not
	// start, rather than starting and refusing later.
	if want := in.GetExpectAccount(); want != "" && want != account {
		return nil, fmt.Errorf("these credentials belong to %s, not the %s this session expects", account, want)
	}

	// Granted in full or refused, so a host never believes it has more than it
	// does. Read is the default and is always granted.
	granted := map[pb.Feature]bool{pb.Feature_FEATURE_READ: true}
	for _, asked := range in.GetFeatures() {
		switch asked {
		case pb.Feature_FEATURE_READ:
		case pb.Feature_FEATURE_CREATE:
			if s.resource.Creator == nil {
				return nil, fmt.Errorf("this resource type cannot create, so the session cannot start")
			}
		case pb.Feature_FEATURE_DELETE:
			if s.resource.Deleter == nil {
				return nil, fmt.Errorf("this resource type cannot delete, so the session cannot start")
			}
		default:
			return nil, fmt.Errorf("unknown feature %s", asked)
		}
		granted[asked] = true
	}

	s.granted = granted
	s.configured = true

	answer := &pb.ConfigureResponse{Account: account}
	for feature := range granted {
		answer.Granted = append(answer.Granted, feature)
	}
	return answer, nil
}

// allows refuses a call the session was not granted, before any cloud call.
func (s *server[T, Spec, S]) allows(feature pb.Feature) error {
	if !s.configured {
		return fmt.Errorf("the session has not been configured")
	}
	if !s.granted[feature] {
		return fmt.Errorf("this session was not granted %s", feature)
	}
	return nil
}

func (s *server[T, Spec, S]) DiscoverScopes(ctx context.Context, _ *pb.DiscoverScopesRequest) (*pb.DiscoverScopesResponse, error) {
	if err := s.allows(pb.Feature_FEATURE_READ); err != nil {
		return nil, err
	}
	if s.resource.Scopes == nil {
		return &pb.DiscoverScopesResponse{}, nil
	}

	found, err := s.resource.Scopes(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.DiscoverScopesResponse{Scopes: found}, nil
}

func (s *server[T, Spec, S]) List(ctx context.Context, in *pb.ListRequest) (*pb.ListResponse, error) {
	if err := s.allows(pb.Feature_FEATURE_READ); err != nil {
		return nil, err
	}
	if s.resource.Lister == nil {
		return nil, fmt.Errorf("this resource type cannot list")
	}

	var scope S
	if err := json.Unmarshal([]byte(in.GetScopeJson()), &scope); err != nil {
		return nil, fmt.Errorf("reading the scope: %w", err)
	}

	found, incomplete, err := s.resource.Lister.List(ctx, scope)
	if err != nil && len(found) == 0 && !incomplete.Any() {
		return nil, err
	}

	answer := &pb.ListResponse{Asked: int32(incomplete.Asked)}
	for _, one := range found {
		carried, err := carry(one, in.GetWithFullData())
		if err != nil {
			return nil, err
		}
		answer.Resources = append(answer.Resources, carried)
	}
	// A part that could not be read travels as a gap, never as an absence.
	for _, gap := range incomplete.Gaps {
		message := ""
		if gap.Err != nil {
			message = gap.Err.Error()
		}
		answer.Gaps = append(answer.Gaps, &pb.Gap{
			Scope: gap.Scope, Fault: faultTo(gap.Fault), Message: message,
		})
	}
	return answer, nil
}

func (s *server[T, Spec, S]) Get(ctx context.Context, in *pb.GetRequest) (*pb.GetResponse, error) {
	if err := s.allows(pb.Feature_FEATURE_READ); err != nil {
		return nil, err
	}
	if s.resource.Getter == nil {
		return nil, fmt.Errorf("this resource type cannot get")
	}

	got, err := s.resource.Getter.Get(ctx, cloudinv.ID(in.GetId()))
	if err != nil {
		return nil, err
	}
	carried, err := carry(got, in.GetWithFullData())
	if err != nil {
		return nil, err
	}
	return &pb.GetResponse{Resource: carried}, nil
}

func (s *server[T, Spec, S]) Create(ctx context.Context, in *pb.CreateRequest) (*pb.CreateResponse, error) {
	if err := s.allows(pb.Feature_FEATURE_CREATE); err != nil {
		return nil, err
	}

	var scope S
	if err := json.Unmarshal([]byte(in.GetScopeJson()), &scope); err != nil {
		return nil, fmt.Errorf("reading the scope: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal([]byte(in.GetSpecJson()), &spec); err != nil {
		return nil, fmt.Errorf("reading the spec: %w", err)
	}

	made, err := s.resource.Creator.Create(ctx, scope, spec)
	if err != nil {
		return nil, err
	}
	carried, err := carry(made, in.GetWithFullData())
	if err != nil {
		return nil, err
	}
	return &pb.CreateResponse{Resource: carried}, nil
}

func (s *server[T, Spec, S]) Delete(ctx context.Context, in *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	if err := s.allows(pb.Feature_FEATURE_DELETE); err != nil {
		return nil, err
	}

	if err := s.resource.Deleter.Delete(ctx, cloudinv.ID(in.GetId())); err != nil {
		return nil, err
	}
	return &pb.DeleteResponse{}, nil
}

// carry turns the library's own type into what crosses the wire: the five
// common fields, and the cloud's own body only when the host asked for it.
func carry[T cloudinv.Resource](one T, withFullData bool) (*pb.Resource, error) {
	out := &pb.Resource{
		Id:          string(one.ResourceID()),
		Type:        one.ResourceType(),
		DisplayName: one.DisplayName(),
		Tags:        one.Tags(),
	}
	if made := one.CreatedAt(); !made.IsZero() {
		out.CreatedAt = timestamppb.New(made)
	}
	if withFullData {
		body, err := json.Marshal(one)
		if err != nil {
			return nil, fmt.Errorf("serialising %s: %w", one.ResourceID(), err)
		}
		out.FullData = body
	}
	return out, nil
}

func faultTo(f cloudinv.Fault) pb.Fault {
	switch f {
	case cloudinv.FaultThrottle:
		return pb.Fault_FAULT_THROTTLE
	case cloudinv.FaultAPIDisabled:
		return pb.Fault_FAULT_API_DISABLED
	case cloudinv.FaultDenied:
		return pb.Fault_FAULT_DENIED
	case cloudinv.FaultNotFound:
		return pb.Fault_FAULT_NOT_FOUND
	}
	return pb.Fault_FAULT_UNKNOWN
}
