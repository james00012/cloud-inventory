# The plugin boundary, built both ways

This module builds the architecture RFC-188 proposes, against the current
revision: one provider per cloud, run as its own process, reached over JSON-RPC
on standard input and output, with a session granted features up front and
bound to one account, and an installer that refuses to run a provider it cannot
verify.

The earlier gRPC version is kept beside it, since the RFC now records gRPC as
the alternative that was prototyped and not chosen. Keeping both is what made
the comparison below measurable.

## What runs

The tests drive a real subprocess, not a server in the same process.

- **Discover scopes, list, get, create, delete.** No update.
- **A list streams.** Each resource arrives on its own message before the
  reply, so neither side holds a whole listing.
- **Features are granted at Configure or the session does not start.** A delete
  in a read-only session is refused with a `notEnabled` kind before any API
  call, which the tests check by asserting the cloud was never asked.
- **A session is bound to one account**, and refuses to start pointed at
  another.
- **A location that could not be read crosses as a gap**, with its fault kind
  and the number of locations attempted.
- **A shell script drives it with jq**, with no client library, which is what
  the protocol was changed for.

## The transport, measured

The RFC says the gRPC provider binary was about five times larger. That holds,
but only where the transport is what you are weighing.

| | JSON-RPC | gRPC |
| --- | --- | --- |
| a provider serving nothing | 3.8 MB, 82 packages | 18 MB, 359 packages |
| the real Cloud Run provider | 23 MB, 440 packages | 24 MB, 477 packages |

So the transport costs 4.7 times as much in isolation and about 4% once the
provider links a cloud SDK, which it must. The reason to prefer JSON-RPC is the
shell script, not the size.

## Distribution and verification

The checks run in order and stop at the first failure: signature, then
checksum, then lock file, then a launch check against the shared cache. Every
failure names the check that refused, and there is no path that warns and
continues. A development override skips all of it and says so.

The tests cover a release signed by an untrusted key, a missing signature, a
tampered zip that is still a valid zip, a rebuild published at the same version,
a cached build edited after install, and a lock file written on one machine
verifying on another.

Signatures here are ed25519 rather than GPG, because the flow is what is being
tested and a keyring would only make the tests harder to run.

## What the boundary does to the provider's struct

The struct does not cross. Its serialisation does, and a host needs a type that
models the cloud to read it back.

| | Cloud Run | EC2 |
| --- | --- | --- |
| full data for one resource | 287 bytes | 1,594 bytes |
| fields on the wire | only those set | 61 |
| of those, not null | 6 | 16 |

Both decode. The finding is what a host must link to decode them: the cloud's
SDK, or a generated type modelling the same cloud.

One cost found by building it: a spec that carries the cloud's own request body
as an embedded pointer loses an empty body across the wire, since it marshals to
nothing and arrives as nil. The provider refuses rather than creating something
the caller did not describe, but a caller cannot tell empty from absent.

## What this does not build

No codegen from cloud schemas in this module, which lives in `../generic` for
Google. No Azure, no audit log, no generated IAM policy, no rate limiting, and
no effects-as-handles. None of them changes an answer above.

## The dependency cost

Adding this layer to the contract module took it from 75 requirements to 92, so
it is a separate module: a consumer that links the library in process pays
nothing for gRPC, go-plugin or STS.
