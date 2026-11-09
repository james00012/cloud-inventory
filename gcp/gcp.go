// Package gcp holds the GCP half of the inventory: one file per resource type,
// each a typed struct and the four calls against it.
package gcp

import (
	"errors"
	"strings"

	cloudinv "github.com/james00012/cloud-inventory"
	"google.golang.org/api/googleapi"
)

// Scope is where to look in GCP. Locations empty means every location the
// project offers: naming them is how a caller narrows the fan-out, so a run
// restricted to one region makes one call rather than one per region.
type Scope struct {
	ProjectID string
	Locations []string
}

// wants reports whether the scope asks for this location.
func (s Scope) wants(location string) bool {
	if len(s.Locations) == 0 {
		return true
	}
	for _, asked := range s.Locations {
		if strings.EqualFold(asked, location) {
			return true
		}
	}
	return false
}

// faultOf reads a GCP error into the shape every provider shares, so a consumer
// never matches on a provider's own error types.
func faultOf(err error) cloudinv.Fault {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 403:
			for _, e := range apiErr.Errors {
				if e.Reason == "accessNotConfigured" || e.Reason == "SERVICE_DISABLED" {
					return cloudinv.FaultAPIDisabled
				}
			}
			return cloudinv.FaultDenied
		case 404:
			return cloudinv.FaultNotFound
		case 429:
			return cloudinv.FaultThrottle
		}
	}
	return cloudinv.FaultUnknown
}
