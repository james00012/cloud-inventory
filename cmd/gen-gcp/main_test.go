package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/james00012/cloud-inventory/generic"
)

// storage is the shape Google publishes for Cloud Storage buckets: a listing
// scoped by a project the caller passes as a query parameter, and a bucket
// addressed by a path parameter of its own.
const storage = `{
  "name": "storage", "version": "v1",
  "baseUrl": "https://storage.googleapis.com/storage/v1/",
  "schemas": {
    "Buckets": {"properties": {"items": {"type": "array", "items": {"$ref": "Bucket"}}}},
    "Bucket": {"properties": {
      "id": {"type": "string"}, "name": {"type": "string"},
      "selfLink": {"type": "string"}, "timeCreated": {"type": "string"},
      "labels": {"type": "object"}
    }}
  },
  "resources": {"buckets": {"methods": {
    "list":   {"path": "b", "httpMethod": "GET",
               "parameters": {"project": {"required": true, "location": "query"},
                              "pageToken": {"location": "query"}},
               "response": {"$ref": "Buckets"}},
    "get":    {"path": "b/{bucket}", "httpMethod": "GET",
               "parameters": {"bucket": {"required": true, "location": "path"}}},
    "delete": {"path": "b/{bucket}", "httpMethod": "DELETE",
               "parameters": {"bucket": {"required": true, "location": "path"}}},
    "insert": {"path": "b", "httpMethod": "POST",
               "parameters": {"project": {"required": true, "location": "query"}}}
  }}}
}`

// pubsub is the shape of a type whose API reports no creation time at all,
// and whose listing is scoped by a parent in the path.
const pubsub = `{
  "name": "pubsub", "version": "v1",
  "baseUrl": "https://pubsub.googleapis.com/",
  "schemas": {
    "ListTopicsResponse": {"properties": {"topics": {"type": "array", "items": {"$ref": "Topic"}}}},
    "Topic": {"properties": {"name": {"type": "string"}, "labels": {"type": "object"}}}
  },
  "resources": {"projects": {"resources": {"topics": {"methods": {
    "list":   {"path": "v1/{+project}/topics", "httpMethod": "GET",
               "parameters": {"project": {"required": true, "location": "path"},
                              "pageToken": {"location": "query"}},
               "response": {"$ref": "ListTopicsResponse"}},
    "get":    {"path": "v1/{+topic}", "httpMethod": "GET",
               "parameters": {"topic": {"required": true, "location": "path"}}},
    "delete": {"path": "v1/{+topic}", "httpMethod": "DELETE",
               "parameters": {"topic": {"required": true, "location": "path"}}}
  }}}}}
}`

func describeAll(t *testing.T, document string) map[string]generic.Descriptor {
	t.Helper()

	var doc discovery
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatalf("reading the document: %v", err)
	}
	found := map[string]generic.Descriptor{}
	walk(&doc, doc.Resources, nil, found)
	return found
}

// TestABucketDescribesItselfFromTheDiscoveryDocument pins every field the
// generic client needs, derived with no hand-written input.
func TestABucketDescribesItselfFromTheDiscoveryDocument(t *testing.T) {
	got, ok := describeAll(t, storage)["buckets"]
	if !ok {
		t.Fatal("the document describes buckets and the generator did not")
	}

	if got.ListPath != "b" || got.GetPath != "b/{bucket}" || got.DeletePath != "b/{bucket}" {
		t.Fatalf("paths: list=%q get=%q delete=%q", got.ListPath, got.GetPath, got.DeletePath)
	}
	if got.ArrayField != "items" {
		t.Fatalf("collection field: %q", got.ArrayField)
	}
	if got.IdentityField != "selfLink" {
		t.Fatalf("identity: %q, wanted the self link, since a get takes a path parameter of its own", got.IdentityField)
	}
	if got.CreatedField != "timeCreated" {
		t.Fatalf("creation time: %q", got.CreatedField)
	}
	if got.LabelsField != "labels" {
		t.Fatalf("labels: %q", got.LabelsField)
	}
	if !got.Paged {
		t.Fatal("the list takes a page token and the descriptor says it does not")
	}
}

// TestAQueryScopeIsNotMistakenForAPathScope is the gap that made a bucket
// listing ask for every project the caller can see. A scope Google takes as a
// query parameter has to be read as one.
func TestAQueryScopeIsNotMistakenForAPathScope(t *testing.T) {
	bucket := describeAll(t, storage)["buckets"]
	if want := []string{"project"}; !reflect.DeepEqual(bucket.ScopeQuery, want) {
		t.Fatalf("query scope: %v, wanted %v", bucket.ScopeQuery, want)
	}
	if len(bucket.ScopeParams) != 0 {
		t.Fatalf("a bucket listing takes no path scope, got %v", bucket.ScopeParams)
	}

	topic := describeAll(t, pubsub)["projects.topics"]
	if want := []string{"project"}; !reflect.DeepEqual(topic.ScopeParams, want) {
		t.Fatalf("path scope: %v, wanted %v", topic.ScopeParams, want)
	}
	if len(topic.ScopeQuery) != 0 {
		t.Fatalf("a topic listing takes no query scope, got %v", topic.ScopeQuery)
	}
}

// TestATypeWithNoCreationTimeGetsNone covers Pub/Sub topics. Naming some other
// timestamp would make every topic look older than any cutoff.
func TestATypeWithNoCreationTimeGetsNone(t *testing.T) {
	got, ok := describeAll(t, pubsub)["projects.topics"]
	if !ok {
		t.Fatal("the document describes topics and the generator did not")
	}
	if got.CreatedField != "" {
		t.Fatalf("creation time: %q, and the API reports none", got.CreatedField)
	}
	if got.IdentityField != "name" {
		t.Fatalf("identity: %q, wanted the name, since the get takes one {+name}-style parameter", got.IdentityField)
	}
}

// TestANewTypeCannotDeleteUntilItIsPromoted is the safety property. A refresh
// of the schemas must never widen what a provider may destroy.
func TestANewTypeCannotDeleteUntilItIsPromoted(t *testing.T) {
	for name, got := range describeAll(t, storage) {
		if want := []string{"list", "get"}; !reflect.DeepEqual(got.Operations, want) {
			t.Fatalf("%s offers %v straight out of the generator, wanted %v", name, got.Operations, want)
		}
	}
}
