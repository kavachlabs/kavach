package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostTranscripts plays every transcript of spec/host against this host.
func TestHostTranscripts(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	bin := filepath.Join(t.TempDir(), "kavach-conformance-host")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(py, "-I", "../../spec/host/run.py", "--host", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "18/18 transcripts passed") {
		t.Fatalf("not every transcript ran:\n%s", out)
	}
}
