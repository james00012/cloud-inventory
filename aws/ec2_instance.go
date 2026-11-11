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

// EC2API is the part of the EC2 API this type uses. It is exported so a
// consumer can drive this type from a test without AWS credentials, which is
// what a test library wrapping this needs.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, opts ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

// EC2Instances is the resource type's four calls.
type EC2Instances struct {
	// in makes the client for one region. Keying it by region alone is what
	// stops a delete reaching the wrong region with the right instance id.
	in func(region string) EC2API
}

// NewEC2Instances binds the type to one account's credentials. Get and Delete
// take an id rather than a scope, so the credentials cannot arrive per call.
func NewEC2Instances(cfg awssdk.Config) *EC2Instances {
	return &EC2Instances{in: func(region string) EC2API {
		regional := cfg.Copy()
		regional.Region = region
		return ec2.NewFromConfig(regional)
	}}
}

// NewEC2InstancesWithClients takes a client per region, which is how a test
// drives this without AWS. It is a function of the region rather than one
// client, so a fake answers for the region it was actually asked about.
func NewEC2InstancesWithClients(byRegion func(region string) EC2API) *EC2Instances {
	return &EC2Instances{in: byRegion}
}

// List fans out over the regions the scope asks for. EC2 has no cross-region
// list, so this loop exists for every AWS resource type and lives once.
func (e *EC2Instances) List(ctx context.Context, scope Scope) ([]EC2Instance, cloudinv.Incomplete, error) {
	regions, err := e.regions(ctx)
	if err != nil {
		return nil, cloudinv.Incomplete{}, fmt.Errorf("listing enabled regions: %w", err)
	}

	offered := make(map[string]bool, len(regions))
	var asked []string
	for _, region := range regions {
		offered[strings.ToLower(region)] = true
		if scope.wants(region) {
			asked = append(asked, region)
		}
	}

	found, incomplete, err := cloudinv.FanOut(ctx, asked, faultOf, func(ctx context.Context, region string) ([]EC2Instance, error) {
		return e.listIn(ctx, region)
	})

	// A region the caller named that the account does not offer is a question
	// nobody answered, not an empty region.
	if missing := cloudinv.Unoffered(scope.Regions, offered); len(missing) > 0 {
		incomplete.Gaps = append(incomplete.Gaps, missing...)
		incomplete.Asked += len(missing)
	}
	return found, incomplete, err
}

func (e *EC2Instances) Get(ctx context.Context, id cloudinv.ID) (EC2Instance, error) {
	region, instanceID, err := splitID(id)
	if err != nil {
		return EC2Instance{}, err
	}

	answer, err := e.in(region).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
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

	client := e.in(region)
	_, err = client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// An instance already gone is the state the caller wanted. The client
		// is built from the id's own region, so a not-found here is the
		// account's answer rather than a lookup in the wrong region.
		if faultOf(err) == cloudinv.FaultNotFound {
			return nil
		}
		return err
	}

	// The contract says a delete does not return until the resource is gone,
	// and TerminateInstances returns while the instance is still shutting down.
	waiter := ec2.NewInstanceTerminatedWaiter(client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}}, terminateWait); err != nil {
		// EC2 stops answering for an instance once it is reaped, which can
		// happen between the terminate and the first poll. That is the state
		// the caller wanted, not a failed delete.
		if faultOf(err) == cloudinv.FaultNotFound {
			return nil
		}
		return fmt.Errorf("waiting for %s in %s to terminate: %w", instanceID, region, err)
	}
	return nil
}

// terminateWait bounds the wait for an instance to reach terminated, so a stuck
// instance fails the delete rather than holding the run forever.
const terminateWait = 10 * time.Minute

// Create takes a scope it does not read: the credentials come from the
// constructor and the region from the spec. It stays in the signature because
// Creator is one interface across providers, and GCP reads the project from it.
func (e *EC2Instances) Create(ctx context.Context, scope Scope, spec EC2InstanceSpec) (EC2Instance, error) {
	if spec.RunInstancesInput == nil {
		return EC2Instance{}, fmt.Errorf("the spec carries no RunInstancesInput")
	}

	// A copy, because the caller may reuse or share the spec and this fills in
	// the counts.
	request := *spec.RunInstancesInput
	if request.MinCount == nil {
		request.MinCount = awssdk.Int32(1)
	}
	if request.MaxCount == nil {
		request.MaxCount = awssdk.Int32(1)
	}

	answer, err := e.in(spec.Region).RunInstances(ctx, &request)
	if err != nil {
		return EC2Instance{}, err
	}
	if len(answer.Instances) == 0 {
		return EC2Instance{}, fmt.Errorf("the run returned no instance")
	}
	return instanceFrom(&answer.Instances[0], spec.Region), nil
}

// regionsFrom is where DescribeRegions is asked. It answers for the whole
// account, so any enabled region serves.
const regionsFrom = "us-east-1"

func (e *EC2Instances) regions(ctx context.Context) ([]string, error) {
	answer, err := e.in(regionsFrom).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
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

func (e *EC2Instances) listIn(ctx context.Context, region string) ([]EC2Instance, error) {
	client := e.in(region)

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
		built.ID = InstanceID(region, *instance.InstanceId)
	}
	return built
}

// InstanceID builds the canonical id for an instance from the two things a
// caller knows, so nobody has to learn the format. Get and Delete take this,
// and List and Create return it.
func InstanceID(region, instanceID string) cloudinv.ID {
	return cloudinv.ID(region + "/" + instanceID)
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
