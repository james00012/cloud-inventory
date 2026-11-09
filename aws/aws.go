// Package aws holds the AWS half of the inventory: one file per resource type,
// each a typed struct and the four calls against it.
package aws

import (
	"errors"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	cloudinv "github.com/james00012/cloud-inventory"
)

// Scope is where to look in AWS. Regions empty means every region the account
// has enabled: naming them is how a caller narrows the fan-out.
type Scope struct {
	// Config carries the credentials and the default region. The library does
	// not build it, because a consumer has its own opinion about how a session
	// is made and which profile it uses.
	Config awssdk.Config

	Regions []string
}

func (s Scope) wants(region string) bool {
	if len(s.Regions) == 0 {
		return true
	}
	for _, asked := range s.Regions {
		if strings.EqualFold(asked, region) {
			return true
		}
	}
	return false
}

// faultOf reads an AWS error into the shape every provider shares, so a
// consumer never matches on an SDK's own error types or on a string.
func faultOf(err error) cloudinv.Fault {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch code := apiErr.ErrorCode(); {
		case code == "RequestLimitExceeded", code == "Throttling", code == "ThrottlingException":
			return cloudinv.FaultThrottle
		case code == "UnauthorizedOperation", code == "AccessDenied", code == "AccessDeniedException":
			return cloudinv.FaultDenied
		case strings.HasSuffix(code, ".NotFound"), strings.HasSuffix(code, "NotFoundException"):
			return cloudinv.FaultNotFound
		case code == "OptInRequired", code == "SubscriptionRequiredException":
			return cloudinv.FaultAPIDisabled
		}
	}
	return cloudinv.FaultUnknown
}
