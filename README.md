# cloud-inventory (prototype)

A throwaway prototype for the Gruntwork RFC *A shared cloud inventory library, and
how cloud-nuke and terratest adopt it*. It is **not** a Gruntwork repository and
**not** meant to be depended on. The module path will change if the design is
adopted.

It exists to settle one question with code rather than opinion: **can a plugin
boundary keep the provider's own SDK fields?** This prototype says no, and shows
what you get when the library stays in-process instead.

## What it holds

Four calls per resource type, generic over the resource's own type, the scope
shape for its provider, and the spec a caller supplies when creating one.

```go
type Lister[T any, S any]    interface { List(ctx context.Context, scope S) ([]T, Incomplete, error) }
type Getter[T any]           interface { Get(ctx context.Context, id ID) (T, error) }
type Deleter                 interface { Delete(ctx context.Context, id ID) error }
type Creator[T, Spec, S any] interface { Create(ctx context.Context, scope S, spec Spec) (T, error) }
```

Two resource types implement all four:

| Type | Provider | Scope shape |
| --- | --- | --- |
| `gcp.CloudRunServices` | GCP | project and locations |
| `aws.EC2Instances` | AWS | credentials and regions |

Both embed the provider's own struct and add only what the API does not give
directly, then satisfy a five-method contract so a consumer can filter across
providers without knowing either one:

```go
type Resource interface {
    ResourceID() ID
    ResourceType() string
    DisplayName() string
    CreatedAt() time.Time
    Tags() map[string]string
}
```

## The three things worth arguing with

1. **A list returns what it could not read, beside the results.** `Incomplete`
   carries the scopes that failed and how many were asked for, so one region of
   forty failing reads differently from the only region asked for failing. A
   destroyer treats an unread region as resources left running; a drift report
   treats it as a verdict it cannot give.
2. **The caller passes the regions to query.** Neither the Cloud Run v2 API nor
   EC2 lists across regions, so both call the shared `FanOut`. Narrowing is an
   input, not a filter applied to the answers.
3. **The provider's struct is embedded, not modelled.** This is the property a
   plugin boundary cannot keep: across a process boundary the struct has to be
   re-modelled as JSON, which is the modelling treadmill the embedding avoids.

## Why the contract is methods, not fields

An EC2 instance has no name field; its name is a tag. A Cloud Run service's name
is the last segment of its resource name. With the provider struct embedded,
there is no sensible shared `Name` field, so the shared shape has to be behavior.

## What is not here

No update call, because an update needs desired state and diffing, which
Terraform owns. No Azure, because nobody has a subscription to prove a delete
against. No live run against any cloud: every test drives a fake client or an
`httptest` server.
