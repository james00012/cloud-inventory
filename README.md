# cloud-inventory (prototype)

A throwaway prototype for Gruntwork RFC-188, *Common core for
Terratest/CloudNuke/Brownfield*. It is **not** a Gruntwork repository and
**not** meant to be depended on. The module path will change if the design is
adopted.

It exists to settle three of the RFC's claims with code rather than opinion:
whether resource types can be generated from a cloud's own schemas, what a
provider process costs, and whether one contract can serve both a test suite
and a destroyer.

## The contract

Four calls per resource type, generic over the resource's own type, the scope
its provider needs, and the spec a caller supplies when creating one.

```go
type Lister[T any, S any]    interface { List(ctx context.Context, scope S) ([]T, Incomplete, error) }
type Getter[T any]           interface { Get(ctx context.Context, id ID) (T, error) }
type Deleter                 interface { Delete(ctx context.Context, id ID) error }
type Creator[T, Spec, S any] interface { Create(ctx context.Context, scope S, spec Spec) (T, error) }
```

Resource types embed the provider's own struct and add only what the API does
not give directly, then satisfy a five-method contract so a consumer can filter
across providers without knowing either one:

```go
type Resource interface {
    ResourceID() ID
    ResourceType() string
    DisplayName() string
    CreatedAt() time.Time
    Tags() map[string]string
}
```

Two types are hand written, `gcp.CloudRunServices` and `aws.EC2Instances`, and
the rest are generated.

## The three findings worth arguing with

1. **A listing reports what it could not read, beside the results.**
   `Incomplete` carries the scopes that failed and how many were attempted, so
   one region of forty failing reads differently from the only region asked for
   failing. A destroyer treats an unread region as resources left running and
   carries on; a test suite treats the same report as a verdict it cannot give
   and fails. The library reports, and the consumer decides.
2. **The caller passes the scopes to query.** Neither the Cloud Run API nor EC2
   lists across regions, so both fan out through the same helper. Narrowing is
   an input, not a filter applied to the answers.
3. **The provider's struct does not cross a process boundary, only its
   serialisation.** Reading it back needs a type that models the cloud, which a
   consumer either links from the SDK or has generated. The five common fields
   need no such type, and that is the part of the contract that is genuinely
   provider-neutral.

## Where each claim is answered

| Question | Where |
| --- | --- |
| Can resource types be generated from a cloud's schemas? | [generic/README.md](generic/README.md) |
| What does a provider process cost, and what does it buy? | [plugin/README.md](plugin/README.md) |

## Why the contract is methods rather than fields

An EC2 instance has no name field; its name is a tag. A Cloud Run service's
name is the last segment of its resource name. With the provider's struct
embedded there is no sensible shared `Name` field, so the shared shape has to
be behavior.

## What is not here

No update call, because an update needs desired state and diffing, which
Terraform owns. No Azure, because nobody has a subscription to prove a delete
against. No generation for AWS, whose service models describe operations rather
than resources. No run against a live cloud: every test drives a narrow adapter
or an `httptest` server.
