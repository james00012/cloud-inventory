package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	cloudinv "github.com/james00012/cloud-inventory"
)

// EC2Instance is one virtual machine. It embeds the API's own type, so every
// field EC2 reports is reachable without this library modelling it, and adds
// only what the API does not give directly.
type EC2Instance struct {
	*ec2types.Instance

	// ID is the canonical identifier, region and instance together, because an
	// instance ID is unique per account while the API that serves it is
	// regional.
	ID cloudinv.ID

	// Region is not on the API's answer at all, so this library carries it.
	Region string
}

func (i EC2Instance) ResourceID() cloudinv.ID { return i.ID }
func (i EC2Instance) ResourceType() string    { return "ec2-instance" }

// CreatedAt reads LaunchTime, since EC2 reports no separate creation time.
func (i EC2Instance) CreatedAt() time.Time {
	if i.Instance == nil || i.LaunchTime == nil {
		return time.Time{}
	}
	return *i.LaunchTime
}

// DisplayName reads the Name tag, because an EC2 instance has no name field.
func (i EC2Instance) DisplayName() string { return i.Tags()["Name"] }

// Tags flattens the API's tag list into the map every provider shares.
func (i EC2Instance) Tags() map[string]string {
	flat := map[string]string{}
	if i.Instance == nil {
		return flat
	}
	for _, tag := range i.Instance.Tags {
		if tag.Key != nil && tag.Value != nil {
			flat[*tag.Key] = *tag.Value
		}
	}
	return flat
}

var _ cloudinv.Resource = EC2Instance{}

// EC2InstanceSpec is what a caller supplies to make an instance. It is the
// API's own request input, so every field EC2 accepts is available, plus the
// region, which the API takes from the client rather than the request.
type EC2InstanceSpec struct {
	*ec2.RunInstancesInput

	Region string
}

// ec2API is the part of the EC2 API this type uses.
type ec2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, opts ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

// EC2Instances is the resource type's four calls.
type EC2Instances struct {
	// in makes the regional client. A test hands over a fake; a run hands over
	// one built from the scope's config.
	in func(scope Scope, region string) ec2API
}

func NewEC2Instances() *EC2Instances {
	return &EC2Instances{in: func(scope Scope, region string) ec2API {
		cfg := scope.Config.Copy()
		cfg.Region = region
		return ec2.NewFromConfig(cfg)
	}}
}

// NewEC2InstancesWithClient takes a client a caller built, which is how a test
// drives this without AWS.
func NewEC2InstancesWithClient(client ec2API) *EC2Instances {
	return &EC2Instances{in: func(Scope, string) ec2API { return client }}
}

// List fans out over the regions the scope asks for. EC2 has no cross-region
// list, so this loop exists for every AWS resource type and lives once.
func (e *EC2Instances) List(ctx context.Context, scope Scope) ([]EC2Instance, cloudinv.Incomplete, error) {
	regions, err := e.regions(ctx, scope)
	if err != nil {
		return nil, cloudinv.Incomplete{}, fmt.Errorf("listing enabled regions: %w", err)
	}

	var asked []string
	for _, region := range regions {
		if scope.wants(region) {
			asked = append(asked, region)
		}
	}

	return cloudinv.FanOut(ctx, asked, faultOf, func(ctx context.Context, region string) ([]EC2Instance, error) {
		return e.listIn(ctx, scope, region)
	})
}

func (e *EC2Instances) Get(ctx context.Context, id cloudinv.ID) (EC2Instance, error) {
	region, instanceID, err := splitID(id)
	if err != nil {
		return EC2Instance{}, err
	}

	answer, err := e.in(Scope{}, region).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return EC2Instance{}, err
	}
	for _, reservation := range answer.Reservations {
		for i := range reservation.Instances {
			return instanceFrom(&reservation.Instances[i], region), nil
		}
	}
	return EC2Instance{}, fmt.Errorf("no instance %s in %s", instanceID, region)
}

func (e *EC2Instances) Delete(ctx context.Context, id cloudinv.ID) error {
	region, instanceID, err := splitID(id)
	if err != nil {
		return err
	}

	_, err = e.in(Scope{}, region).TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil && faultOf(err) == cloudinv.FaultNotFound {
		// An instance already gone is the state the caller wanted.
		return nil
	}
	return err
}

func (e *EC2Instances) Create(ctx context.Context, scope Scope, spec EC2InstanceSpec) (EC2Instance, error) {
	request := spec.RunInstancesInput
	if request.MinCount == nil {
		request.MinCount = awssdk.Int32(1)
	}
	if request.MaxCount == nil {
		request.MaxCount = awssdk.Int32(1)
	}

	answer, err := e.in(scope, spec.Region).RunInstances(ctx, request)
	if err != nil {
		return EC2Instance{}, err
	}
	if len(answer.Instances) == 0 {
		return EC2Instance{}, fmt.Errorf("the run returned no instance")
	}
	return instanceFrom(&answer.Instances[0], spec.Region), nil
}

func (e *EC2Instances) regions(ctx context.Context, scope Scope) ([]string, error) {
	answer, err := e.in(scope, "us-east-1").DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}

	var found []string
	for _, region := range answer.Regions {
		if region.RegionName != nil {
			found = append(found, *region.RegionName)
		}
	}
	return found, nil
}

func (e *EC2Instances) listIn(ctx context.Context, scope Scope, region string) ([]EC2Instance, error) {
	client := e.in(scope, region)

	var found []EC2Instance
	pager := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, reservation := range page.Reservations {
			for i := range reservation.Instances {
				found = append(found, instanceFrom(&reservation.Instances[i], region))
			}
		}
	}
	return found, nil
}

func instanceFrom(instance *ec2types.Instance, region string) EC2Instance {
	built := EC2Instance{Instance: instance, Region: region}
	if instance.InstanceId != nil {
		built.ID = cloudinv.ID(region + "/" + *instance.InstanceId)
	}
	return built
}

// splitID reads the region and the instance out of an id. An EC2 instance id is
// unique per account but the API that answers for it is regional, so the region
// is part of the canonical id.
func splitID(id cloudinv.ID) (region, instanceID string, err error) {
	at := strings.Index(string(id), "/")
	if at < 0 {
		return "", "", fmt.Errorf("not an instance id: %s", id)
	}
	return string(id)[:at], string(id)[at+1:], nil
}

var (
	_ cloudinv.Lister[EC2Instance, Scope]                   = (*EC2Instances)(nil)
	_ cloudinv.Getter[EC2Instance]                          = (*EC2Instances)(nil)
	_ cloudinv.Deleter                                      = (*EC2Instances)(nil)
	_ cloudinv.Creator[EC2Instance, EC2InstanceSpec, Scope] = (*EC2Instances)(nil)
)
