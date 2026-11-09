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
// project offers, and an exclusion is read here rather than by the caller so
// the fan-out never spends a call on a location the run rules out.
type Scope struct {
	ProjectID string

	Locations        []string
	ExcludeLocations []string
}

// wants reports whether the scope asks for this location. An exclusion beats an
// inclusion, which is what a caller naming both means.
func (s Scope) wants(location string) bool {
	for _, excluded := range s.ExcludeLocations {
		if strings.EqualFold(excluded, location) {
			return false
		}
	}
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
	if !errors.As(err, &apiErr) {
		return cloudinv.FaultUnknown
	}

	switch apiErr.Code {
	case 403:
		if serviceDisabled(apiErr) {
			return cloudinv.FaultAPIDisabled
		}
		return cloudinv.FaultDenied
	case 404:
		return cloudinv.FaultNotFound
	case 429:
		return cloudinv.FaultThrottle
	}
	return cloudinv.FaultUnknown
}

// serviceDisabled reads both shapes Google uses to say an API is switched off.
// The current one puts the reason in Details; the older one puts it in Errors,
// and a client built on either sees only its own.
func serviceDisabled(apiErr *googleapi.Error) bool {
	for _, item := range apiErr.Errors {
		if item.Reason == "accessNotConfigured" || item.Reason == "SERVICE_DISABLED" {
			return true
		}
	}
	for _, detail := range apiErr.Details {
		asMap, ok := detail.(map[string]interface{})
		if !ok {
			continue
		}
		if asMap["reason"] == "SERVICE_DISABLED" || asMap["reason"] == "accessNotConfigured" {
			return true
		}
	}
	return false
}
