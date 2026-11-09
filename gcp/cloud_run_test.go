package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
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

// newFakeCloudRunWithLocations stands both API versions up on one server, so a
// test can drive List rather than only the single-service calls.
func newFakeCloudRunWithLocations(t *testing.T, handler http.HandlerFunc) *CloudRunServices {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	v1, err := runv1.NewService(context.Background(),
		option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	v2, err := runv2.NewService(context.Background(),
		option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)

	return NewCloudRunServicesWithClients(v1, v2)
}

// cloudRunServer answers the location list and then each location's services,
// refusing the locations a test names.
func cloudRunServer(t *testing.T, locations []string, refuse map[string]int, asked *[]string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/locations") {
			out := &runv1.ListLocationsResponse{}
			for _, location := range locations {
				out.Locations = append(out.Locations, &runv1.Location{LocationId: location})
			}
			require.NoError(t, json.NewEncoder(w).Encode(out))
			return
		}

		location := locationOf(strings.TrimSuffix(r.URL.Path, "/services"))
		*asked = append(*asked, location)

		if code, refused := refuse[location]; refused {
			w.WriteHeader(code)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code": code, "message": "refused",
					"details": []any{map[string]any{"reason": "SERVICE_DISABLED"}},
				},
			}))
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleCloudRunV2ListServicesResponse{
			Services: []*runv2.GoogleCloudRunV2Service{{
				Name: "projects/p/locations/" + location + "/services/one",
			}},
		}))
	}
}

// A run narrowed to one location asks that location alone, since the v2 API
// refuses a wildcard and every other location costs a call.
func TestListAsksOnlyTheLocationsTheScopeNames(t *testing.T) {
	t.Parallel()

	var asked []string
	client := newFakeCloudRunWithLocations(t, cloudRunServer(t, []string{"us-central1", "europe-west4"}, nil, &asked))

	found, incomplete, err := client.List(context.Background(),
		Scope{ProjectID: "p", Locations: []string{"europe-west4"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"europe-west4"}, asked)
	require.Len(t, found, 1)
	assert.Equal(t, "europe-west4", found[0].Location)
	assert.False(t, incomplete.Any())
}

// An exclusion is read inside the scope, so an excluded location is never
// asked and never counted among the locations the run depends on.
func TestListNeverAsksAnExcludedLocation(t *testing.T) {
	t.Parallel()

	var asked []string
	client := newFakeCloudRunWithLocations(t, cloudRunServer(t, []string{"us-central1", "europe-west4"}, nil, &asked))

	found, incomplete, err := client.List(context.Background(),
		Scope{ProjectID: "p", ExcludeLocations: []string{"europe-west4"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"us-central1"}, asked)
	assert.Len(t, found, 1)
	assert.Equal(t, 1, incomplete.Asked)
}

// One location refusing costs that location alone, and the fault says why.
func TestListReportsTheLocationItCouldNotRead(t *testing.T) {
	t.Parallel()

	var asked []string
	client := newFakeCloudRunWithLocations(t, cloudRunServer(t,
		[]string{"us-central1", "europe-west4"}, map[string]int{"us-central1": 403}, &asked))

	found, incomplete, err := client.List(context.Background(), Scope{ProjectID: "p"})
	require.NoError(t, err)

	assert.Len(t, found, 1)
	assert.False(t, incomplete.Total())
	assert.Contains(t, incomplete.String(), "us-central1 (api disabled)")
	assert.Error(t, incomplete.Cause())
}

// Every location refusing is a run that learned nothing, which a consumer must
// be able to tell from a project with no services.
func TestListReportsAWhollyUnreadProject(t *testing.T) {
	t.Parallel()

	var asked []string
	client := newFakeCloudRunWithLocations(t, cloudRunServer(t, []string{"us-central1", "europe-west4"},
		map[string]int{"us-central1": 403, "europe-west4": 403}, &asked))

	found, incomplete, err := client.List(context.Background(), Scope{ProjectID: "p"})
	require.NoError(t, err)

	assert.Empty(t, found)
	assert.True(t, incomplete.Total())
}

// A location the caller named that the project does not offer is a gap, or a
// misspelled location reads as an empty project.
func TestListReportsALocationTheProjectDoesNotOffer(t *testing.T) {
	t.Parallel()

	var asked []string
	client := newFakeCloudRunWithLocations(t, cloudRunServer(t, []string{"us-central1"}, nil, &asked))

	_, incomplete, err := client.List(context.Background(),
		Scope{ProjectID: "p", Locations: []string{"us-centrall"}})
	require.NoError(t, err)

	assert.Empty(t, asked)
	assert.Contains(t, incomplete.String(), "us-centrall")

	// Total is what a consumer branches on, so a run narrowed to one
	// misspelled location has to read as a run that learned nothing.
	assert.True(t, incomplete.Total())
}

// Get and Create return the zero value beside an error, and a destroyer's
// error path reads the name off it, so the contract methods must not panic.
func TestTheZeroServiceAnswersItsContractWithoutPanicking(t *testing.T) {
	t.Parallel()

	var zero CloudRunService
	assert.Empty(t, zero.DisplayName())
	assert.Nil(t, zero.Tags())
	assert.True(t, zero.CreatedAt().IsZero())
	assert.Equal(t, "cloud-run-service", zero.ResourceType())
}

// A spec with no body would reach the generated client as a nil request.
func TestCreateRefusesASpecWithNoBody(t *testing.T) {
	t.Parallel()

	client := newFakeCloudRun(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Create called the API with no body")
	})

	_, err := client.Create(context.Background(), Scope{ProjectID: "p"},
		CloudRunServiceSpec{ServiceID: "web", Location: "us-central1"})
	require.Error(t, err)
}

// An operation that never finishes must fail its call rather than hold the run
// forever, and the error has to name the operation the way the AWS side names
// its instance.
func TestWaitGivesUpOnAnOperationThatNeverFinishes(t *testing.T) {
	t.Parallel()

	client := newFakeCloudRun(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&runv2.GoogleLongrunningOperation{
			Name: "projects/p/locations/us-central1/operations/stuck", Done: false,
		}))
	})
	client.ceiling = 50 * time.Millisecond

	err := client.Delete(context.Background(),
		cloudinv.ID("projects/p/locations/us-central1/services/web"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "operations/stuck")
}
