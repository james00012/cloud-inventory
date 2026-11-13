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

	// Parents are the resources this one sits inside, so a tool can group a
	// listing without knowing the cloud's own naming.
	Parents []cloudinv.ID `json:"parents,omitempty"`

	// Deletable is false where the provider already knows a delete would fail,
	// and NotDeletable says why, so a tool reports rather than tries.
	Deletable    bool   `json:"deletable"`
	NotDeletable string `json:"notDeletable,omitempty"`

	FullData json.RawMessage `json:"fullData,omitempty"`
}

// Parented is the optional interface a resource type implements where the
// cloud says what a resource sits inside.
type Parented interface {
	Parents() []cloudinv.ID
}

// Deletability is the optional interface a resource type implements where the
// cloud says a resource cannot be deleted as it stands.
type Deletability interface {
	Deletable() (bool, string)
}

// Taggable is the optional interface a create spec implements so the provider
// can put the run's tag on what it makes.
type Taggable interface {
	AddTag(key, value string)
}

// TypeInfo is what a provider declares about one resource type, so a tool can
// plan a run without knowing the cloud.
type TypeInfo struct {
	Type string `json:"type"`

	// Operations the type offers, out of list, get, create and delete.
	Operations []string `json:"operations"`

	// ScopeFields are what a list must be given, which is the shape of a scope.
	ScopeFields []string `json:"scopeFields,omitempty"`

	// DeleteBefore names the types this one must be deleted before. A subnet
	// is deleted before its network. The provider declares it; the tool plans
	// the order and runs it.
	DeleteBefore []string `json:"deleteBefore,omitempty"`
}

// AuditEntry records one create or delete and how it ended. A host reads the
// log back over the protocol, so the record does not only live in the
// provider's own output.
type AuditEntry struct {
	At       time.Time   `json:"at"`
	Caller   string      `json:"caller"`
	Action   string      `json:"action"`
	Resource cloudinv.ID `json:"resource"`
	Account  string      `json:"account"`
	Run      string      `json:"run,omitempty"`
	Outcome  string      `json:"outcome"`
	Error    string      `json:"error,omitempty"`
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

	// Declares what this provider offers, which a tool reads to plan a run.
	Declares []TypeInfo

	// Identify says which resource a create would make, where the provider can
	// tell from the scope and the spec. It is what makes a retried create
	// answer with the resource the first one made instead of a second one.
	Identify func(scope Scope, spec CreateSpec) (cloudinv.ID, bool)

	// RunTagKey is the tag every created resource carries, so a cleanup can
	// always find what a run made.
	RunTagKey string
}

// Service holds a configured session. A call outside what Configure granted is
// refused here, before it reaches the cloud.
type Service[T cloudinv.Resource, CreateSpec any, Scope any] struct {
	spec       Spec[T, CreateSpec, Scope]
	granted    map[Feature]bool
	configured bool

	caller  string
	account string
	run     string
	audit   []AuditEntry
}

func New[T cloudinv.Resource, CreateSpec any, Scope any](spec Spec[T, CreateSpec, Scope]) *Service[T, CreateSpec, Scope] {
	return &Service[T, CreateSpec, Scope]{spec: spec}
}

// Configure grants the session its features or refuses to start it.
// Session is what a host says about itself at Configure, beyond the features.
type Session struct {
	// Caller names the tool, which every audit entry records.
	Caller string

	// Run identifies this run. Everything created carries it as a tag, so a
	// cleanup can always find what the run made.
	Run string

	// ExpectAccount is the account the host believes it is pointed at.
	ExpectAccount string
}

func (s *Service[T, CreateSpec, Scope]) Configure(ctx context.Context, features []Feature, session Session) ([]Feature, string, error) {
	expectAccount := session.ExpectAccount
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
	s.caller = session.Caller
	s.account = account
	s.run = session.Run

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

	// Everything a run creates carries the run's tag, so a cleanup can find
	// what it made even where the cloud reports no creation time.
	if key, value := s.RunTag(); key != "" {
		if taggable, ok := any(&wanted).(Taggable); ok {
			taggable.AddTag(key, value)
		}
	}

	// A retried create answers with the resource the first one made, rather
	// than making a second, where the provider can say what it would make.
	if s.spec.Identify != nil && s.spec.Getter != nil {
		if id, ok := s.spec.Identify(scope, wanted); ok {
			if existing, err := s.spec.Getter.Get(ctx, id); err == nil {
				carried, err := carry(existing, withFullData)
				if err == nil {
					s.record("create", id, "already existed", nil)
				}
				return carried, err
			}
		}
	}

	made, err := s.spec.Creator.Create(ctx, scope, wanted)
	if err != nil {
		s.record("create", "", "failed", err)
		return Resource{}, err
	}
	s.record("create", made.ResourceID(), "created", nil)
	return carry(made, withFullData)
}

// DescribeTypes is what this provider declares: its resource types, what each
// offers, and which must be deleted before which. A tool reads it to plan a
// run without knowing the cloud.
func (s *Service[T, CreateSpec, Scope]) DescribeTypes(ctx context.Context) ([]TypeInfo, error) {
	if err := s.allows(Read); err != nil {
		return nil, err
	}
	return s.spec.Declares, nil
}

// Audit is every create and delete this session made, which is how a host
// reads the log back rather than only finding it in the provider's output.
func (s *Service[T, CreateSpec, Scope]) Audit(ctx context.Context) ([]AuditEntry, error) {
	if err := s.allows(Read); err != nil {
		return nil, err
	}
	return append([]AuditEntry(nil), s.audit...), nil
}

// RunTag is the tag a create adds, or empty where the session named no run.
func (s *Service[T, CreateSpec, Scope]) RunTag() (string, string) {
	if s.run == "" || s.spec.RunTagKey == "" {
		return "", ""
	}
	return s.spec.RunTagKey, s.run
}

func (s *Service[T, CreateSpec, Scope]) record(action string, id cloudinv.ID, outcome string, err error) {
	entry := AuditEntry{
		At: time.Now().UTC(), Caller: s.caller, Action: action,
		Resource: id, Account: s.account, Run: s.run, Outcome: outcome,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	s.audit = append(s.audit, entry)
}

func (s *Service[T, CreateSpec, Scope]) Delete(ctx context.Context, id cloudinv.ID) error {
	if err := s.allows(Delete); err != nil {
		return err
	}

	err := s.spec.Deleter.Delete(ctx, id)
	if err != nil {
		s.record("delete", id, "failed", err)
		return err
	}
	s.record("delete", id, "deleted", nil)
	return nil
}

func carry[T cloudinv.Resource](one T, withFullData bool) (Resource, error) {
	out := Resource{
		ID:          one.ResourceID(),
		Type:        one.ResourceType(),
		DisplayName: one.DisplayName(),
		Tags:        one.Tags(),

		// Deletable unless the cloud's own record says otherwise. A provider
		// never guesses: a resource it cannot judge is offered for deletion
		// and the attempt reports, because hiding one from a cleanup is worse
		// than trying and failing.
		Deletable: true,
	}
	if parented, ok := any(one).(Parented); ok {
		out.Parents = parented.Parents()
	}
	if judged, ok := any(one).(Deletability); ok {
		if can, why := judged.Deletable(); !can {
			out.Deletable = false
			out.NotDeletable = why
		}
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
