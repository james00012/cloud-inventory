package generic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// load reads a descriptor the generator produced. Nothing in these tests was
// written by hand for a resource type.
func load(t *testing.T, file, name string) Descriptor {
	t.Helper()

	body, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatalf("reading the generated descriptors: %v", err)
	}
	var doc struct {
		Descriptors map[string]Descriptor `json:"descriptors"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("reading the generated descriptors: %v", err)
	}
	d, ok := doc.Descriptors[name]
	if !ok {
		t.Fatalf("the generator produced no descriptor for %s", name)
	}
	return d
}

// TestCloudRunFromAGeneratedDescriptor drives a newer Google API, where a
// resource is addressed by a self-contained resource path.
func TestCloudRunFromAGeneratedDescriptor(t *testing.T) {
	var seen []string
	exists := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		service := map[string]any{
			"name":       "projects/demo/locations/us-central1/services/api",
			"createTime": "2026-11-01T00:00:00Z",
			"uri":        "https://api.example.run.app",
			"labels":     map[string]any{"env": "demo"},
		}

		switch {
		case r.Method == http.MethodPost:
			exists = true
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operations/one", "done": true})
		case r.Method == http.MethodDelete:
			exists = false
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operations/two", "done": true})
		case strings.HasSuffix(r.URL.Path, "/services"):
			out := map[string]any{}
			if exists {
				out["services"] = []any{service}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "not found"}})
				return
			}
			_ = json.NewEncoder(w).Encode(service)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Descriptor: load(t, "run-v2.json", "projects.locations.services")}
	scope := Scope{"parent": {"projects/demo/locations/us-central1"}}
	ctx := context.Background()

	made, err := client.Create(ctx, scope, Spec{
		Name: "api",
		Body: map[string]any{"template": map[string]any{"containers": []any{map[string]any{"image": "gcr.io/demo/api:1"}}}},
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	// The five contract methods come from the descriptor, not from code
	// written for Cloud Run.
	if got := string(made.ResourceID()); got != "projects/demo/locations/us-central1/services/api" {
		t.Fatalf("identity %q", got)
	}
	if made.DisplayName() != "api" {
		t.Fatalf("display name %q", made.DisplayName())
	}
	if made.CreatedAt().IsZero() {
		t.Fatal("no creation time")
	}
	if made.Tags()["env"] != "demo" {
		t.Fatalf("tags %v", made.Tags())
	}
	if made.Body["uri"] != "https://api.example.run.app" {
		t.Fatal("the API's own fields are not reachable")
	}

	found, incomplete, err := client.List(ctx, scope)
	if err != nil || incomplete.Any() {
		t.Fatalf("listing: %v %v", err, incomplete.String())
	}
	if len(found) != 1 {
		t.Fatalf("listed %d", len(found))
	}

	if err := client.Delete(ctx, made.ResourceID()); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if exists {
		t.Fatal("the delete did not reach the API")
	}
}

// TestComputeFromAGeneratedDescriptor drives an older Google API, where a get
// takes three path parameters and the resource is addressed by its selfLink.
func TestComputeFromAGeneratedDescriptor(t *testing.T) {
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operation-1", "status": "DONE"})
			return
		}

		instance := map[string]any{
			"name":              "web-1",
			"creationTimestamp": "2026-11-01T00:00:00Z",
			"labels":            map[string]any{"env": "demo"},
			"machineType":       "zones/us-central1-a/machineTypes/e2-micro",
			"selfLink":          "http://" + r.Host + "/projects/demo/zones/us-central1-a/instances/web-1",
		}
		if strings.HasSuffix(r.URL.Path, "/instances") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{instance}})
			return
		}
		_ = json.NewEncoder(w).Encode(instance)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Descriptor: load(t, "compute-v1.json", "instances")}
	scope := Scope{"project": {"demo"}, "zone": {"us-central1-a"}}
	ctx := context.Background()

	found, incomplete, err := client.List(ctx, scope)
	if err != nil || incomplete.Any() {
		t.Fatalf("listing: %v %v", err, incomplete.String())
	}
	if len(found) != 1 {
		t.Fatalf("listed %d", len(found))
	}

	one := found[0]
	if one.DisplayName() != "web-1" {
		t.Fatalf("display name %q", one.DisplayName())
	}
	if one.CreatedAt().IsZero() {
		t.Fatal("no creation time")
	}
	if one.Tags()["env"] != "demo" {
		t.Fatalf("tags %v", one.Tags())
	}

	// The identity is the selfLink, which a get and a delete address directly.
	got, err := client.Get(ctx, one.ResourceID())
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if got.Body["machineType"] != "zones/us-central1-a/machineTypes/e2-micro" {
		t.Fatal("the API's own fields are not reachable")
	}

	if err := client.Delete(ctx, one.ResourceID()); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if deleted != "/projects/demo/zones/us-central1-a/instances/web-1" {
		t.Fatalf("deleted %q", deleted)
	}
}

// TestAZoneThatCouldNotBeReadIsAGap pins that the generic path keeps the
// contract's partial-read promise, since it fans out over the scope itself.
func TestAZoneThatCouldNotBeReadIsAGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "europe-west1-b") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "caller lacks permission"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Descriptor: load(t, "compute-v1.json", "instances")}

	_, incomplete, err := client.List(context.Background(), Scope{
		"project": {"demo"}, "zone": {"us-central1-a", "europe-west1-b"},
	})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if !incomplete.Any() {
		t.Fatal("a refused zone read as an empty one")
	}
	if incomplete.Asked != 2 || incomplete.Total() {
		t.Fatalf("asked %d, total %v", incomplete.Asked, incomplete.Total())
	}
	if len(incomplete.Gaps) != 1 || incomplete.Gaps[0].Fault.String() == "" {
		t.Fatalf("gaps %+v", incomplete.Gaps)
	}
}

// TestANewTypeArrivesListOnly pins the promotion rule: generation can add a
// type, but only a reviewed change can let it create or delete. A scheduled
// refresh that picked up a new type must not be able to nuke with it.
func TestANewTypeArrivesListOnly(t *testing.T) {
	d := load(t, "run-v2-jobs.json", "projects.locations.jobs")

	if len(d.Operations) != 2 || d.Operations[0] != "list" || d.Operations[1] != "get" {
		t.Fatalf("a freshly generated type offers %v", d.Operations)
	}

	client := &Client{BaseURL: "http://unused", Descriptor: d}
	ctx := context.Background()

	if err := client.Delete(ctx, "projects/demo/locations/us-central1/jobs/nightly"); err == nil {
		t.Fatal("a list-only type deleted something")
	} else if !strings.Contains(err.Error(), "reviewed change") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	if _, err := client.Create(ctx, Scope{"parent": {"projects/demo/locations/us-central1"}}, Spec{Name: "nightly"}); err == nil {
		t.Fatal("a list-only type created something")
	}
}

// TestAPromotedTypeCanDelete is the other half: once reviewed, the same
// generated descriptor carries the operation.
func TestAPromotedTypeCanDelete(t *testing.T) {
	d := load(t, "run-v2.json", "projects.locations.services")

	if !d.offers("delete") {
		t.Fatalf("a promoted type offers %v", d.Operations)
	}
}
