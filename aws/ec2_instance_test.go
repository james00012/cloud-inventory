package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
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

	// A lookup by id is the delete waiter asking whether the instance is gone.
	if len(in.InstanceIds) > 0 {
		return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
			InstanceId: awssdk.String(in.InstanceIds[0]),
			State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated},
		}}}}}, nil
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
	made := ec2types.Instance{InstanceId: awssdk.String("i-new")}
	for _, spec := range in.TagSpecifications {
		made.Tags = append(made.Tags, spec.Tags...)
	}
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{made}}, nil
}

// regionally gives every region the same fake and records which region was
// asked for. A test that needs a wrong region to look different builds its own
// client per region, as TestDeleteAsksTheRegionNamedInTheId does.
func regionally(fake *fakeEC2) *EC2Instances {
	return NewEC2InstancesWithClients(func(region string) ec2API {
		fake.region = region
		return fake
	})
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

// A delete must reach the region in the id. Pointing every region at one
// client used to hide this: the wrong region answers NotFound, the already-gone
// branch fires, and a running instance is reported deleted.
func TestDeleteAsksTheRegionNamedInTheId(t *testing.T) {
	t.Parallel()

	asked := map[string][]string{}
	instances := NewEC2InstancesWithClients(func(region string) ec2API {
		return &recordingEC2{region: region, terminated: asked}
	})

	require.NoError(t, instances.Delete(context.Background(), cloudinv.ID("eu-west-1/i-9")))
	assert.Equal(t, map[string][]string{"eu-west-1": {"i-9"}}, asked)

	// An id with no region is not an id.
	_, _, err := splitID("i-9")
	assert.Error(t, err)
}

// A region the caller named that the account does not offer is a gap, not an
// empty region, or a misspelled region reads as a clean account.
func TestListReportsARegionTheAccountDoesNotOffer(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{regions: []string{"us-east-1", "us-west-2"}}

	found, incomplete, err := regionally(fake).List(context.Background(), Scope{Regions: []string{"us-east-l"}})
	require.NoError(t, err)

	assert.Empty(t, found)
	assert.Contains(t, incomplete.String(), "us-east-l")

	// Total is what a consumer branches on, so a run narrowed to one
	// misspelled region has to read as a run that learned nothing.
	assert.True(t, incomplete.Total())
}

// The codes EC2 actually returns have to land somewhere a consumer can act on.
func TestFaultOfReadsTheCodesEc2Returns(t *testing.T) {
	t.Parallel()

	assert.Equal(t, cloudinv.FaultDenied, faultOf(&smithy.GenericAPIError{Code: "AuthFailure"}))
	assert.Equal(t, cloudinv.FaultDenied, faultOf(&smithy.GenericAPIError{Code: "UnauthorizedOperation"}))
	assert.Equal(t, cloudinv.FaultThrottle, faultOf(&smithy.GenericAPIError{Code: "RequestLimitExceeded"}))
	assert.Equal(t, cloudinv.FaultNotFound, faultOf(&smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}))
	assert.Equal(t, cloudinv.FaultAPIDisabled, faultOf(&smithy.GenericAPIError{Code: "OptInRequired"}))

	// Wrapped the way the SDK hands it up through an operation.
	wrapped := &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeInstances",
		Err: &smithy.GenericAPIError{Code: "AuthFailure"}}
	assert.Equal(t, cloudinv.FaultDenied, faultOf(wrapped))
}

// The caller may reuse or share the spec, so Create fills its counts on a copy.
func TestCreateLeavesTheCallersRequestAlone(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{}
	request := &ec2.RunInstancesInput{ImageId: awssdk.String("ami-1")}

	_, err := regionally(fake).Create(context.Background(), Scope{}, EC2InstanceSpec{
		RunInstancesInput: request, Region: "us-east-1",
	})
	require.NoError(t, err)

	assert.Nil(t, request.MinCount, "Create wrote into the caller's request")
	assert.Nil(t, request.MaxCount)
}

func TestCreateRefusesASpecWithNoRequest(t *testing.T) {
	t.Parallel()

	_, err := regionally(&fakeEC2{}).Create(context.Background(), Scope{}, EC2InstanceSpec{Region: "us-east-1"})
	require.Error(t, err)
}

// recordingEC2 answers a terminate for whichever region built it, and reports
// the instance terminated so the waiter returns.
type recordingEC2 struct {
	region     string
	terminated map[string][]string
}

func (r *recordingEC2) DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	return &ec2.DescribeRegionsOutput{}, nil
}

func (r *recordingEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: awssdk.String(in.InstanceIds[0]),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated},
	}}}}}, nil
}

func (r *recordingEC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	r.terminated[r.region] = append(r.terminated[r.region], in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (r *recordingEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	return &ec2.RunInstancesOutput{}, nil
}

// EC2 stops answering for an instance once it is reaped, which can land
// between the terminate and the waiter's first poll. A delete that already
// happened must not be reported as a failure.
func TestDeleteTreatsAReapedInstanceAsGone(t *testing.T) {
	t.Parallel()

	instances := NewEC2InstancesWithClients(func(region string) ec2API {
		return &reapedEC2{}
	})

	require.NoError(t, instances.Delete(context.Background(), cloudinv.ID("eu-west-1/i-9")))
}

// A delete that cannot be confirmed names the instance and the region, since
// the waiter's own error names neither.
func TestDeleteNamesWhatItCouldNotConfirm(t *testing.T) {
	t.Parallel()

	instances := NewEC2InstancesWithClients(func(region string) ec2API {
		return &refusingEC2{}
	})

	err := instances.Delete(context.Background(), cloudinv.ID("eu-west-1/i-9"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "i-9")
	assert.Contains(t, err.Error(), "eu-west-1")
}

// reapedEC2 accepts the terminate, then answers as EC2 does once the instance
// is gone from its records.
type reapedEC2 struct{ fakeEC2 }

func (r *reapedEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "does not exist"}
}

// refusingEC2 accepts the terminate but will not say whether it finished.
type refusingEC2 struct{ fakeEC2 }

func (r *refusingEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "not allowed"}
}

// An exclusion is read inside the scope, so an excluded region is never asked
// and never counted among the regions the run depends on.
func TestListNeverAsksAnExcludedRegion(t *testing.T) {
	t.Parallel()

	fake := &fakeEC2{
		regions:   []string{"us-east-1", "us-west-2"},
		instances: map[string][]string{"us-east-1": {"i-1"}, "us-west-2": {"i-2"}},
	}

	found, incomplete, err := regionally(fake).List(context.Background(),
		Scope{ExcludeRegions: []string{"us-west-2"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"us-east-1"}, fake.asked)
	assert.Len(t, found, 1)
	assert.Equal(t, 1, incomplete.Asked)
}
