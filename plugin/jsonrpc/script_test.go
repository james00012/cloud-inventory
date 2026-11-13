package jsonrpc_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAShellScriptCanDriveTheProvider is the question asked of the protocol in
// review: can a script drive a provider and read its output with jq, with no
// client library at all. It is the reason the transport is JSON on standard
// input and output rather than gRPC.
func TestAShellScriptCanDriveTheProvider(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}

	binary, err := build()
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	fake := &cloudRun{refuse: map[string]bool{}, count: 2}
	serve(t, fake)

	out, err := exec.Command("bash", "testdata/drive.sh", binary).CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("the script read nothing useful:\n%s", out)
	}

	if !strings.Contains(lines[0], `"granted":["read"]`) {
		t.Fatalf("configure did not read back: %s", lines[0])
	}

	streamed := 0
	for _, line := range lines {
		if strings.Contains(line, "streamed") {
			streamed++
		}
	}
	// Two locations, two services each, each on its own message.
	if streamed != 4 {
		t.Fatalf("jq saw %d streamed resources:\n%s", streamed, out)
	}
	if !strings.Contains(lines[len(lines)-1], `"asked":2`) {
		t.Fatalf("the listing reply did not read back: %s", lines[len(lines)-1])
	}
}
