package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv2 "google.golang.org/api/run/v2"
)

// newFakeCloudRun stands the v2 API up on an httptest server, which is the
// pattern terratest already uses for its own Cloud Run tests.
func newFakeCloudRun(t *testing.T, handler http.HandlerFunc) *CloudRunServices {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	service, err := runv2.NewService(context.Background(),
		option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)

	return NewCloudRunServicesWithClients(nil, service)
}

// Create is the call no consumer drives today, so without this the library
// would ship a quarter of its surface unexercised.
func TestCreateSendsTheApisOwnBodyAndReadsTheServiceBack(t *testing.T) {
	t.Parallel()

	var posted runv2.GoogleCloudRunV2Service
	var postedID string

	client := newFakeCloudRun(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPost {
			postedID = r.URL.Query().Get("serviceId")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
			require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleLongrunningOperation{
				Name: "operations/one", Done: true,
			}))
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleCloudRunV2Service{
			Name:       "projects/p/locations/us-central1/services/web",
			CreateTime: "2026-11-01T00:00:00Z",
			Uri:        "https://web.example.run.app",
			Labels:     map[string]string{"env": "test"},
			Template: &runv2.GoogleCloudRunV2RevisionTemplate{
				Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/p/web:1"}},
			},
		}))
	})

	made, err := client.Create(context.Background(), Scope{ProjectID: "p"}, CloudRunServiceSpec{
		GoogleCloudRunV2Service: &runv2.GoogleCloudRunV2Service{
			Labels: map[string]string{"env": "test"},
			Template: &runv2.GoogleCloudRunV2RevisionTemplate{
				Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/p/web:1"}},
			},
		},
		ServiceID: "web",
		Location:  "us-central1",
	})
	require.NoError(t, err)

	// The spec's fields reach the API untouched, because the spec is the API's
	// own body rather than something this library models.
	assert.Equal(t, "web", postedID)
	assert.Equal(t, map[string]string{"env": "test"}, posted.Labels)
	require.NotNil(t, posted.Template)
	require.Len(t, posted.Template.Containers, 1)
	assert.Equal(t, "gcr.io/p/web:1", posted.Template.Containers[0].Image)

	// Create answers with the type List and Get answer with.
	assert.Equal(t, cloudinv.ID("projects/p/locations/us-central1/services/web"), made.ID)
	assert.Equal(t, "us-central1", made.Location)
	assert.False(t, made.Created.IsZero())

	// The API's own fields are reachable without this library modelling them.
	assert.Equal(t, "https://web.example.run.app", made.Uri)
	assert.Equal(t, "web", made.DisplayName())
	assert.Equal(t, map[string]string{"env": "test"}, made.Tags())
	require.Len(t, made.Template.Containers, 1)
	assert.Equal(t, "gcr.io/p/web:1", made.Template.Containers[0].Image)
}

// A delete waits for the operation, or a caller is told a service is gone while
// a later list still answers with it.
func TestDeleteWaitsForTheOperationToFinish(t *testing.T) {
	t.Parallel()

	waits := 0
	client := newFakeCloudRun(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodDelete {
			require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleLongrunningOperation{
				Name: "projects/p/locations/us-central1/operations/one", Done: false,
			}))
			return
		}

		waits++
		require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleLongrunningOperation{
			Name: "projects/p/locations/us-central1/operations/one", Done: waits >= 2,
		}))
	})

	require.NoError(t, client.Delete(context.Background(),
		cloudinv.ID("projects/p/locations/us-central1/services/web")))
	assert.Equal(t, 2, waits)
}

// Naming no location means every location, and a location is matched however
// it is typed, since a region reaches this from a command line.
func TestScopeMatchesALocationHoweverItIsTyped(t *testing.T) {
	t.Parallel()

	assert.True(t, Scope{ProjectID: "p"}.wants("us-central1"))
	assert.True(t, Scope{ProjectID: "p", Locations: []string{"US-CENTRAL1"}}.wants("us-central1"))
	assert.False(t, Scope{ProjectID: "p", Locations: []string{"US-CENTRAL1"}}.wants("europe-west4"))
}

// A consumer decides what each fault costs, so the library has to tell a
// switched-off API apart from a refused permission.
func TestFaultOfReadsTheShapesAConsumerActsOn(t *testing.T) {
	t.Parallel()

	disabled := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "SERVICE_DISABLED"}}}
	assert.Equal(t, cloudinv.FaultAPIDisabled, faultOf(disabled))

	denied := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "forbidden"}}}
	assert.Equal(t, cloudinv.FaultDenied, faultOf(denied))

	assert.Equal(t, cloudinv.FaultThrottle, faultOf(&googleapi.Error{Code: 429}))
	assert.Equal(t, cloudinv.FaultNotFound, faultOf(&googleapi.Error{Code: 404}))
}

// The location is in the resource name, which is the only place GCP puts it on
// a listed service.
func TestLocationOfReadsTheNameGcpAnswersWith(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "us-central1", locationOf("projects/p/locations/us-central1/services/one"))
	assert.Equal(t, "", locationOf("nonsense"))
}
