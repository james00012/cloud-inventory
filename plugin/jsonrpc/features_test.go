package jsonrpc_test

import (
	"strings"
	"testing"

	"github.com/james00012/cloud-inventory/gcp"
	"github.com/james00012/cloud-inventory/plugin/core"
	runv2 "google.golang.org/api/run/v2"
)

func body() *runv2.GoogleCloudRunV2Service {
	return &runv2.GoogleCloudRunV2Service{
		Template: &runv2.GoogleCloudRunV2RevisionTemplate{
			Containers: []*runv2.GoogleCloudRunV2Container{{Image: "gcr.io/demo/api:1"}},
		},
	}
}

func TestAProviderDeclaresWhatItOffers(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := start(t, fake)

	declared, err := session.DescribeTypes()
	if err != nil {
		t.Fatalf("describing types: %v", err)
	}
	if len(declared) != 1 {
		t.Fatalf("declared %d types", len(declared))
	}
	if declared[0].Type != "cloud-run-service" {
		t.Fatalf("declared %q", declared[0].Type)
	}
	if len(declared[0].Operations) != 4 {
		t.Fatalf("declared operations %v", declared[0].Operations)
	}
	if len(declared[0].ScopeFields) == 0 {
		t.Fatal("declared no scope fields, so a tool cannot build a scope")
	}
}

func TestWhatARunCreatesCarriesTheRunsTag(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := startAs(t, fake, "cloud-nuke", "01JABCD", core.Create)

	if _, err := session.Create(gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api", Location: "us-central1", GoogleCloudRunV2Service: body(),
	}, false); err != nil {
		t.Fatalf("creating: %v", err)
	}

	creates := fake.creates()
	if len(creates) != 1 {
		t.Fatalf("the provider posted %d creates", len(creates))
	}
	labels, _ := creates[0]["labels"].(map[string]any)
	if labels["cloud-inventory-run"] != "01JABCD" {
		t.Fatalf("the run's tag did not reach the API: %v", creates[0])
	}
}

func TestARetriedCreateDoesNotMakeASecondResource(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := startAs(t, fake, "cloud-nuke", "01JABCD", core.Create)

	spec := gcp.CloudRunServiceSpec{ServiceID: "api", Location: "us-central1", GoogleCloudRunV2Service: body()}
	first, err := session.Create(gcp.Scope{ProjectID: "demo"}, spec, false)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	second, err := session.Create(gcp.Scope{ProjectID: "demo"}, spec, false)
	if err != nil {
		t.Fatalf("retrying the create: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("the retry made %s, not %s", second.ID, first.ID)
	}
	t.Logf("requests: %v", fake.asked())
	if n := len(fake.creates()); n != 1 {
		t.Fatalf("the retry posted a second create: %d in all", n)
	}
}

func TestTheAuditLogRecordsEveryCreateAndDeleteAndTheHostCanReadIt(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := startAs(t, fake, "cloud-nuke", "01JABCD", core.Create, core.Delete)

	made, err := session.Create(gcp.Scope{ProjectID: "demo"}, gcp.CloudRunServiceSpec{
		ServiceID: "api", Location: "us-central1", GoogleCloudRunV2Service: body(),
	}, false)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := session.Delete(made.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	entries, err := session.Audit()
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the log holds %d entries", len(entries))
	}
	for _, entry := range entries {
		if entry.Caller != "cloud-nuke" || entry.Run != "01JABCD" || entry.Account != "demo" {
			t.Fatalf("an entry does not say who did it: %+v", entry)
		}
		if entry.Resource != made.ID {
			t.Fatalf("an entry names %q", entry.Resource)
		}
		if entry.At.IsZero() {
			t.Fatal("an entry has no time")
		}
	}
	if entries[0].Action != "create" || entries[1].Action != "delete" {
		t.Fatalf("the log is out of order: %v, %v", entries[0].Action, entries[1].Action)
	}
}

func TestAReadOnlySessionStillReadsTheAuditLog(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}}
	session := start(t, fake)

	entries, err := session.Audit()
	if err != nil {
		t.Fatalf("a read-only session could not read the log: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a session that wrote nothing has %d entries", len(entries))
	}
}

func TestAResourceSaysWhatItSitsInside(t *testing.T) {
	fake := &cloudRun{refuse: map[string]bool{}, present: true}
	session := start(t, fake)

	got, err := session.Get(gcp.ServiceID("demo", "us-central1", "api"), false)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if len(got.Parents) != 1 {
		t.Fatalf("parents %v", got.Parents)
	}
	if !strings.HasSuffix(string(got.Parents[0]), "locations/us-central1") {
		t.Fatalf("the parent is not the location: %q", got.Parents[0])
	}
	if !got.Deletable {
		t.Fatalf("a live service reported as not deletable: %q", got.NotDeletable)
	}
}
