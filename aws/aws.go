// Package aws holds the AWS half of the inventory: one file per resource type,
// each a typed struct and the four calls against it.
package aws

import (
	"errors"
	"strings"

	"github.com/aws/smithy-go"
	cloudinv "github.com/james00012/cloud-inventory"
)

// Scope is where to look in AWS: which regions, and nothing else. The
// credentials come from the constructor, because Get and Delete take an id
// rather than a scope and still have to reach the account.
type Scope struct {
	// Regions empty means every region the account has enabled.
	Regions        []string
	ExcludeRegions []string
}

// wants reports whether the scope asks for this region. An exclusion beats an
// inclusion, which is what a caller naming both means.
func (s Scope) wants(region string) bool {
	for _, excluded := range s.ExcludeRegions {
		if strings.EqualFold(excluded, region) {
			return false
		}
	}
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
		case code == "RequestLimitExceeded", code == "Throttling":
			return cloudinv.FaultThrottle
		case code == "UnauthorizedOperation", code == "AuthFailure":
			return cloudinv.FaultDenied
		case strings.HasSuffix(code, ".NotFound"), strings.HasSuffix(code, "NotFoundException"):
			return cloudinv.FaultNotFound
		case code == "OptInRequired":
			return cloudinv.FaultAPIDisabled
		}
	}
	return cloudinv.FaultUnknown
}
