# The provider boundary

This module builds the architecture RFC-188 proposes, against the current
revision: one provider per cloud, run as its own process, reached over JSON-RPC
2.0 on standard input and output, with a session granted its features up front
and bound to one account, and an installer that refuses to run a provider it
cannot verify.

## What the contract carries

- **The provider declares its types**, what each offers, the fields a scope
  needs, and which must be deleted before which. A consumer plans the order in
  `plan`, which refuses a declaration that cannot be ordered.
- **A resource says what it sits inside**, and whether a delete cannot work and
  why. A provider reports only what the cloud's own record states rather than
  guessing, because hiding a resource from a cleanup is worse than trying and
  failing.
- **Everything a run creates carries the run's tag**, and a retried create
  answers with the resource the first one made rather than a second.
- **Every create and delete is audited** with the caller, the account, the run
  and the outcome, and a consumer reads the log back over the protocol.
- **The protocol has a JSON Schema**, and a test drives a whole session and
  validates every message in both directions against it, so the schema is the
  contract rather than a description of one.

## What the tests drive

A real subprocess, not a server in the same process.

- **Discover scopes, list, get, create, delete.** No update.
- **A listing streams.** Each resource arrives as its own notification before
  the reply, so neither side holds a whole listing in memory.
- **Features are granted at Configure or the session does not start.** A delete
  in a read-only session is refused with a `notEnabled` error before any API
  call, which the tests confirm by asserting the cloud was never reached.
- **A session is bound to one account** and refuses to start pointed at
  another.
- **A location that could not be read crosses as a gap**, carrying its fault
  kind and the number of locations attempted.
- **A shell script drives the protocol with jq** and no client library, which
  is what the move to JSON-RPC was for.

## What the boundary does to the provider's struct

The struct does not cross. Its serialisation does, and reading it back needs a
type that models the cloud.

| | Cloud Run | EC2 |
| --- | --- | --- |
| full data for one resource | 286 bytes | 1,594 bytes |
| fields on the wire | only those set | 61 |
| of those, not null | 6 | 16 |

Both decode, and `core`'s tests measure both on every run. The finding is what
a consumer must link to decode them: the cloud's SDK, or a generated type
modelling the same cloud. The gap between the two columns is `omitempty`.
Google's generated types declare it, so only the fields that were set cross.
The AWS SDK's types do not, so all 61 cross whether they were set or not.

A second cost found by building it: a spec carrying the cloud's own request
body as an embedded pointer loses an empty body across the wire, because it
marshals to nothing and arrives as nil. The provider refuses rather than
creating something the caller did not describe, but a caller cannot tell empty
from absent.

## Transport size

A provider serving nothing is 3.8 MB and 82 packages over JSON-RPC, against
18.2 MB and 359 over gRPC, which matches the RFC's estimate. Once a provider
links a cloud SDK the gap falls to a few percent, because the SDK dominates the
binary. The reason to prefer JSON-RPC is that a shell script can drive it, not
the size. The gRPC implementation was removed once the RFC settled the
question, so this repository no longer reproduces the comparison.

## Distribution and verification

The checks run in order and stop at the first failure: signature, then
checksum, then lock file, then a launch check against the shared cache. Every
failure names the check that refused, and no path warns and continues. A
development override skips all of it and says so.

The tests cover a release signed by an untrusted key, a missing signature, a
tampered archive that is still a valid archive, a rebuild published at the same
version, a cached build edited after install, and a lock file written on one
machine verifying on another.

Signatures are ed25519 rather than GPG, because the flow is what is being
tested and a keyring would only make the tests harder to run.

## What this does not build

No generation from cloud schemas, which lives in `../generic`. No Azure, no
generated IAM policy, no rate limiting, and no effects-as-handles. None of them
changes an answer above.

## The dependency cost

None, which was not true of the gRPC version. The contract module resolves 75
modules and this one 76, and the only addition is a JSON Schema validator that
no non-test file imports. A consumer that links the library in process pays
nothing for the protocol or the installer, and it is a separate module so that
this stays visible.
