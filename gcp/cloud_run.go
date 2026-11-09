package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	cloudinv "github.com/james00012/cloud-inventory"
	runv1 "google.golang.org/api/run/v1"
	runv2 "google.golang.org/api/run/v2"
)

// CloudRunService is one Cloud Run service. It embeds the API's own type, so
// every field Google reports is reachable without this library modelling it,
// and adds only what the API does not give directly.
type CloudRunService struct {
	*runv2.GoogleCloudRunV2Service

	// ID is the canonical identifier every consumer stores and compares. It
	// holds the same string as Name, under the name the other providers use.
	ID cloudinv.ID

	// Location is in Name, but reading it out is this library's job.
	Location string

	// Created is CreateTime parsed, because the API reports it as a string.
	Created time.Time
}

func (s CloudRunService) ResourceID() cloudinv.ID { return s.ID }
func (s CloudRunService) ResourceType() string    { return "cloud-run-service" }
func (s CloudRunService) CreatedAt() time.Time    { return s.Created }

// DisplayName is the last segment of the resource name, which is what the
// console shows and what somebody writing a filter types. It is empty on the
// zero value, which Get and Create return beside an error.
func (s CloudRunService) DisplayName() string {
	if s.GoogleCloudRunV2Service == nil {
		return ""
	}
	return shortName(s.Name)
}

// Tags are Cloud Run's labels, under the name every provider shares.
func (s CloudRunService) Tags() map[string]string {
	if s.GoogleCloudRunV2Service == nil {
		return nil
	}
	return s.Labels
}

var _ cloudinv.Resource = CloudRunService{}

// CloudRunServiceSpec is what a caller supplies to make a service. It is the
// API's own request body, so every field Cloud Run accepts is available, plus
// the two values the API takes outside the body.
type CloudRunServiceSpec struct {
	*runv2.GoogleCloudRunV2Service

	// ServiceID is the short name, which the API takes as a query parameter.
	ServiceID string

	// Location is in the parent, which the API takes as a path segment.
	Location string
}

// CloudRunServices is the resource type's four calls.
type CloudRunServices struct {
	// locations is the v1 client, because listing a project's Cloud Run
	// regions is a v1 call. The v2 API has no equivalent.
	locations *runv1.APIService

	// services is the v2 client, which is the current API for a service.
	services *runv2.Service
}

// NewCloudRunServicesWithClients takes clients the caller built, which is how a
// test drives this against an httptest server.
func NewCloudRunServicesWithClients(locations *runv1.APIService, services *runv2.Service) *CloudRunServices {
	return &CloudRunServices{locations: locations, services: services}
}

func NewCloudRunServices(ctx context.Context) (*CloudRunServices, error) {
	locations, err := runv1.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloud run location client: %w", err)
	}
	services, err := runv2.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloud run client: %w", err)
	}
	return &CloudRunServices{locations: locations, services: services}, nil
}

// List fans out over the locations the scope asks for. The v2 list call refuses
// the "-" wildcard, so every consumer would otherwise write this loop, and the
// per-location failure handling with it.
func (c *CloudRunServices) List(ctx context.Context, scope Scope) ([]CloudRunService, cloudinv.Incomplete, error) {
	locations, err := c.listLocations(ctx, scope.ProjectID)
	if err != nil {
		return nil, cloudinv.Incomplete{}, fmt.Errorf("listing Cloud Run locations: %w", err)
	}

	offered := make(map[string]bool, len(locations))
	var asked []string
	for _, location := range locations {
		offered[strings.ToLower(location)] = true
		if scope.wants(location) {
			asked = append(asked, location)
		}
	}

	found, incomplete, err := cloudinv.FanOut(ctx, asked, faultOf, func(ctx context.Context, location string) ([]CloudRunService, error) {
		return c.listIn(ctx, scope.ProjectID, location)
	})

	// A location the caller named that the project does not offer is a question
	// nobody answered, not an empty location.
	if missing := cloudinv.Unoffered(scope.Locations, offered); len(missing) > 0 {
		incomplete.Gaps = append(incomplete.Gaps, missing...)
		incomplete.Asked += len(missing)
	}
	return found, incomplete, err
}

func (c *CloudRunServices) Get(ctx context.Context, id cloudinv.ID) (CloudRunService, error) {
	answer, err := c.services.Projects.Locations.Services.Get(string(id)).Context(ctx).Do()
	if err != nil {
		return CloudRunService{}, err
	}
	return serviceFrom(answer), nil
}

func (c *CloudRunServices) Delete(ctx context.Context, id cloudinv.ID) error {
	operation, err := c.services.Projects.Locations.Services.Delete(string(id)).Context(ctx).Do()
	if err != nil {
		// A service already gone is the state the caller wanted.
		if faultOf(err) == cloudinv.FaultNotFound {
			return nil
		}
		return err
	}
	return c.wait(ctx, operation)
}

func (c *CloudRunServices) Create(ctx context.Context, scope Scope, spec CloudRunServiceSpec) (CloudRunService, error) {
	if spec.GoogleCloudRunV2Service == nil {
		return CloudRunService{}, fmt.Errorf("the spec carries no service body")
	}

	parent := fmt.Sprintf("projects/%s/locations/%s", scope.ProjectID, spec.Location)

	operation, err := c.services.Projects.Locations.Services.
		Create(parent, spec.GoogleCloudRunV2Service).ServiceId(spec.ServiceID).Context(ctx).Do()
	if err != nil {
		return CloudRunService{}, err
	}
	if err := c.wait(ctx, operation); err != nil {
		return CloudRunService{}, err
	}

	// The create answers with an operation, so the library reads the service
	// back and returns the same type List and Get return.
	return c.Get(ctx, cloudinv.ID(parent+"/services/"+spec.ServiceID))
}

func (c *CloudRunServices) listLocations(ctx context.Context, projectID string) ([]string, error) {
	var found []string
	err := c.locations.Projects.Locations.List("projects/"+projectID).Pages(ctx, func(page *runv1.ListLocationsResponse) error {
		for _, location := range page.Locations {
			if location != nil && location.LocationId != "" {
				found = append(found, location.LocationId)
			}
		}
		return nil
	})
	return found, err
}

func (c *CloudRunServices) listIn(ctx context.Context, projectID, location string) ([]CloudRunService, error) {
	var found []CloudRunService
	parent := fmt.Sprintf("projects/%s/locations/%s", projectID, location)
	err := c.services.Projects.Locations.Services.List(parent).Pages(ctx, func(page *runv2.GoogleCloudRunV2ListServicesResponse) error {
		for _, service := range page.Services {
			if service != nil {
				found = append(found, serviceFrom(service))
			}
		}
		return nil
	})
	return found, err
}

func serviceFrom(service *runv2.GoogleCloudRunV2Service) CloudRunService {
	created, _ := time.Parse(time.RFC3339, service.CreateTime)

	return CloudRunService{
		GoogleCloudRunV2Service: service,
		ID:                      cloudinv.ID(service.Name),
		Location:                locationOf(service.Name),
		Created:                 created,
	}
}

// locationOf reads the location out of a full resource name, which is where GCP
// puts it: projects/{project}/locations/{location}/services/{service}.
func locationOf(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if part == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func shortName(name string) string {
	if at := strings.LastIndex(name, "/"); at >= 0 {
		return name[at+1:]
	}
	return name
}

const cloudRunOperationWait = 30 * time.Second

// operationCeiling bounds the whole wait, not one call of it.
const operationCeiling = 10 * time.Minute

func (c *CloudRunServices) wait(ctx context.Context, operation *runv2.GoogleLongrunningOperation) error {
	// A ceiling, so a stuck operation fails its call rather than holding the
	// run forever on a context the consumer never gave a deadline.
	ctx, cancel := context.WithTimeout(ctx, operationCeiling)
	defer cancel()

	for operation != nil && !operation.Done {
		if err := ctx.Err(); err != nil {
			return err
		}
		request := &runv2.GoogleLongrunningWaitOperationRequest{Timeout: fmt.Sprintf("%ds", int(cloudRunOperationWait.Seconds()))}
		answer, err := c.services.Projects.Locations.Operations.Wait(operation.Name, request).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("waiting for the operation to finish: %w", err)
		}
		operation = answer
	}
	if operation != nil && operation.Error != nil {
		return fmt.Errorf("%d: %s", operation.Error.Code, operation.Error.Message)
	}
	return nil
}

var (
	_ cloudinv.Lister[CloudRunService, Scope]                       = (*CloudRunServices)(nil)
	_ cloudinv.Getter[CloudRunService]                              = (*CloudRunServices)(nil)
	_ cloudinv.Deleter                                              = (*CloudRunServices)(nil)
	_ cloudinv.Creator[CloudRunService, CloudRunServiceSpec, Scope] = (*CloudRunServices)(nil)
)
