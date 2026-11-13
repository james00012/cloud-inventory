package jsonrpc_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/core"
	"github.com/james00012/cloud-inventory/plugin/jsonrpc"
	"github.com/james00012/cloud-inventory/plugin/jsonrpc/schema"
	"github.com/santhosh-tekuri/jsonschema/v5"
	runv2 "google.golang.org/api/run/v2"
)

// compiled is the protocol's schema, which the RFC calls the contract.
var compiled = sync.OnceValue(func() *jsonschema.Schema {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat = true
	if err := compiler.AddResource("protocol.json", bytes.NewReader(schema.Protocol)); err != nil {
		panic(err)
	}
	return compiler.MustCompile("protocol.json")
})

func valid(t *testing.T, where string, line []byte) {
	t.Helper()

	var message any
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatalf("%s is not json: %v", where, err)
	}
	if err := compiled().Validate(message); err != nil {
		t.Errorf("%s does not match the protocol schema:\n%s\n%v", where, line, err)
	}
}

// TestEveryMessageMatchesTheSchema drives a whole session and checks each line
// in both directions against the schema. The schema is only the contract if
// the implementation is held to it.
func TestEveryMessageMatchesTheSchema(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{"europe-west1": true}, count: 2}
	binary, err := build()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	serve(t, fake)

	var lines []struct {
		direction string
		body      []byte
	}
	session, err := jsonrpc.Start(t.Context(), jsonrpc.Options{
		Binary: binary, Features: []core.Feature{core.Create, core.Delete},
		ExpectAccount: "demo", Caller: "cloud-nuke", Run: "01JABCD",
		Scope: gcp.Scope{ProjectID: "demo"},
		Wire: func(direction string, line []byte) {
			lines = append(lines, struct {
				direction string
				body      []byte
			}{direction, line})
		},
	})
	if err != nil {
		t.Fatalf("starting: %v", err)
	}
	defer session.Close()

	if _, err := session.DescribeTypes(); err != nil {
		t.Fatalf("describing types: %v", err)
	}
	if _, err := session.DiscoverScopes(); err != nil {
		t.Fatalf("discovering scopes: %v", err)
	}
	made, err := session.Create(gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api", Location: "us-central1",
		GoogleCloudRunV2Service: &runv2.GoogleCloudRunV2Service{
			Template: &runv2.GoogleCloudRunV2RevisionTemplate{
				Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if _, err := session.Get(made.ID, false); err != nil {
		t.Fatalf("getting: %v", err)
	}
	if _, err := session.List(gcp.Scope{ProjectID: "demo"}, false, func(core.Resource) error { return nil }); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if err := session.Delete(made.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := session.Audit(); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}

	if len(lines) < 16 {
		t.Fatalf("only %d messages crossed the wire", len(lines))
	}
	streamed := 0
	for i, line := range lines {
		valid(t, line.direction+" message "+itoa(i), line.body)
		if strings.Contains(string(line.body), `"method":"resource"`) {
			streamed++
		}
	}
	if streamed == 0 {
		t.Fatal("no streamed resource was checked against the schema")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}
