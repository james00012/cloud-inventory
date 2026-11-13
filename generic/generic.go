// Package generic is one implementation of the contract for every Google
// resource type whose shape a Discovery document already describes. A resource
// type is a Descriptor the generator emits, not code somebody writes.
package generic

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	cloudinv "github.com/james00012/cloud-inventory"
)

// Descriptor is everything the generator reads out of a Discovery document for
// one resource type. Nothing here is written by hand.
type Descriptor struct {
	Type string `json:"type"`

	ListPath   string `json:"listPath"`
	GetPath    string `json:"getPath"`
	DeletePath string `json:"deletePath"`
	CreatePath string `json:"createPath,omitempty"`

	// ArrayField is the field of the list response holding the resources.
	ArrayField string `json:"arrayField"`

	// IdentityField, CreatedField and LabelsField are where the contract's
	// methods read from. Empty means the API has no such field, and the
	// contract answers with a zero value.
	IdentityField string `json:"identityField"`
	CreatedField  string `json:"createdField,omitempty"`
	LabelsField   string `json:"labelsField,omitempty"`

	// ScopeParams are the path parameters a list must be given, and ScopeQuery
	// the query parameters it must be given. Together they are the shape of a
	// scope for this type: some APIs name the project in the path, some in the
	// query, and a caller should not have to know which.
	ScopeParams []string `json:"scopeParams"`
	ScopeQuery  []string `json:"scopeQuery,omitempty"`

	// CreateQuery is the query parameter a create takes the new name in, where
	// the API does not take it in the body.
	CreateQuery string `json:"createQuery,omitempty"`

	Paged bool `json:"paged"`

	// Operations this type offers. A generated type arrives with list and get
	// only; create and delete are added by a reviewed change, so a scheduled
	// refresh can never make a new type deletable on its own.
	Operations []string `json:"operations"`
}

// offers reports whether a reviewed change promoted this type to an operation.
func (d Descriptor) offers(operation string) bool {
	for _, allowed := range d.Operations {
		if allowed == operation {
			return true
		}
	}
	return false
}

// Resource is one resource read generically: the contract's five methods over
// the body the API answered with.
type Resource struct {
	Body map[string]any

	descriptor Descriptor
	scope      string
}

func (r Resource) ResourceID() cloudinv.ID {
	text, _ := r.Body[r.descriptor.IdentityField].(string)
	return cloudinv.ID(text)
}

func (r Resource) ResourceType() string { return r.descriptor.Type }

// DisplayName is the last segment of the identity, which is where every Google
// resource path keeps the name a person recognises.
func (r Resource) DisplayName() string {
	text := string(r.ResourceID())
	if at := strings.LastIndex(text, "/"); at >= 0 {
		return text[at+1:]
	}
	return text
}

func (r Resource) CreatedAt() time.Time {
	if r.descriptor.CreatedField == "" {
		return time.Time{}
	}
	text, _ := r.Body[r.descriptor.CreatedField].(string)
	made, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return made
}

func (r Resource) Tags() map[string]string {
	flat := map[string]string{}
	if r.descriptor.LabelsField == "" {
		return flat
	}
	raw, _ := r.Body[r.descriptor.LabelsField].(map[string]any)
	for key, value := range raw {
		if text, ok := value.(string); ok {
			flat[key] = text
		}
	}
	return flat
}

// scopeFields is every parameter a list must be given, however the API takes
// it.
func (d Descriptor) scopeFields() []string {
	return append(append([]string{}, d.ScopeParams...), d.ScopeQuery...)
}

// Scope is what a caller names: the parameters this type's list requires,
// and the values to try for each. One value each is the common case; a list
// that fans out names several.
type Scope map[string][]string

// Client is the generic implementation. One of these serves any resource type
// whose Descriptor the generator produced.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	Descriptor Descriptor
}

var _ cloudinv.Resource = Resource{}

// List reads every resource in each combination of the scope's parameters, and
// reports the combinations it could not read rather than leaving them out.
func (c *Client) List(ctx context.Context, scope Scope) ([]Resource, cloudinv.Incomplete, error) {
	parts, err := combinations(c.Descriptor.scopeFields(), scope)
	if err != nil {
		return nil, cloudinv.Incomplete{}, err
	}

	return cloudinv.FanOut(ctx, keysOf(parts), faultOf, func(ctx context.Context, key string) ([]Resource, error) {
		return c.listIn(ctx, parts[key])
	})
}

func (c *Client) listIn(ctx context.Context, values map[string]string) ([]Resource, error) {
	var found []Resource
	token := ""

	for {
		path, err := expand(c.Descriptor.ListPath, values)
		if err != nil {
			return nil, err
		}
		query := url.Values{}
		for _, field := range c.Descriptor.ScopeQuery {
			if value, ok := values[field]; ok {
				query.Set(field, value)
			}
		}
		if token != "" {
			query.Set("pageToken", token)
		}

		var answer map[string]any
		if err := c.call(ctx, http.MethodGet, path, query, nil, &answer); err != nil {
			return nil, err
		}

		items, _ := answer[c.Descriptor.ArrayField].([]any)
		for _, item := range items {
			body, _ := item.(map[string]any)
			found = append(found, Resource{Body: body, descriptor: c.Descriptor})
		}

		token, _ = answer["nextPageToken"].(string)
		if token == "" || !c.Descriptor.Paged {
			return found, nil
		}
	}
}

// Get reads one resource by the identity a listing returned.
func (c *Client) Get(ctx context.Context, id cloudinv.ID) (Resource, error) {
	where, err := address(c.Descriptor, id)
	if err != nil {
		return Resource{}, err
	}

	var body map[string]any
	if err := c.call(ctx, http.MethodGet, where, nil, nil, &body); err != nil {
		return Resource{}, err
	}
	return Resource{Body: body, descriptor: c.Descriptor}, nil
}

// address is where one resource lives. Newer APIs answer with a resource path
// that the get template takes whole; older ones answer with a selfLink, which
// is already the full address.
func address(d Descriptor, id cloudinv.ID) (string, error) {
	text := string(id)
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return text, nil
	}
	return expand(d.GetPath, map[string]string{"name": text})
}

// Delete removes one resource and waits for the operation it answers with,
// since the contract says a delete does not return until the resource is gone.
func (c *Client) Delete(ctx context.Context, id cloudinv.ID) error {
	if !c.Descriptor.offers("delete") {
		return fmt.Errorf("%s is list-only: adding delete to it is a reviewed change", c.Descriptor.Type)
	}

	where, err := address(c.Descriptor, id)
	if err != nil {
		return err
	}

	var answer map[string]any
	if err := c.call(ctx, http.MethodDelete, where, nil, nil, &answer); err != nil {
		if faultOf(err) == cloudinv.FaultNotFound {
			// Already gone is the state the caller wanted.
			return nil
		}
		return err
	}
	return c.settle(ctx, answer)
}

// Create makes one resource from the API's own request body.
func (c *Client) Create(ctx context.Context, scope Scope, spec Spec) (Resource, error) {
	if c.Descriptor.CreatePath == "" || !c.Descriptor.offers("create") {
		return Resource{}, fmt.Errorf("%s is list-only: adding create to it is a reviewed change", c.Descriptor.Type)
	}

	values, err := single(c.Descriptor.scopeFields(), scope)
	if err != nil {
		return Resource{}, err
	}
	path, err := expand(c.Descriptor.CreatePath, values)
	if err != nil {
		return Resource{}, err
	}

	query := url.Values{}
	for _, field := range c.Descriptor.ScopeQuery {
		if value, ok := values[field]; ok {
			query.Set(field, value)
		}
	}
	if c.Descriptor.CreateQuery != "" && spec.Name != "" {
		query.Set(c.Descriptor.CreateQuery, spec.Name)
	}

	var answer map[string]any
	if err := c.call(ctx, http.MethodPost, path, query, spec.Body, &answer); err != nil {
		return Resource{}, err
	}
	if err := c.settle(ctx, answer); err != nil {
		return Resource{}, err
	}

	// An operation does not carry the resource, so the create reads it back,
	// which is also what makes Create answer with what List and Get answer.
	if id := identityAfterCreate(answer, c.Descriptor, values, spec); id != "" {
		return c.Get(ctx, cloudinv.ID(id))
	}
	return Resource{Body: answer, descriptor: c.Descriptor}, nil
}

// Spec is what a caller supplies to create: the API's own body, and the name
// where the API takes it as a query parameter rather than in the body.
type Spec struct {
	Name string
	Body map[string]any
}
