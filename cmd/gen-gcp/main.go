// Command gen-gcp turns a Google Discovery document into the descriptors the
// generic client runs on. Nothing it emits is written by hand.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/james00012/cloud-inventory/generic"
)

// createdFields and labelFields are the names Google uses for the two optional
// contract fields. A type carrying none of them answers with a zero value.
var (
	createdFields = []string{"createTime", "creationTimestamp", "timeCreated", "createdAt", "createTimestamp"}
	labelFields   = []string{"labels", "userLabels", "resourceLabels"}
)

type discovery struct {
	Name      string                 `json:"name"`
	Version   string                 `json:"version"`
	BaseURL   string                 `json:"baseUrl"`
	Schemas   map[string]schema      `json:"schemas"`
	Resources map[string]resourceDoc `json:"resources"`
}

type schema struct {
	Properties map[string]property `json:"properties"`
}

type property struct {
	Type  string `json:"type"`
	Ref   string `json:"$ref"`
	Items *struct {
		Ref string `json:"$ref"`
	} `json:"items"`
}

type resourceDoc struct {
	Methods   map[string]method      `json:"methods"`
	Resources map[string]resourceDoc `json:"resources"`
}

type method struct {
	Path       string           `json:"path"`
	HTTPMethod string           `json:"httpMethod"`
	Parameters map[string]param `json:"parameters"`
	Response   *struct {
		Ref string `json:"$ref"`
	} `json:"response"`
	Request *struct {
		Ref string `json:"$ref"`
	} `json:"request"`
}

type param struct {
	Required bool   `json:"required"`
	Location string `json:"location"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-gcp <discovery.json> [resource.path ...]")
		os.Exit(2)
	}

	body, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var doc discovery
	if err := json.Unmarshal(body, &doc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	wanted := map[string]bool{}
	for _, name := range os.Args[2:] {
		wanted[name] = true
	}

	found := map[string]generic.Descriptor{}
	walk(&doc, doc.Resources, nil, found)

	out := map[string]any{"baseUrl": doc.BaseURL, "descriptors": map[string]generic.Descriptor{}}
	keep := out["descriptors"].(map[string]generic.Descriptor)
	for name, d := range found {
		if len(wanted) == 0 || wanted[name] {
			keep[name] = d
		}
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
	fmt.Fprintf(os.Stderr, "%s %s: %d of %d resource types describable\n", doc.Name, doc.Version, len(keep), len(found))
}

func walk(doc *discovery, resources map[string]resourceDoc, trail []string, out map[string]generic.Descriptor) {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		body := resources[name]
		here := append(append([]string{}, trail...), name)
		if d, ok := describe(doc, body.Methods, strings.Join(here, ".")); ok {
			out[strings.Join(here, ".")] = d
		}
		walk(doc, body.Resources, here, out)
	}
}

// describe reads one resource type out of the document, or reports that the
// document does not say enough about it.
func describe(doc *discovery, methods map[string]method, path string) (generic.Descriptor, bool) {
	list, hasList := methods["list"]
	get, hasGet := methods["get"]
	del, hasDelete := methods["delete"]
	if !hasList || !hasGet || !hasDelete {
		return generic.Descriptor{}, false
	}

	item, arrayField := itemSchema(doc, list)
	if item == nil {
		return generic.Descriptor{}, false
	}

	// How a resource is addressed follows from the document. A get that takes
	// one {+name} is given a self-contained resource path, so the name is the
	// identity. A get that takes several path parameters is not, so the
	// selfLink is, since it is the only self-contained address the API offers.
	identity := ""
	switch {
	case len(requiredPathParams(get)) == 1 && strings.Contains(get.Path, "{+name}"):
		identity = "name"
	case hasProperty(item, "selfLink"):
		identity = "selfLink"
	case hasProperty(item, "name"):
		identity = "name"
	case hasProperty(item, "id"):
		identity = "id"
	}
	if identity == "" {
		return generic.Descriptor{}, false
	}

	d := generic.Descriptor{
		Type:          strings.ReplaceAll(path, ".", "-"),
		ListPath:      list.Path,
		GetPath:       get.Path,
		DeletePath:    del.Path,
		ArrayField:    arrayField,
		IdentityField: identity,
		CreatedField:  firstPresent(item, createdFields),
		LabelsField:   firstPresent(item, labelFields),
		ScopeParams:   requiredPathParams(list),
		Paged:         hasParam(list, "pageToken"),
	}

	// A create is insert on the older APIs and create on the newer ones, and
	// the newer ones take the new name as a query parameter.
	if create, ok := methods["create"]; ok {
		d.CreatePath = create.Path
		d.CreateQuery = newNameQuery(create)
	} else if insert, ok := methods["insert"]; ok {
		d.CreatePath = insert.Path
		d.CreateQuery = newNameQuery(insert)
	}

	return d, true
}

func itemSchema(doc *discovery, list method) (*schema, string) {
	if list.Response == nil {
		return nil, ""
	}
	response, ok := doc.Schemas[list.Response.Ref]
	if !ok {
		return nil, ""
	}

	fields := make([]string, 0, len(response.Properties))
	for field := range response.Properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	for _, field := range fields {
		spec := response.Properties[field]
		if spec.Type == "array" && spec.Items != nil && spec.Items.Ref != "" {
			if item, ok := doc.Schemas[spec.Items.Ref]; ok {
				return &item, field
			}
		}
	}
	return nil, ""
}

func hasProperty(item *schema, name string) bool {
	_, ok := item.Properties[name]
	return ok
}

func firstPresent(item *schema, candidates []string) string {
	for _, candidate := range candidates {
		if _, ok := item.Properties[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func requiredPathParams(m method) []string {
	var found []string
	for name, spec := range m.Parameters {
		if spec.Required && spec.Location == "path" {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

func hasParam(m method, name string) bool {
	_, ok := m.Parameters[name]
	return ok
}

// newNameQuery is the query parameter a create takes the new resource's name
// in, where the API does not take it in the body.
func newNameQuery(m method) string {
	for _, candidate := range []string{"serviceId", "jobId", "topicId", "bucketId", "repositoryId", "instanceId", "functionId"} {
		if spec, ok := m.Parameters[candidate]; ok && spec.Location == "query" {
			return candidate
		}
	}
	return ""
}
