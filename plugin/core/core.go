// Package core is the contract's logic with no transport in it: the session's
// features, the account it is bound to, and the five calls. A transport turns
// its types into bytes, which is what keeps the protocol replaceable.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	cloudinv "github.com/james00012/cloud-inventory"
)

// Feature is what a session may do. Read is the default; create and delete
// each have to be asked for.
type Feature string

const (
	Read   Feature = "read"
	Create Feature = "create"
	Delete Feature = "delete"
)

// Resource is what crosses the boundary: the common fields any tool filters
// on, and the cloud's own body only when it was asked for.
type Resource struct {
	ID          cloudinv.ID       `json:"id"`
	Type        string            `json:"type"`
	DisplayName string            `json:"displayName,omitempty"`
	CreatedAt   *time.Time        `json:"createdAt,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	FullData    json.RawMessage   `json:"fullData,omitempty"`
}

// Gap is a part of a scope the provider could not read.
type Gap struct {
	Scope   string `json:"scope"`
	Fault   string `json:"fault"`
	Message string `json:"message,omitempty"`
}

// Listing is what a list answers with, including what it could not read.
type Listing struct {
	Resources []Resource `json:"resources"`
	Gaps      []Gap      `json:"gaps,omitempty"`
	Asked     int        `json:"asked"`
}

// Spec is one resource type wired from the per-cloud library.
type Spec[T cloudinv.Resource, CreateSpec any, Scope any] struct {
	Lister  cloudinv.Lister[T, Scope]
	Getter  cloudinv.Getter[T]
	Deleter cloudinv.Deleter
	Creator cloudinv.Creator[T, CreateSpec, Scope]

	Account func(ctx context.Context) (string, error)
	Scopes  func(ctx context.Context) ([]string, error)
}

// Service holds a configured session. A call outside what Configure granted is
// refused here, before it reaches the cloud.
type Service[T cloudinv.Resource, CreateSpec any, Scope any] struct {
	spec       Spec[T, CreateSpec, Scope]
	granted    map[Feature]bool
	configured bool
}

func New[T cloudinv.Resource, CreateSpec any, Scope any](spec Spec[T, CreateSpec, Scope]) *Service[T, CreateSpec, Scope] {
	return &Service[T, CreateSpec, Scope]{spec: spec}
}

// Configure grants the session its features or refuses to start it.
func (s *Service[T, CreateSpec, Scope]) Configure(ctx context.Context, features []Feature, expectAccount string) ([]Feature, string, error) {
	account := ""
	if s.spec.Account != nil {
		found, err := s.spec.Account(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("reading which account these credentials belong to: %w", err)
		}
		account = found
	}
	if expectAccount != "" && expectAccount != account {
		return nil, "", fmt.Errorf("these credentials belong to %s, not the %s this session expects", account, expectAccount)
	}

	granted := map[Feature]bool{Read: true}
	for _, asked := range features {
		switch asked {
		case Read:
		case Create:
			if s.spec.Creator == nil {
				return nil, "", fmt.Errorf("this resource type cannot create, so the session cannot start")
			}
		case Delete:
			if s.spec.Deleter == nil {
				return nil, "", fmt.Errorf("this resource type cannot delete, so the session cannot start")
			}
		default:
			return nil, "", fmt.Errorf("unknown feature %q", asked)
		}
		granted[asked] = true
	}

	s.granted = granted
	s.configured = true

	var out []Feature
	for _, f := range []Feature{Read, Create, Delete} {
		if granted[f] {
			out = append(out, f)
		}
	}
	return out, account, nil
}

// NotEnabled is the error kind a refused call answers with, so a host can tell
// a refusal from a cloud failure.
type NotEnabled struct{ Feature Feature }

func (n *NotEnabled) Error() string { return fmt.Sprintf("this session was not granted %s", n.Feature) }

func (s *Service[T, CreateSpec, Scope]) allows(f Feature) error {
	if !s.configured {
		return fmt.Errorf("the session has not been configured")
	}
	if !s.granted[f] {
		return &NotEnabled{Feature: f}
	}
	return nil
}

func (s *Service[T, CreateSpec, Scope]) DiscoverScopes(ctx context.Context) ([]string, error) {
	if err := s.allows(Read); err != nil {
		return nil, err
	}
	if s.spec.Scopes == nil {
		return nil, nil
	}
	return s.spec.Scopes(ctx)
}

// List reads the scope and hands each resource to emit as it is read, which is
// what lets a transport stream rather than hold a whole listing.
func (s *Service[T, CreateSpec, Scope]) List(ctx context.Context, scopeJSON []byte, withFullData bool, emit func(Resource) error) (Listing, error) {
	if err := s.allows(Read); err != nil {
		return Listing{}, err
	}
	if s.spec.Lister == nil {
		return Listing{}, fmt.Errorf("this resource type cannot list")
	}

	var scope Scope
	if err := unmarshalScope(scopeJSON, &scope); err != nil {
		return Listing{}, err
	}

	found, incomplete, err := s.spec.Lister.List(ctx, scope)
	if err != nil && len(found) == 0 && !incomplete.Any() {
		return Listing{}, err
	}

	out := Listing{Asked: incomplete.Asked}
	for _, one := range found {
		carried, err := carry(one, withFullData)
		if err != nil {
			return Listing{}, err
		}
		if emit != nil {
			if err := emit(carried); err != nil {
				return Listing{}, err
			}
		} else {
			out.Resources = append(out.Resources, carried)
		}
	}
	for _, gap := range incomplete.Gaps {
		message := ""
		if gap.Err != nil {
			message = gap.Err.Error()
		}
		out.Gaps = append(out.Gaps, Gap{Scope: gap.Scope, Fault: gap.Fault.String(), Message: message})
	}
	return out, nil
}

func (s *Service[T, CreateSpec, Scope]) Get(ctx context.Context, id cloudinv.ID, withFullData bool) (Resource, error) {
	if err := s.allows(Read); err != nil {
		return Resource{}, err
	}
	if s.spec.Getter == nil {
		return Resource{}, fmt.Errorf("this resource type cannot get")
	}

	got, err := s.spec.Getter.Get(ctx, id)
	if err != nil {
		return Resource{}, err
	}
	return carry(got, withFullData)
}

func (s *Service[T, CreateSpec, Scope]) Create(ctx context.Context, scopeJSON, specJSON []byte, withFullData bool) (Resource, error) {
	if err := s.allows(Create); err != nil {
		return Resource{}, err
	}

	var scope Scope
	if err := unmarshalScope(scopeJSON, &scope); err != nil {
		return Resource{}, err
	}
	var wanted CreateSpec
	if err := json.Unmarshal(specJSON, &wanted); err != nil {
		return Resource{}, fmt.Errorf("reading the spec: %w", err)
	}

	made, err := s.spec.Creator.Create(ctx, scope, wanted)
	if err != nil {
		return Resource{}, err
	}
	return carry(made, withFullData)
}

func (s *Service[T, CreateSpec, Scope]) Delete(ctx context.Context, id cloudinv.ID) error {
	if err := s.allows(Delete); err != nil {
		return err
	}
	return s.spec.Deleter.Delete(ctx, id)
}

func carry[T cloudinv.Resource](one T, withFullData bool) (Resource, error) {
	out := Resource{
		ID:          one.ResourceID(),
		Type:        one.ResourceType(),
		DisplayName: one.DisplayName(),
		Tags:        one.Tags(),
	}
	if made := one.CreatedAt(); !made.IsZero() {
		out.CreatedAt = &made
	}
	if withFullData {
		body, err := json.Marshal(one)
		if err != nil {
			return Resource{}, fmt.Errorf("serialising %s: %w", one.ResourceID(), err)
		}
		out.FullData = body
	}
	return out, nil
}

func unmarshalScope(body []byte, into any) error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("reading the scope: %w", err)
	}
	return nil
}
