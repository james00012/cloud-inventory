# Generation from schemas, tried

RFC-188's central claim is that coverage should grow when a cloud publishes a
new resource type, not when somebody writes code for it, through "one generic
implementation per cloud" driven by data read out of the cloud's own schemas.

This is that, for Google: a generator that reads a Discovery document, and one
client that serves any resource type the generator described.

## What came out

Across eight Google APIs, every resource type offering list, get and delete
produced a complete descriptor with **no human input**.

| | |
| --- | --- |
| resource types examined | 137 |
| described by the generator | 137 |
| verified end to end against a stand-in | 2 |

The two verified are Cloud Run services and Compute instances, chosen because
they address resources in opposite ways. Both run through the same client, with
create, list, get, delete, and the contract's five methods, from a descriptor
nobody edited.

## What the document does and does not say

The parts that are mechanical:

- **Identity, in 99% of types.** A newer API's get takes one self-contained
  resource path, so the name is the identity. An older API's get takes two to
  four path parameters, so it is not, and the `selfLink` those resources carry
  is used instead. 15% are the first kind and 84% carry a selfLink.
- **Creation time in 88%** and **labels in 29%**. A type with neither answers
  the contract with a zero value, the way an EC2 instance with no Name tag does.
- **Paths, pagination and the list response's array field**, every time.

The parts that are not:

- **Waiting for an operation.** 85% of deletes answer with one. Cloud Run's is
  addressed by its own name; Compute's needs the project, the zone and the
  operation name. The generator could emit that too, since the document
  describes the operations resource, but it does not today, and the test covers
  only the case where the operation is already done.
- **Telling a disabled API from a permission the caller lacks.** Google answers
  403 for both and only the message separates them.
- **Delete order and pre-delete steps.** No Discovery document carries either.
  This is the hand-written layer the RFC names, and nothing here reduces it.

## What this does not claim

137 descriptors is not 137 working resource types. It is 137 types the document
describes well enough to try, of which two have been driven end to end against a
stand-in and none against Google.
