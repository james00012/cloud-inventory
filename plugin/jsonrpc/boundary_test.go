package jsonrpc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/core"
	"github.com/james00012/cloud-inventory/plugin/jsonrpc"
	runv1 "google.golang.org/api/run/v1"
	runv2 "google.golang.org/api/run/v2"
)

// build compiles the real provider once, so these tests drive a separate
// process over its standard input and output.
var build = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "provider")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "provider-gcp-json")
	out, err := exec.Command("go", "build", "-o", binary, "../cmd/provider-gcp-json").CombinedOutput()
	if err != nil {
		return "", &buildFailed{string(out), err}
	}
	return binary, nil
})

type buildFailed struct {
	out string
	err error
}

func (b *buildFailed) Error() string { return b.err.Error() + ": " + b.out }

// cloudRun stands Cloud Run up and records what it was asked.
type cloudRun struct {
	mu      sync.Mutex
	posted  []map[string]any
	seen    []string
	count   int
	refuse  map[string]bool
	present bool
}

func (c *cloudRun) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Method+" "+r.URL.Path)
		c.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		done := map[string]any{"name": "operations/one", "done": true}

		switch {
		case strings.HasSuffix(r.URL.Path, "/locations"):
			_ = json.NewEncoder(w).Encode(&runv1.ListLocationsResponse{Locations: []*runv1.Location{
				{LocationId: "us-central1"}, {LocationId: "europe-west1"},
			}})
		case r.Method == http.MethodDelete:
			c.set(false)
			_ = json.NewEncoder(w).Encode(done)
		case r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, ":wait"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.mu.Lock()
			c.posted = append(c.posted, body)
			c.mu.Unlock()
			c.set(true)
			_ = json.NewEncoder(w).Encode(done)
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(done)
		case c.refusedFor(r.URL.Path):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "caller lacks permission"}})
		case strings.HasSuffix(r.URL.Path, "/services"):
			answer := &runv2.GoogleCloudRunV2ListServicesResponse{}
			for i := 0; i < c.many(); i++ {
				answer.Services = append(answer.Services, c.body(i))
			}
			_ = json.NewEncoder(w).Encode(answer)
		case !c.has(r.URL.Path):
			// Strict about the path and about existence, so a provider that
			// builds the wrong path, or reads one nothing created, is caught
			// here rather than answered anyway.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "service not found"}})
		default:
			_ = json.NewEncoder(w).Encode(c.body(0))
		}
	}
}

func (c *cloudRun) body(i int) *runv2.GoogleCloudRunV2Service {
	return &runv2.GoogleCloudRunV2Service{
		Name:       "projects/demo/locations/us-central1/services/api" + strings.Repeat("x", i),
		CreateTime: "2026-11-01T00:00:00Z",
		Uri:        "https://api.example.run.app",
		Labels:     map[string]string{"env": "demo"},
		Template: &runv2.GoogleCloudRunV2RevisionTemplate{
			ServiceAccount: "runner@demo.iam.gserviceaccount.com",
		},
	}
}

func (c *cloudRun) set(v bool) { c.mu.Lock(); c.present = v; c.mu.Unlock() }

// has is true only where the service exists and the path names it, so a
// provider that builds the wrong path is caught here rather than answered.
func (c *cloudRun) has(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.present && strings.HasSuffix(path, "/projects/demo/locations/us-central1/services/api")
}
func (c *cloudRun) many() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count > 0 {
		return c.count
	}
	if c.present {
		return 1
	}
	return 0
}
func (c *cloudRun) refusedFor(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for l := range c.refuse {
		if strings.Contains(path, l) {
			return true
		}
	}
	return false
}
func (c *cloudRun) creates() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.posted...)
}

func (c *cloudRun) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func serve(t *testing.T, fake *cloudRun) string {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	t.Setenv("CLOUD_INVENTORY_GCP_ENDPOINT", server.URL)
	t.Setenv("CLOUD_INVENTORY_GCP_PROJECT", "demo")
	return server.URL
}

func start(t *testing.T, fake *cloudRun, features ...core.Feature) *jsonrpc.Session {
	t.Helper()
	return startAs(t, fake, "", "", features...)
}

func startAs(t *testing.T, fake *cloudRun, caller, run string, features ...core.Feature) *jsonrpc.Session {
	t.Helper()
	binary, err := build()
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	serve(t, fake)

	session, err := jsonrpc.Start(context.Background(), jsonrpc.Options{
		Binary: binary, Features: features, ExpectAccount: "demo",
		Caller: caller, Run: run,
		Scope: gcp.Scope{ProjectID: "demo"}, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatalf("starting the session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestTheWholeContractOverStandardInputAndOutput(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := start(t, fake, core.Create, core.Delete)

	if session.Account() != "demo" {
		t.Fatalf("bound to %q", session.Account())
	}

	scopes, err := session.DiscoverScopes()
	if err != nil || len(scopes) != 2 {
		t.Fatalf("discovering scopes: %v %v", scopes, err)
	}

	made, err := session.Create(gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api", Location: "us-central1",
		GoogleCloudRunV2Service: &runv2.GoogleCloudRunV2Service{
			Template: &runv2.GoogleCloudRunV2RevisionTemplate{
				Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if made.ID != gcp.ServiceID("demo", "us-central1", "api") {
		t.Fatalf("id %q", made.ID)
	}
	if made.DisplayName != "api" || made.Tags["env"] != "demo" || made.CreatedAt == nil {
		t.Fatalf("the common fields did not survive: %+v", made)
	}

	// The cloud's own body crosses as bytes, and a host that wants its fields
	// supplies a type that models them.
	var view runv2.GoogleCloudRunV2Service
	if err := json.Unmarshal(made.FullData, &view); err != nil {
		t.Fatalf("decoding full data: %v", err)
	}
	if view.Template.ServiceAccount != "runner@demo.iam.gserviceaccount.com" {
		t.Fatal("the nested template did not survive")
	}

	got, err := session.Get(made.ID, false)
	if err != nil || got.ID != made.ID {
		t.Fatalf("getting: %v %v", got.ID, err)
	}
	if len(got.FullData) != 0 {
		t.Fatal("full data arrived without being asked for")
	}

	if err := session.Delete(made.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	for _, request := range fake.asked() {
		if strings.HasPrefix(request, "DELETE") {
			return
		}
	}
	t.Fatal("the delete did not reach the cloud")
}

// TestAListStreams pins that resources arrive one message at a time rather than
// in the reply, which is what the protocol promises.
func TestAListStreams(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, count: 3}
	session := start(t, fake)

	var arrived []string
	listing, err := session.List(gcp.Scope{ProjectID: "demo"}, false, func(one core.Resource) error {
		arrived = append(arrived, string(one.ID))
		return nil
	})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}

	// Two locations, three services each.
	if len(arrived) != 6 {
		t.Fatalf("streamed %d resources", len(arrived))
	}
	if len(listing.Resources) != 0 {
		t.Fatal("the reply also carried the resources, so nothing was streamed")
	}
	if listing.Asked != 2 {
		t.Fatalf("asked %d", listing.Asked)
	}
}

func TestAReadOnlySessionIsRefusedBeforeTheCloud(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, present: true}
	session := start(t, fake)

	err := session.Delete(gcp.ServiceID("demo", "us-central1", "api"))
	if err == nil {
		t.Fatal("a read-only session deleted a service")
	}

	// The refusal carries a kind, so a host acts on it rather than on the text.
	var protocol *jsonrpc.Error
	if !asProtocol(err, &protocol) {
		t.Fatalf("refused with %T", err)
	}
	if protocol.Kind != "notEnabled" || protocol.Code != jsonrpc.CodeNotEnabled {
		t.Fatalf("refused as %+v", protocol)
	}
	for _, request := range fake.asked() {
		if strings.HasPrefix(request, "DELETE") {
			t.Fatal("the refusal still reached the cloud")
		}
	}
}

func TestASessionPointedAtTheWrongAccountDoesNotStart(t *testing.T) {
	binary, err := build()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	fake := &cloudRun{refuse: map[string]bool{}}
	serve(t, fake)

	session, err := jsonrpc.Start(context.Background(), jsonrpc.Options{
		Binary: binary, ExpectAccount: "another-project",
		Scope: gcp.Scope{ProjectID: "demo"}, Stderr: os.Stderr,
	})
	if err == nil {
		_ = session.Close()
		t.Fatal("a session pointed at the wrong project started")
	}
	if !strings.Contains(err.Error(), "not the another-project") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestALocationThatCouldNotBeReadArrivesAsAGap(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{"europe-west1": true}, present: true}
	session := start(t, fake)

	listing, err := session.List(gcp.Scope{ProjectID: "demo"}, false, nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listing.Gaps) != 1 {
		t.Fatalf("gaps %+v", listing.Gaps)
	}
	if listing.Gaps[0].Fault != "permission denied" {
		t.Fatalf("the fault kind did not survive: %q", listing.Gaps[0].Fault)
	}
	if listing.Asked != 2 {
		t.Fatalf("asked %d", listing.Asked)
	}
}

func asProtocol(err error, into **jsonrpc.Error) bool {
	if e, ok := err.(*jsonrpc.Error); ok {
		*into = e
		return true
	}
	return false
}

// TestAnEmptySpecBodyStillCrossesTheBoundary records what the spec's own
// marshalling bought. The body is an embedded Google struct with its own
// MarshalJSON, which Go promotes, so a spec used to lose both its own fields
// and an empty body on the way out. Now an empty body arrives as an empty
// body, and whether that is a legal create is the cloud's business.
func TestAnEmptySpecBodyStillCrossesTheBoundary(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := start(t, fake, core.Create)

	_, err := session.Create(gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api", Location: "us-central1",
		GoogleCloudRunV2Service: &runv2.GoogleCloudRunV2Service{},
	}, false)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	creates := fake.creates()
	if len(creates) != 1 {
		t.Fatalf("the provider posted %d creates", len(creates))
	}
	// The path carries the spec's own fields, which is what used to vanish.
	for _, request := range fake.asked() {
		if strings.HasPrefix(request, "POST") && strings.Contains(request, "locations/us-central1/services") {
			return
		}
	}
	t.Fatalf("the create did not reach the right path: %v", fake.asked())
}
