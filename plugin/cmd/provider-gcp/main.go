// Command provider-gcp serves Google Cloud resource types over the inventory
// contract. A host starts it as a subprocess and never links a Google SDK.
package main

import (
	"context"
	"fmt"
	"os"

	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/provider"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
	runv2 "google.golang.org/api/run/v2"
)

// endpointVar points the provider at a stand-in for Cloud Run, which is how it
// is driven in a test. Unset, it reaches Google with ambient credentials.
const (
	endpointVar = "CLOUD_INVENTORY_GCP_ENDPOINT"
	projectVar  = "CLOUD_INVENTORY_GCP_PROJECT"
)

func main() {
	ctx := context.Background()

	var options []option.ClientOption
	if endpoint := os.Getenv(endpointVar); endpoint != "" {
		options = append(options, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	}

	locations, err := runv1.NewService(ctx, options...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloud run location client:", err)
		os.Exit(1)
	}
	services, err := runv2.NewService(ctx, options...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloud run client:", err)
		os.Exit(1)
	}

	project := os.Getenv(projectVar)
	cloudRun := gcp.NewCloudRunServicesWithClients(locations, services)

	provider.Serve(provider.Server(provider.Resource[gcp.CloudRunService, gcp.CloudRunServiceSpec, gcp.Scope]{
		Lister:  cloudRun,
		Getter:  cloudRun,
		Deleter: cloudRun,
		Creator: cloudRun,

		// On Google the account a session is bound to is the project.
		Account: func(context.Context) (string, error) { return project, nil },

		Scopes: func(ctx context.Context) ([]string, error) {
			answer, err := locations.Projects.Locations.List("projects/" + project).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			var found []string
			for _, location := range answer.Locations {
				found = append(found, location.LocationId)
			}
			return found, nil
		},

		Fault: func(error) cloudinv.Fault { return cloudinv.FaultUnknown },
	}))
}
