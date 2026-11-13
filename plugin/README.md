# The plugin boundary, built

This module builds the architecture RFC-188 proposes: one provider per cloud,
run as its own process, reached over gRPC through a versioned protobuf contract,
with a session that is granted features up front and bound to one account.

It serves the same resource types the in-process library already has, so the two
designs can be compared on the same code rather than on expectations.

## What runs

A host starts the provider binary and makes every call in the contract across
the boundary. The tests drive a real subprocess, not a server in the same
process, against a stand-in for Cloud Run.

- **Discover scopes, list, get, create, delete.** No update, because moving a
  resource toward a desired state is OpenTofu's job.
- **Features are granted at Configure or the session does not start.** Read is
  the default. A delete in a read-only session is refused before any API call,
  which the tests check by asserting the cloud was never asked.
- **A session is bound to one account.** Pointed at a different one, it refuses
  to start rather than starting and failing later.
- **A part of a scope that could not be read crosses as a gap**, with its fault
  kind and the number of parts attempted, so a host can tell one failed region
  of two from the only region asked for failing.

## What it costs, measured

The provider's own struct cannot cross a process boundary. Its serialisation
can, and a host needs a type that models the cloud to read it back.

| | Cloud Run | EC2 |
| --- | --- | --- |
| full data for one resource | 287 bytes | 1,594 bytes |
| fields on the wire | only those set | 61 |
| of those, not null | 6 | 16 |

Google's generated clients carry JSON tags with `omitempty`, so a service
round-trips into the SDK's own struct and costs only what was set. The AWS SDK's
types carry no JSON tags at all, because they were never meant to be serialised
this way, so the whole struct travels and two thirds of it is nulls.

Both decode. The point is what the host must link to decode them: **the SDK, or
a generated type that models the same cloud.** A host that links neither gets a
`map[string]any` and every field access becomes a string lookup. So a plugin
boundary does not let a consumer have the provider's fields and no dependency on
a type that models that provider. It lets it choose which of the two it takes.

## What this does not build

No codegen from cloud schemas, no Azure, no audit log, no generated IAM policy
and no streaming. Those are in the RFC and none of them changes the answer
above, so building them here would only make this larger.

## The dependency cost, also measured

Adding this layer to the contract module took it from 75 to 92 modules and
raised the floor on `smithy-go` past the version terratest pins. It is a
separate module for that reason: a consumer that links the library in process
pays nothing for gRPC, go-plugin or STS.

| module | what links it | modules required |
| --- | --- | --- |
| `cloud-inventory` | an in-process consumer | 75 |
| `cloud-inventory/plugin` | a host that starts providers | 93 |
