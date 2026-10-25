package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// The files app.js's apiURL asks a copy for; the hash is what its fnv1a
// gives for the same path.
func TestStaticPath(t *testing.T) {
	for target, want := range map[string]string{
		"/api/jobs":                              "api/jobs/index.json",
		"/api/jobs/a/compare":                    "api/jobs/a/compare/index.json",
		"/api/trials/j/t/invariants?names=a%2Cb": "api/trials/j/t/invariants/index.json",
		"/api/trials/j/t/invariants/source?i=1":  "api/trials/j/t/invariants/source/1.txt",
		"/api/trials/j/t/invariants/file?path=" + url.QueryEscape("src/kombu/é.py"): "api/trials/j/t/invariants/file/fbca0e30.txt",
	} {
		if got := staticPath(target); got != want {
			t.Errorf("staticPath(%s) = %s, want %s", target, got, want)
		}
	}
}

func TestExport(t *testing.T) {
	// Backup state is about this machine: a copy shows none, whatever is set.
	t.Setenv("R2_ACCOUNT_ID", "acct")
	t.Setenv("R2_ACCESS_KEY_ID", "AK")
	t.Setenv("R2_SECRET_ACCESS_KEY", "SK")
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.CopyFS(jobs, os.DirFS("../../harbor/testdata/jobs")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "site")
	var stdout, stderr bytes.Buffer
	args := []string{"export", "-root", "../../../../..", "-jobs", jobs, "-tasks", "../../harbor/testdata/tasks", "-out", out}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}

	index, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	var list []harbor.Job
	readJSON(t, filepath.Join(out, "api/jobs/index.json"), &list)
	if len(list) == 0 {
		t.Fatal("no runs exported")
	}
	for _, j := range list {
		if j.Status != "done" {
			t.Errorf("run %s is %s: only finished runs are exported by default", j.ID, j.Status)
		}
		if j.Stoppable || j.Resumable || j.Retryable {
			t.Errorf("run %s offers to change it", j.ID)
		}
	}
	if !bytes.Contains(index, []byte(`<meta name="bench-static" content="`+list[0].ID+`">`)) {
		t.Errorf("index.html isn't marked as a copy that opens on %s", list[0].ID)
	}
	var job harbor.Job
	readJSON(t, filepath.Join(out, "api/jobs", list[0].ID, "index.json"), &job)
	for _, tr := range job.Trials {
		if tr.ID == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(out, "api/trials", tr.Job, tr.ID, "steps/index.json")); err != nil {
			t.Errorf("attempt %s: %v", tr.ID, err)
		}
	}
	// A Kavach arm's inspector has an Invariants tab even with no contract
	// checks, and asks for the definitions with no names.
	kavach := 0
	for _, j := range list {
		var job harbor.Job
		readJSON(t, filepath.Join(out, "api/jobs", j.ID, "index.json"), &job)
		for _, tr := range job.Trials {
			if tr.ID == "" || tr.Arm != "kavach" && tr.Arm != "kavach-implement" {
				continue
			}
			kavach++
			if _, err := os.Stat(filepath.Join(out, "api/trials", tr.Job, tr.ID, "invariants/index.json")); err != nil {
				t.Errorf("kavach attempt %s has no invariants: %v", tr.ID, err)
			}
		}
	}
	if kavach == 0 {
		t.Error("no kavach attempt was exported, so the invariants check proved nothing")
	}

	// An attempt with an OpenCode log has its Events tab, as one page with
	// nothing after it. That run hasn't finished, so it is exported by name.
	glm := filepath.Join(t.TempDir(), "site")
	if code := run(append(args[:len(args)-1], glm, "-job", "opencode-glm"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var ev harbor.Events
	readJSON(t, filepath.Join(glm, "api/trials/opencode-glm/redis-10068-kavach__Pass001/events/index.json"), &ev)
	if ev.Source != "agent/opencode.txt" || len(ev.Lines) == 0 || ev.Next != ev.Size {
		t.Errorf("events: source %q, %d lines, next %d, size %d", ev.Source, len(ev.Lines), ev.Next, ev.Size)
	}

	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(`"sync"`)) {
			t.Errorf("%s holds sync state", p)
		}
		return nil
	})

	// The jobs directory is outside the repository here; its paths still
	// don't reach the copy.
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(jobs)) {
			t.Errorf("%s names %s", p, jobs)
		}
		return nil
	})

	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "not empty") {
		t.Errorf("a second export into the same directory: exit %d\n%s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(append(args[:len(args)-1], t.TempDir(), "-job", "nope"), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), `no run named "nope"`) {
		t.Errorf("an unknown run: exit %d\n%s", code, stderr.String())
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
