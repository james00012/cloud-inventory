// Package cloudinv holds the mechanism of talking to a cloud: how to create,
// list, get and delete one resource type, in plain Go values. It holds no
// policy. Which resources to act on, in what order, with what parallelism and
// what to report are decisions its consumers make.
package cloudinv

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ID is a resource's canonical identifier, stable enough to match against the
// address the same resource has in IaC state.
type ID string

// Fault says why a read or a write did not happen, in the few shapes every
// provider has. A consumer decides what each one costs: cloud-nuke backs off a
// throttle and skips a switched-off API, a drift report says it could not read.
type Fault int

const (
	FaultUnknown Fault = iota
	FaultThrottle
	FaultAPIDisabled
	FaultDenied
	FaultNotFound
)

func (f Fault) String() string {
	switch f {
	case FaultThrottle:
		return "throttled"
	case FaultAPIDisabled:
		return "api disabled"
	case FaultDenied:
		return "permission denied"
	case FaultNotFound:
		return "not found"
	}
	return "unknown"
}

// Gap is one part of a scope that could not be read.
type Gap struct {
	// Scope names the part, such as a region or an account.
	Scope string
	Fault Fault
	Err   error
}

// Incomplete is what a list could not read. It is returned beside the results
// rather than instead of them, because a list that reached nine regions of ten
// is useful and a list that silently reported nine is not: for a tool that
// deletes, the missing region holds live resources, and for one that compares
// against IaC it holds phantom drift.
type Incomplete struct {
	Gaps []Gap

	// Asked is how many parts of the scope the list tried to read. A consumer
	// needs it to tell one region of forty failing from the only region it
	// asked for failing, which are the same Gaps and very different news.
	Asked int
}

func (i Incomplete) Any() bool { return len(i.Gaps) > 0 }

// Total reports whether nothing was read at all, which no consumer should
// present as an empty cloud.
func (i Incomplete) Total() bool { return i.Asked > 0 && len(i.Gaps) == i.Asked }

func (i Incomplete) String() string {
	parts := make([]string, 0, len(i.Gaps))
	for _, gap := range i.Gaps {
		parts = append(parts, fmt.Sprintf("%s (%s)", gap.Scope, gap.Fault))
	}
	return strings.Join(parts, ", ")
}

// Resource is what every resource type answers with, whatever provider it comes
// from. A type satisfies it by embedding the provider's own struct and adding
// these few methods, so a consumer filtering or reporting across providers uses
// the methods while a consumer that knows the provider reads its struct
// directly.
type Resource interface {
	// ResourceID is the canonical identifier, which a delete takes, an
	// assertion reads and a drift report compares.
	ResourceID() ID

	// ResourceType is the name this type goes by on a command line.
	ResourceType() string

	// DisplayName is the name a person recognizes, which is a tag on AWS and
	// part of the resource name on GCP.
	DisplayName() string

	// CreatedAt is when the provider says the resource was made, zero where it
	// does not say.
	CreatedAt() time.Time

	// Tags are the provider's labels or tags, flattened.
	Tags() map[string]string
}

// Lister reads every resource of its type in the scope. It fans out over the
// parts of a scope the provider makes it ask separately, and reports the parts
// it could not read in Incomplete rather than dropping them.
type Lister[T any, S any] interface {
	List(ctx context.Context, scope S) ([]T, Incomplete, error)
}

// Getter reads one resource by id, which is what an assertion needs.
type Getter[T any] interface {
	Get(ctx context.Context, id ID) (T, error)
}

// Deleter deletes one resource by id and does not return until it is gone.
// Deleting one that is already gone is not an error.
type Deleter interface {
	Delete(ctx context.Context, id ID) error
}

// Creator makes one resource from a spec that carries the provider's own
// request body, so every field the API accepts stays reachable.
type Creator[T any, Spec any, S any] interface {
	Create(ctx context.Context, scope S, spec Spec) (T, error)
}

// FanOut reads every part of a scope the provider makes a caller ask
// separately, such as a region or an account, and collects what it could not
// read instead of failing the whole list. Every provider has scopes of this
// shape, so this loop and its partial-failure handling live here once rather
// than in each resource type, let alone in each consumer.
func FanOut[T any](ctx context.Context, parts []string, fault func(error) Fault, read func(context.Context, string) ([]T, error)) ([]T, Incomplete, error) {
	incomplete := Incomplete{Asked: len(parts)}

	var found []T
	for _, part := range parts {
		// A cancelled caller stops asking rather than working through the rest
		// of the list it had planned.
		if err := ctx.Err(); err != nil {
			return nil, incomplete, err
		}

		answer, err := read(ctx, part)
		if err != nil {
			incomplete.Gaps = append(incomplete.Gaps, Gap{Scope: part, Fault: fault(err), Err: err})
			continue
		}
		found = append(found, answer...)
	}

	return found, incomplete, nil
}
