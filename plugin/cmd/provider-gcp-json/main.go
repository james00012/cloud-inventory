// Command provider-gcp-json serves Google Cloud resource types over JSON-RPC on
// its standard input and output. A host starts it as a subprocess and links no
// Google SDK; a shell script can drive it with jq.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/core"
	"github.com/james00012/cloud-inventory/plugin/jsonrpc"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
	runv2 "google.golang.org/api/run/v2"
)

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
		fail("cloud run location client", err)
	}
	services, err := runv2.NewService(ctx, options...)
	if err != nil {
		fail("cloud run client", err)
	}

	project := os.Getenv(projectVar)
	cloudRun := gcp.NewCloudRunServicesWithClients(locations, services)

	service := core.New(core.Spec[gcp.CloudRunService, gcp.CloudRunServiceSpec, gcp.Scope]{
		Lister:  cloudRun,
		Getter:  cloudRun,
		Deleter: cloudRun,
		Creator: cloudRun,

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
	})

	// Only protocol messages reach standard output, so a script can pipe it
	// straight into jq.
	if err := jsonrpc.NewServer(service).Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fail("serving", err)
	}
}

func fail(what string, err error) {
	fmt.Fprintln(os.Stderr, what+":", err)
	os.Exit(1)
}
