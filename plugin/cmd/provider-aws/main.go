// Command provider-aws serves AWS resource types over the inventory contract.
// A host starts it as a subprocess and never links an AWS SDK.
package main

import (
	"context"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	cloudinv "github.com/james00012/cloud-inventory"
	"github.com/james00012/cloud-inventory/aws"
	"github.com/james00012/cloud-inventory/plugin/provider"
)

func main() {
	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws credentials:", err)
		os.Exit(1)
	}

	instances := aws.NewEC2Instances(cfg)
	identity := sts.NewFromConfig(cfg)

	provider.Serve(provider.Server(provider.Resource[aws.EC2Instance, aws.EC2InstanceSpec, aws.Scope]{
		Lister:  instances,
		Getter:  instances,
		Deleter: instances,
		Creator: instances,

		// The account a session is bound to is the one the credentials answer
		// for, which is what stops a session pointed at the wrong one.
		Account: func(ctx context.Context) (string, error) {
			who, err := identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return "", err
			}
			if who.Account == nil {
				return "", nil
			}
			return *who.Account, nil
		},

		Fault: func(error) cloudinv.Fault { return cloudinv.FaultUnknown },
	}))
}
