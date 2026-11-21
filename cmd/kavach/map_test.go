package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/kmap"
)

func buildLedgerHosts(t *testing.T) (oldBin, newBin string) {
	t.Helper()
	tmp := t.TempDir()
	oldBin = filepath.Join(tmp, "ledger-old")
	newBin = filepath.Join(tmp, "ledger-new")
	if out, err := exec.Command("go", "build", "-o", oldBin, "../../examples/ledger/sdk").CombinedOutput(); err != nil {
		t.Fatalf("build ledger-old: %v\n%s", err, out)
	}
	if out, err := exec.Command("go", "build", "-tags", "ledgerfix", "-o", newBin, "../../examples/ledger/sdk").CombinedOutput(); err != nil {
		t.Fatalf("build ledger-new: %v\n%s", err, out)
	}
	return oldBin, newBin
}

func TestCmdMapReplayAndHTML(t *testing.T) {
	oldBin, newBin := buildLedgerHosts(t)
	specPath := "../../examples/ledger/.kavach/plan/map.json"

	code, out, errb := runCLI(t, "map", specPath, fixture, "--bin", oldBin, "--new", newBin, "--json")
	if code != exitPass {
		t.Fatalf("map --json exit %d, stderr:\n%s", code, errb)
	}
	var m kmap.Model
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode map json: %v\n%s", err, out)
	}
	if len(m.Fixtures) != 1 {
		t.Fatalf("expected 1 fixture, got %d", len(m.Fixtures))
	}
	var oldRun, newRun *kmap.Run
	for i := range m.Fixtures[0].Runs {
		r := &m.Fixtures[0].Runs[i]
		switch r.ID {
		case "old":
			oldRun = r
		case "new":
			newRun = r
		}
	}
	if oldRun == nil || !strings.HasPrefix(oldRun.Verdict, "still_failing@32") {
		t.Fatalf("oldRun = %+v, want still_failing@32", oldRun)
	}
	if newRun == nil || newRun.Verdict != "fixed" {
		t.Fatalf("newRun = %+v, want fixed", newRun)
	}
	if len(oldRun.Declared) == 0 || len(newRun.Declared) == 0 {
		t.Fatalf("expected non-empty Declared invariants: old=%v new=%v", oldRun.Declared, newRun.Declared)
	}

	outHTML := filepath.Join(t.TempDir(), "m.html")
	code, out, errb = runCLI(t, "map", specPath, fixture, "-o", outHTML)
	if code != exitPass {
		t.Fatalf("map -o exit %d, stderr:\n%s\nstdout:\n%s", code, errb, out)
	}
	data, err := os.ReadFile(outHTML)
	if err != nil {
		t.Fatalf("read %s: %v", outHTML, err)
	}
	if !strings.Contains(string(data), `id="kavach-map-data"`) {
		t.Fatalf("HTML missing kavach-map-data")
	}
}

func TestCmdMapFixtureOnly(t *testing.T) {
	outHTML := filepath.Join(t.TempDir(), "auto.html")
	code, _, errb := runCLI(t, "map", fixture, "-o", outHTML)
	if code != exitPass {
		t.Fatalf("map fixture-only exit %d, stderr:\n%s", code, errb)
	}
	if _, err := os.Stat(outHTML); err != nil {
		t.Fatalf("expected %s to exist: %v", outHTML, err)
	}
}

func TestCmdMapUsageErrors(t *testing.T) {
	cases := [][]string{
		{"map"},
		{"map", "unknown.txt"},
		{"map", "a.json", "b.json"},
		{"map", fixture, "--new", "./bin"},
		{"map", fixture, "--max-data", "-1"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
