package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/host"
	runv1 "google.golang.org/api/run/v1"
	runv2 "google.golang.org/api/run/v2"
)

// buildProvider compiles the real provider binary once, so these tests drive a
// separate process over gRPC rather than a server in this one. The package
// path is relative to this directory, since that is where a test runs.
var buildProvider = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "inventory-provider")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "provider-gcp")

	out, err := exec.Command("go", "build", "-o", binary, "../cmd/provider-gcp").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return binary, nil
})

// cloudRun stands Cloud Run up on one server and records what it was asked.
type cloudRun struct {
	mu      sync.Mutex
	seen    []string
	exists  bool
	refuse  map[string]bool
	project string
}

func (c *cloudRun) handler(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Method+" "+r.URL.Path)
		c.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		done := &runv2.GoogleLongrunningOperation{Name: "operations/one", Done: true}

		switch {
		case strings.HasSuffix(r.URL.Path, "/locations"):
			_ = json.NewEncoder(w).Encode(&runv1.ListLocationsResponse{Locations: []*runv1.Location{
				{LocationId: "us-central1"}, {LocationId: "europe-west1"},
			}})

		case r.Method == http.MethodDelete:
			c.mu.Lock()
			c.exists = false
			c.mu.Unlock()
			_ = json.NewEncoder(w).Encode(done)

		case r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, ":wait"):
			c.mu.Lock()
			c.exists = true
			c.mu.Unlock()
			_ = json.NewEncoder(w).Encode(done)

		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(done)

		case c.refusedFor(r.URL.Path):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": 403, "message": "caller lacks permission"},
			})

		case strings.HasSuffix(r.URL.Path, "/services"):
			answer := &runv2.GoogleCloudRunV2ListServicesResponse{}
			if c.present() {
				answer.Services = append(answer.Services, c.body())
			}
			_ = json.NewEncoder(w).Encode(answer)

		case !c.present():
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": 404, "message": "service not found"},
			})

		default:
			_ = json.NewEncoder(w).Encode(c.body())
		}
	}
}

func (c *cloudRun) body() *runv2.GoogleCloudRunV2Service {
	return &runv2.GoogleCloudRunV2Service{
		Name:        "projects/demo/locations/us-central1/services/api",
		CreateTime:  "2026-11-01T00:00:00Z",
		Uri:         "https://api.example.run.app",
		LaunchStage: "GA",
		Labels:      map[string]string{"env": "demo"},
		Template: &runv2.GoogleCloudRunV2RevisionTemplate{
			ServiceAccount: "runner@demo.iam.gserviceaccount.com",
			Containers:     []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
		},
	}
}

func (c *cloudRun) present() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exists
}

func (c *cloudRun) refusedFor(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for location := range c.refuse {
		if strings.Contains(path, location) {
			return true
		}
	}
	return false
}

func (c *cloudRun) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// start compiles the provider, points it at the fake, and configures a session.
func start(t *testing.T, fake *cloudRun, features ...host.Feature) *host.Session {
	t.Helper()

	binary, err := buildProvider()
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)

	t.Setenv("CLOUD_INVENTORY_GCP_ENDPOINT", server.URL)
	t.Setenv("CLOUD_INVENTORY_GCP_PROJECT", "demo")

	session, err := host.Start(context.Background(), host.Options{
		Binary:        binary,
		Features:      features,
		ExpectAccount: "demo",
		Scope:         gcp.Scope{ProjectID: "demo"},
	})
	if err != nil {
		t.Fatalf("starting the session: %v", err)
	}
	t.Cleanup(session.Close)

	return session
}

// TestTheWholeContractOverARealSubprocess drives every call in the contract
// across the plugin boundary, against a provider running as its own process.
func TestTheWholeContractOverARealSubprocess(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := start(t, fake, host.Create, host.Delete)
	ctx := context.Background()

	if session.Account() != "demo" {
		t.Fatalf("session bound to %q", session.Account())
	}

	scopes, err := session.DiscoverScopes(ctx)
	if err != nil {
		t.Fatalf("discovering scopes: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("discovered %v", scopes)
	}

	made, err := session.Create(ctx, gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api",
		Location:  "us-central1",
		GoogleCloudRunV2Service: &runv2.GoogleCloudRunV2Service{
			Template: &runv2.GoogleCloudRunV2RevisionTemplate{
				Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	// The five common fields are typed on the wire, so a host filters on them
	// without knowing anything about Cloud Run.
	if made.ID != gcp.ServiceID("demo", "us-central1", "api") {
		t.Fatalf("got id %q", made.ID)
	}
	if made.Type != "cloud-run-service" || made.DisplayName != "api" {
		t.Fatalf("got type %q name %q", made.Type, made.DisplayName)
	}
	if made.CreatedAt.IsZero() {
		t.Fatal("the creation time did not survive the wire")
	}
	if made.Tags["env"] != "demo" {
		t.Fatalf("got tags %v", made.Tags)
	}

	got, err := session.Get(ctx, made.ID, false)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if got.ID != made.ID {
		t.Fatalf("get answered for %q", got.ID)
	}
	if len(got.FullData) != 0 {
		t.Fatal("full data arrived without being asked for")
	}

	found, incomplete, err := session.List(ctx, gcp.Scope{ProjectID: "demo"}, false)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if incomplete.Any() {
		t.Fatalf("a clean project reported gaps: %v", incomplete.String())
	}
	if len(found) != 2 {
		t.Fatalf("listed %d services across two locations", len(found))
	}

	if err := session.Delete(ctx, made.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if fake.present() {
		t.Fatal("the delete did not reach the cloud")
	}
}

// TestAReadOnlySessionIsRefusedBeforeTheCloud pins the safety model: a call the
// session was not granted never becomes an API call.
func TestAReadOnlySessionIsRefusedBeforeTheCloud(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, exists: true}
	session := start(t, fake)
	ctx := context.Background()

	err := session.Delete(ctx, gcp.ServiceID("demo", "us-central1", "api"))
	if err == nil {
		t.Fatal("a read-only session deleted a service")
	}
	if !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	for _, request := range fake.asked() {
		if strings.HasPrefix(request, "DELETE") {
			t.Fatalf("the refusal still reached the cloud: %v", fake.asked())
		}
	}
	if !fake.present() {
		t.Fatal("the service was deleted anyway")
	}
}

// TestASessionPointedAtTheWrongAccountDoesNotStart pins the account binding.
func TestASessionPointedAtTheWrongAccountDoesNotStart(t *testing.T) {
	binary, err := buildProvider()
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	fake := &cloudRun{refuse: map[string]bool{}}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)

	t.Setenv("CLOUD_INVENTORY_GCP_ENDPOINT", server.URL)
	t.Setenv("CLOUD_INVENTORY_GCP_PROJECT", "demo")

	session, err := host.Start(context.Background(), host.Options{
		Binary:        binary,
		ExpectAccount: "some-other-project",
		Scope:         gcp.Scope{ProjectID: "demo"},
	})
	if err == nil {
		session.Close()
		t.Fatal("a session pointed at the wrong project started")
	}
	if !strings.Contains(err.Error(), "not the some-other-project") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestALocationThatCouldNotBeReadArrivesAsAGap pins that a partial read crosses
// the boundary as unknown rather than as an empty project.
func TestALocationThatCouldNotBeReadArrivesAsAGap(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{"europe-west1": true}, exists: true}
	session := start(t, fake)

	_, incomplete, err := session.List(context.Background(), gcp.Scope{ProjectID: "demo"}, false)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if !incomplete.Any() {
		t.Fatal("a refused location arrived as an empty one")
	}
	if incomplete.Asked != 2 {
		t.Fatalf("asked reported %d", incomplete.Asked)
	}
	if incomplete.Total() {
		t.Fatal("one refused location of two read as a total failure")
	}
	if len(incomplete.Gaps) != 1 || incomplete.Gaps[0].Fault != cloudinv.FaultDenied {
		t.Fatalf("the fault did not survive the wire: %+v", incomplete.Gaps)
	}
}
