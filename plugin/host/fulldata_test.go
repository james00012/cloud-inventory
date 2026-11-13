package host_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	cloudinv "github.com/james00012/cloud-inventory"
	awsinv "github.com/james00012/cloud-inventory/aws"
	"github.com/james00012/cloud-inventory/gcp"
	pb "github.com/james00012/cloud-inventory/plugin/proto"
	"github.com/james00012/cloud-inventory/plugin/provider"
	runv2 "google.golang.org/api/run/v2"
)

// TestCloudRunSurvivesTheBoundary measures what a host can still read once the
// provider's own struct has crossed the wire as bytes.
func TestCloudRunSurvivesTheBoundary(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, exists: true}
	session := start(t, fake)

	got, err := session.Get(context.Background(), gcp.ServiceID("demo", "us-central1", "api"), true)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if len(got.FullData) == 0 {
		t.Fatal("asked for full data and got none")
	}

	// The host has no Cloud Run type of its own, so it supplies one. Here that
	// is the SDK's struct, which only works because a host may link it.
	var view runv2.GoogleCloudRunV2Service
	if err := got.View(&view); err != nil {
		t.Fatalf("decoding the full data: %v", err)
	}

	want := fake.body()
	if view.Uri != want.Uri || view.LaunchStage != want.LaunchStage {
		t.Fatalf("scalars did not survive: %+v", view)
	}
	if view.Template == nil || view.Template.ServiceAccount != want.Template.ServiceAccount {
		t.Fatal("the nested revision template did not survive")
	}
	if view.Template.Containers[0].Image != want.Template.Containers[0].Image {
		t.Fatal("the container image did not survive")
	}

	t.Logf("cloud run full data: %d bytes for %d set fields", len(got.FullData), 6)
}

// fakeEC2 answers for one instance, which is all this measurement needs.
type fakeEC2 struct{ instance ec2types.Instance }

func (f fakeEC2) DescribeInstances(context.Context, *ec2sdk.DescribeInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.DescribeInstancesOutput, error) {
	return &ec2sdk.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{f.instance}}},
	}, nil
}
func (f fakeEC2) DescribeRegions(context.Context, *ec2sdk.DescribeRegionsInput, ...func(*ec2sdk.Options)) (*ec2sdk.DescribeRegionsOutput, error) {
	return &ec2sdk.DescribeRegionsOutput{Regions: []ec2types.Region{{RegionName: awssdk.String("eu-west-1")}}}, nil
}
func (f fakeEC2) TerminateInstances(context.Context, *ec2sdk.TerminateInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.TerminateInstancesOutput, error) {
	return &ec2sdk.TerminateInstancesOutput{}, nil
}
func (f fakeEC2) RunInstances(context.Context, *ec2sdk.RunInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.RunInstancesOutput, error) {
	return &ec2sdk.RunInstancesOutput{}, nil
}

// TestEC2CrossesTheBoundaryAtAPrice measures the same thing for a provider
// whose SDK types were never meant to be serialised.
func TestEC2CrossesTheBoundaryAtAPrice(t *testing.T) {
	launched := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	original := ec2types.Instance{
		InstanceId:   awssdk.String("i-0abc123"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		LaunchTime:   &launched,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:    &ec2types.Placement{AvailabilityZone: awssdk.String("eu-west-1a")},
		Tags:         []ec2types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String("web")}},
	}

	instances := awsinv.NewEC2InstancesWithClients(func(string) awsinv.EC2API {
		return fakeEC2{instance: original}
	})

	// The same server a provider binary serves, driven in process: the
	// serialisation is what is being measured, not the transport.
	server := provider.Server(provider.Resource[awsinv.EC2Instance, awsinv.EC2InstanceSpec, awsinv.Scope]{
		Lister: instances, Getter: instances,
		Account: func(context.Context) (string, error) { return "1234", nil },
	})
	if _, err := server.Configure(context.Background(), &pb.ConfigureRequest{ScopeJson: "{}"}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	answer, err := server.Get(context.Background(), &pb.GetRequest{
		Id: string(awsinv.InstanceID("eu-west-1", "i-0abc123")), WithFullData: true,
	})
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	body := answer.GetResource().GetFullData()

	var view ec2types.Instance
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decoding the full data: %v", err)
	}

	// What survives.
	if !reflect.DeepEqual(view.InstanceId, original.InstanceId) ||
		view.InstanceType != original.InstanceType ||
		view.State.Name != original.State.Name ||
		!view.LaunchTime.Equal(launched) {
		t.Fatalf("the fields a caller reads did not survive: %+v", view)
	}

	// What it costs. The AWS SDK's types carry no json tags and no omitempty,
	// so every field of the struct is on the wire whether it was set or not.
	var loose map[string]any
	if err := json.Unmarshal(body, &loose); err != nil {
		t.Fatalf("reading the body back: %v", err)
	}
	set := 0
	for _, v := range loose {
		if v != nil {
			set++
		}
	}
	t.Logf("ec2 full data: %d bytes, %d json fields, %d of them non-null", len(body), len(loose), set)

	if len(loose) < 50 {
		t.Fatalf("expected the whole struct on the wire, got %d fields", len(loose))
	}
	// Two thirds of the fields on the wire are nulls, which is what no
	// omitempty costs on a struct the SDK never meant to serialise.
	if set*2 > len(loose) {
		t.Fatalf("expected most fields to be null, %d of %d were set", set, len(loose))
	}
}

// TestTheHostStillNeedsTheCloudSdkToReadFullData is the finding the typed view
// idea turns on: the bytes decode into something, but only a type that models
// the cloud, which a host either links from the SDK or has generated for it.
func TestTheHostStillNeedsTheCloudSdkToReadFullData(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, exists: true}
	session := start(t, fake)

	got, err := session.Get(context.Background(), gcp.ServiceID("demo", "us-central1", "api"), true)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}

	// Without such a type the host has a map, and every field access is a
	// string lookup with no checking.
	var loose map[string]any
	if err := got.View(&loose); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	template, ok := loose["template"].(map[string]any)
	if !ok {
		t.Fatalf("the nested template is not reachable without a type: %v", loose)
	}
	if _, ok := template["serviceAccount"].(string); !ok {
		t.Fatal("the service account is not reachable without a type")
	}

	// And the five common fields need none of that, which is the part of the
	// contract that is genuinely provider-neutral.
	if got.DisplayName != "api" || got.Tags["env"] != "demo" {
		t.Fatalf("the common fields did not survive: %+v", got)
	}
	if got.ID == "" || got.Type != "cloud-run-service" {
		t.Fatal("the identity did not survive")
	}
	_ = cloudinv.FaultUnknown
}
