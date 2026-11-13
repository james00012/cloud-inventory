# Generation from cloud schemas, tried

RFC-188's central claim is that coverage should grow when a cloud publishes a
new resource type rather than when somebody writes code for it, through one
generic implementation per cloud driven by the cloud's own schemas.

This is that claim, for Google: a generator that reads a Discovery document,
and one client that serves any resource type the generator described.

## What came out

Across eight Google APIs, every resource type offering list, get and delete
produced a complete descriptor with **no per-type mapping**, meaning no file
written for that type. A bucket's descriptor names its paths, its collection
field, its identifier, its creation timestamp, its label map and the project
parameter its listing requires, all resolved from the document.

| | |
| --- | --- |
| resource types examined | 137 |
| described by the generator | 137 |
| driven end to end against a stand-in | 2 |

The two driven are Cloud Run services and Compute instances, chosen because
they address resources in opposite ways. Both run through the same client with
create, list, get, delete and the contract's five methods, from a descriptor
nobody edited.

## What the document does and does not settle

Mechanical, and resolved every time:

- **Paths, pagination and the collection field in the list response.**
- **The identifier, for all 137 types.** A newer API's get takes one
  self-contained resource path, so the resource name is the identifier. An
  older API's get takes several path parameters, so it is not, and the
  `selfLink` those resources carry is used instead. 18% are the first kind and
  82% the second.
- **The scope a listing requires**, read from the list method's required
  parameters and split by whether the API takes each in the path or the query
  string. Cloud Storage takes the project as a query parameter, and a generator
  that reads only path parameters produces a descriptor that would list every
  project the caller can see.
- **A creation timestamp for 88% and a label map for 29%.** A type with neither
  answers the contract with a zero value, the way an EC2 instance carrying no
  Name tag does.

Not in the document, and still hand written:

- **Deletion preconditions.** Google states the bucket precondition as the
  sentence "Deletes an empty bucket" in the method description. It is prose,
  not a field, so a generated client issues one delete and the API rejects it.
- **Deletion order.** No Discovery document carries it.
- **Waiting on a long-running operation.** 85% of deletes answer with one.
  Cloud Run's is addressed by its own name; Compute's needs the project, the
  zone and the operation name. The generator could emit this, since the
  document describes the operations resource, but it does not today, and the
  tests cover only an operation that is already done.
- **Telling a disabled API from a permission the caller lacks.** Google answers
  403 for both and only the message separates them.

## Destructive operations need a reviewed promotion

A generated descriptor offers `list` and `get` and nothing else. `create` and
`delete` are added only for a type named in a promotions file, so a scheduled
refresh of the schemas can widen what a provider can read but never what it can
destroy.

## What this does not claim

137 descriptors is not 137 working resource types. It is 137 types the document
describes well enough to try, of which two have been driven end to end against
a stand-in and none against Google.
