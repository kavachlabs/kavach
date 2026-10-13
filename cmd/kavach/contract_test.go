package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

// storeContract builds the test host, writes traffic it recorded by hand and
// returns the host's path and a work directory holding traffic.kavach and
// SPEC.md.
func storeContract(t *testing.T) (host, work string) {
	t.Helper()
	work = t.TempDir()
	host = filepath.Join(work, "storehost")
	if out, err := exec.Command("go", "build", "-o", host, "./testdata/storehost").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	recs := []journal.Record{{Seq: 0, Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: []journal.Fact{}}}
	seq := uint64(1)
	for _, s := range [][2]any{
		{[]string{"SET", "greeting", "hello"}, "OK"}, {[]string{"SET", "n", "41"}, "OK"},
		{[]string{"GET", "missing"}, "nil"}, {[]string{"INCR", "n"}, "ERR unknown command"},
		{[]string{"GET", "n"}, "41"},
	} {
		b, _ := json.Marshal(map[string]any{"argv": s[0]})
		recs = append(recs,
			journal.Record{Seq: seq, Type: journal.TypeInput, Source: "test", Data: b},
			journal.Record{Seq: seq + 1, Type: journal.TypeOutput, Sink: "reply", Data: []byte(s[1].(string))})
		seq += 2
	}
	meta := journal.Meta{Service: "store", Compression: journal.CompressionNone, Start: journal.StartGenesis}
	if err := journal.WriteFile(filepath.Join(work, "traffic.kavach"), meta, recs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "SPEC.md"), []byte("Add INCR.\n"), 0o644)
	return host, work
}

func TestContractCLI(t *testing.T) {
	host, work := storeContract(t)
	dir := filepath.Join(work, "contract")
	code, out, errs := runCLI(t, "contract", "init", "--dir", dir, "--spec", filepath.Join(work, "SPEC.md"),
		"--traffic", filepath.Join(work, "traffic.kavach"), "--feature-match", `$.argv[0]=="INCR"`, "--feature-steps", "9",
		"--invariant", "counter-holds-integer")
	if code != exitPass {
		t.Fatalf("init: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "2 feature steps") {
		t.Errorf("init output:\n%s", out)
	}

	check := func(build string) (int, map[string]any) {
		t.Helper()
		code, out, errs := runCLI(t, "contract", "check", "--dir", dir, "--old", host+" base", "--new", host+" "+build, "--json")
		var r map[string]any
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%s: %v\n%s%s", build, err, out, errs)
		}
		return code, r
	}
	for build, want := range map[string]string{"base": "unreached@7", "regressing": "regression@5", "sloppy": "variant_failed(", "good": "unapproved"} {
		code, r := check(build)
		if v := r["verdict"].(string); !strings.HasPrefix(v, want) || code != exitFail {
			t.Errorf("%s: exit %d, verdict %s, want %s", build, code, v, want)
		}
	}

	// Without --yes, approval needs a person at a terminal: with stdin not a
	// terminal (or /dev/null, which reads as an empty answer) nothing is locked.
	if code, _, errs = runCLI(t, "contract", "approve", "--dir", dir); code == exitPass {
		t.Errorf("approve without a person: exit %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock.json")); err == nil {
		t.Error("approve without a person wrote a lock")
	}
	if code, out, errs = runCLI(t, "contract", "approve", "--dir", dir, "--yes"); code != exitPass {
		t.Fatalf("approve --yes: exit %d\n%s%s", code, out, errs)
	}
	code, r := check("good")
	if code != exitPass || r["verdict"] != "pass" {
		t.Errorf("good, approved: exit %d, %v", code, r["verdict"])
	}
	if code, out, _ = runCLI(t, "contract", "show", "--dir", dir); !strings.Contains(out, "approved (with --yes, not at a terminal)") {
		t.Errorf("show:\n%s", out)
	}
}
