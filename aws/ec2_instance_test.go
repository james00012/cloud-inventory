package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEC2 answers with the regions and instances a test names, and records the
// regions it was asked about.
type fakeEC2 struct {
	regions   []string
	instances map[string][]string
	refuse    map[string]error

	asked      []string
	terminated []string
	region     string
}

func (f *fakeEC2) DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	out := &ec2.DescribeRegionsOutput{}
	for _, region := range f.regions {
		out.Regions = append(out.Regions, ec2types.Region{RegionName: awssdk.String(region)})
	}
	return out, nil
}

func (f *fakeEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.asked = append(f.asked, f.region)
	if err, refused := f.refuse[f.region]; refused {
		return nil, err
	}

	var instances []ec2types.Instance
	for _, id := range f.instances[f.region] {
		launched := time.Now()
		instances = append(instances, ec2types.Instance{
			InstanceId: awssdk.String(id),
			LaunchTime: &launched,
			State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Tags:       []ec2types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String(id + "-name")}},
		})
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: instances}},
	}, nil
}

func (f *fakeEC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.terminated = append(f.terminated, in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{
		InstanceId: awssdk.String("i-new"),
		Tags:       in.TagSpecifications[0].Tags,
	}}}, nil
}

// regionally builds the type over a fake whose answers depend on the region the
// library asked for, which is what the fan-out has to get right.
func regionally(fake *fakeEC2) *EC2Instances {
	return &EC2Instances{in: func(scope Scope, region string) ec2API {
		fake.region = region
		return fake
	}}
}

// A run narrowed to one region asks that region alone. AWS has no cross-region
// list, so this is the same fan-out the GCP side uses, which is why it lives in
// the shared core.
func TestListAsksOnlyTheRegionsTheScopeNames(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{
		regions:   []string{"us-east-1", "us-west-2", "eu-west-1"},
		instances: map[string][]string{"us-east-1": {"i-1"}, "us-west-2": {"i-2"}, "eu-west-1": {"i-3"}},
	}

	found, incomplete, err := regionally(fake).List(context.Background(), Scope{Regions: []string{"us-west-2"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"us-west-2"}, fake.asked)
	require.Len(t, found, 1)
	assert.Equal(t, cloudinv.ID("us-west-2/i-2"), found[0].ID)

	// The name comes from a tag here and from the resource name on GCP, which
	// is why the shared contract is methods rather than fields.
	assert.Equal(t, "i-2-name", found[0].DisplayName())
	assert.False(t, incomplete.Any())

	// The API's own fields stay reachable.
	assert.Equal(t, ec2types.InstanceStateNameRunning, found[0].State.Name)
}

// One region refusing costs that region alone, and the caller is told which.
func TestListReportsTheRegionItCouldNotRead(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{
		regions:   []string{"us-east-1", "us-west-2"},
		instances: map[string][]string{"us-west-2": {"i-2"}},
		refuse:    map[string]error{"us-east-1": errors.New("UnauthorizedOperation")},
	}

	found, incomplete, err := regionally(fake).List(context.Background(), Scope{})
	require.NoError(t, err)

	assert.Len(t, found, 1)
	assert.True(t, incomplete.Any())
	assert.False(t, incomplete.Total())
	assert.Equal(t, 2, incomplete.Asked)
	assert.Contains(t, incomplete.String(), "us-east-1")
}

// The region is part of the canonical id, because an instance id is unique per
// account while the API that answers for it is regional.
func TestDeleteReadsTheRegionOutOfTheId(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{}
	instances := regionally(fake)

	require.NoError(t, instances.Delete(context.Background(), cloudinv.ID("eu-west-1/i-9")))
	assert.Equal(t, []string{"i-9"}, fake.terminated)
	assert.Equal(t, "eu-west-1", fake.region)

	_, _, err := splitID("i-9")
	assert.Error(t, err)
}

// The spec is the API's own request input, so a caller reaches any field EC2
// accepts without this library modelling it.
func TestCreateSendsTheApisOwnRequest(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{}
	made, err := regionally(fake).Create(context.Background(), Scope{}, EC2InstanceSpec{
		RunInstancesInput: &ec2.RunInstancesInput{
			ImageId:      awssdk.String("ami-1"),
			InstanceType: ec2types.InstanceTypeT3Micro,
			TagSpecifications: []ec2types.TagSpecification{{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: awssdk.String("Name"), Value: awssdk.String("web")},
					{Key: awssdk.String("env"), Value: awssdk.String("test")},
				},
			}},
		},
		Region: "us-east-1",
	})
	require.NoError(t, err)

	assert.Equal(t, cloudinv.ID("us-east-1/i-new"), made.ID)
	assert.Equal(t, "web", made.DisplayName())
	assert.Equal(t, "test", made.Tags()["env"])
	assert.Equal(t, "i-new", *made.InstanceId)
}
