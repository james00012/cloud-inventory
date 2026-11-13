package core_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	awsinv "github.com/james00012/cloud-inventory/aws"
	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/core"
	"google.golang.org/api/option"
	runv2 "google.golang.org/api/run/v2"
)

// oneInstance answers for a single instance, which is all these measurements
// need. It is a narrow adapter over the SDK rather than a double of it.
type oneInstance struct{ instance ec2types.Instance }

func (o oneInstance) DescribeInstances(context.Context, *ec2sdk.DescribeInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.DescribeInstancesOutput, error) {
	return &ec2sdk.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{o.instance}}},
	}, nil
}

func (o oneInstance) DescribeRegions(context.Context, *ec2sdk.DescribeRegionsInput, ...func(*ec2sdk.Options)) (*ec2sdk.DescribeRegionsOutput, error) {
	return &ec2sdk.DescribeRegionsOutput{Regions: []ec2types.Region{{RegionName: awssdk.String("eu-west-1")}}}, nil
}

func (o oneInstance) TerminateInstances(context.Context, *ec2sdk.TerminateInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.TerminateInstancesOutput, error) {
	return &ec2sdk.TerminateInstancesOutput{}, nil
}

func (o oneInstance) RunInstances(context.Context, *ec2sdk.RunInstancesInput, ...func(*ec2sdk.Options)) (*ec2sdk.RunInstancesOutput, error) {
	return &ec2sdk.RunInstancesOutput{}, nil
}

var launched = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func instance() ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   awssdk.String("i-0abc123"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		LaunchTime:   &launched,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:    &ec2types.Placement{AvailabilityZone: awssdk.String("eu-west-1a")},
		Tags:         []ec2types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String("web")}},
	}
}

// read configures a session over one EC2 instance and returns it.
func read(t *testing.T, one ec2types.Instance) core.Resource {
	t.Helper()

	instances := awsinv.NewEC2InstancesWithClients(func(string) awsinv.EC2API {
		return oneInstance{instance: one}
	})
	service := core.New(core.Spec[awsinv.EC2Instance, awsinv.EC2InstanceSpec, awsinv.Scope]{
		Lister:  instances,
		Getter:  instances,
		Account: func(context.Context) (string, error) { return "1234", nil },
	})
	if _, _, err := service.Configure(context.Background(), []core.Feature{core.Read}, core.Session{Caller: "test"}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	got, err := service.Get(context.Background(), awsinv.InstanceID("eu-west-1", "i-0abc123"), true)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	return got
}

// TestAnSdkStructSurvivesTheContract pins that a caller still reads the fields
// it would have read from the SDK, once the provider's own struct has been
// carried as bytes.
func TestAnSdkStructSurvivesTheContract(t *testing.T) {
	original := instance()
	got := read(t, original)

	var view ec2types.Instance
	if err := json.Unmarshal(got.FullData, &view); err != nil {
		t.Fatalf("decoding the full data: %v", err)
	}

	if awssdk.ToString(view.InstanceId) != awssdk.ToString(original.InstanceId) {
		t.Fatalf("instance id did not survive: %q", awssdk.ToString(view.InstanceId))
	}
	if view.InstanceType != original.InstanceType {
		t.Fatalf("instance type did not survive: %q", view.InstanceType)
	}
	if view.State == nil || view.State.Name != original.State.Name {
		t.Fatalf("state did not survive: %+v", view.State)
	}
	if view.LaunchTime == nil || !view.LaunchTime.Equal(launched) {
		t.Fatalf("launch time did not survive: %v", view.LaunchTime)
	}
}

// TestAnSdkStructCostsMostlyNulls is what carrying one costs. The AWS SDK's
// types declare no omitempty, so every field is on the wire whether it was set
// or not, which is the price of full data a generated view would not pay.
func TestAnSdkStructCostsMostlyNulls(t *testing.T) {
	got := read(t, instance())

	var loose map[string]any
	if err := json.Unmarshal(got.FullData, &loose); err != nil {
		t.Fatalf("reading the body back: %v", err)
	}

	set := 0
	for _, value := range loose {
		if value != nil {
			set++
		}
	}
	t.Logf("%d bytes, %d fields, %d of them set", len(got.FullData), len(loose), set)

	if len(loose) < 50 {
		t.Fatalf("expected the whole struct on the wire, got %d fields", len(loose))
	}
	if set*2 > len(loose) {
		t.Fatalf("expected most fields null, %d of %d were set", set, len(loose))
	}
}

// TestFullDataNeedsATypeButTheCommonFieldsDoNot is the finding the typed view
// idea turns on. Reading full data needs a type that models the cloud, which a
// caller either links from the SDK or has generated. The five common fields
// need no such type, and that is the part of the contract that is neutral.
func TestFullDataNeedsATypeButTheCommonFieldsDoNot(t *testing.T) {
	got := read(t, instance())

	// Without a type a caller has a map, where every field is a string lookup
	// the compiler cannot check.
	var loose map[string]any
	if err := json.Unmarshal(got.FullData, &loose); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	placement, ok := loose["Placement"].(map[string]any)
	if !ok {
		t.Fatalf("the nested placement is not reachable without a type: %v", loose["Placement"])
	}
	if _, ok := placement["AvailabilityZone"].(string); !ok {
		t.Fatal("the availability zone is not reachable without a type")
	}

	if got.ID == "" || got.Type != "ec2-instance" {
		t.Fatalf("the identity did not survive: %q %q", got.ID, got.Type)
	}
	if got.DisplayName != "web" || got.Tags["Name"] != "web" {
		t.Fatalf("the common fields did not survive: %+v", got)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(launched) {
		t.Fatalf("the creation time did not survive: %v", got.CreatedAt)
	}
}

// cloudRunService answers a get for one service, with the fields a caller
// would actually read back.
func cloudRunService(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&runv2.GoogleCloudRunV2Service{
		Name:        "projects/demo/locations/us-central1/services/api",
		Uri:         "https://api-demo.a.run.app",
		LaunchStage: "GA",
		Labels:      map[string]string{"env": "demo"},
		CreateTime:  launched.Format(time.RFC3339),
		Template: &runv2.GoogleCloudRunV2RevisionTemplate{
			ServiceAccount: "runner@demo.iam.gserviceaccount.com",
			Containers:     []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
		},
	})
}

// TestAStructWrittenForTheWireCostsFarLess is the contrast that decides what
// full data costs. Google's generated types declare omitempty, so only the
// fields that were set cross, where the AWS SDK's types send all 61.
func TestAStructWrittenForTheWireCostsFarLess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(cloudRunService))
	t.Cleanup(server.Close)

	api, err := runv2.NewService(context.Background(),
		option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("standing up Cloud Run: %v", err)
	}
	services := gcp.NewCloudRunServicesWithClients(nil, api)

	service := core.New(core.Spec[gcp.CloudRunService, gcp.CloudRunServiceSpec, gcp.Scope]{
		Getter:  services,
		Account: func(context.Context) (string, error) { return "demo", nil },
	})
	if _, _, err := service.Configure(context.Background(), []core.Feature{core.Read}, core.Session{Caller: "test"}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	got, err := service.Get(context.Background(), gcp.ServiceID("demo", "us-central1", "api"), true)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}

	var loose map[string]any
	if err := json.Unmarshal(got.FullData, &loose); err != nil {
		t.Fatalf("reading the body back: %v", err)
	}
	t.Logf("%d bytes, %d fields", len(got.FullData), len(loose))

	// Every field present was set, which is what omitempty buys.
	for name, value := range loose {
		if value == nil {
			t.Fatalf("%q crossed as null, so the struct does not declare omitempty", name)
		}
	}
	if len(loose) > 12 {
		t.Fatalf("expected only the fields that were set, got %d", len(loose))
	}
}
