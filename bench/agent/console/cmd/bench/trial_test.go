package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
)

// trialCLI runs bench trial against a temporary config, jobs directory and
// tasks directory with a control and a kavach arm of "inc" and only a control
// arm of "half".
func trialCLI(t *testing.T) (run func(args ...string) (int, string, string), config, jobs string) {
	t.Helper()
	dir := t.TempDir()
	config, jobs = filepath.Join(dir, "trial.json"), filepath.Join(dir, "jobs")
	tasks := filepath.Join(dir, "tasks")
	for _, n := range []string{"inc-control", "inc-kavach", "inc-workflow", "half-control"} {
		os.MkdirAll(filepath.Join(tasks, n), 0o755)
		img := ""
		if n == "inc-kavach" {
			img = "[environment]\ndocker_image = \"ghcr.io/x/bench-inc:kavach-abc\"\n"
		}
		os.WriteFile(filepath.Join(tasks, n, "task.toml"), []byte(img), 0o644)
	}
	real := imagePublished
	imagePublished = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { imagePublished = real })
	return func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		full := append([]string{args[0], args[1], "-config", config, "-jobs", jobs, "-tasks", tasks, "-env", filepath.Join(dir, "none.env")}, args[2:]...)
		return cmdTrial(full, &out, &errb), out.String(), errb.String()
	}, config, jobs
}

func TestTrialCLIInvites(t *testing.T) {
	run, config, jobs := trialCLI(t)
	if code, _, errs := run("invite", "add", "-runs", "3"); code != 1 || !strings.Contains(errs, "-label") {
		t.Fatalf("no label: %d %q", code, errs)
	}
	code, out, errs := run("invite", "add", "-label", "Garry (YC)", "-runs", "3", "-url", "https://try.example")
	if code != 0 {
		t.Fatalf("add: %d %s", code, errs)
	}
	m := regexp.MustCompile(`code  (yc(-[a-z2-9]{4}){4})\n`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "url   https://try.example/#/trial/invite?code="+m[1]) {
		t.Fatalf("add output:\n%s", out)
	}
	if code, _, errs := run("invite", "add", "-label", "Fixed", "-code", "partner-fixed-1"); code != 0 {
		t.Fatalf("add -code: %d %s", code, errs)
	}
	if code, _, errs := run("invite", "add", "-label", "Short", "-code", "abc"); code != 1 || !strings.Contains(errs, "at least") {
		t.Fatalf("a short code: %d %q", code, errs)
	}

	// Usage the server recorded shows in the list.
	os.MkdirAll(filepath.Join(jobs, ".bench"), 0o755)
	os.WriteFile(filepath.Join(jobs, ".bench", "invites-used.json"), []byte(`{"`+api.InviteDigest(m[1])+`":{"runs":3,"last":"2026-05-01T10:30:00Z","jobs":["run-a"]}}`), 0o600)
	_, out, _ = run("invite", "list")
	if !strings.Contains(out, "Garry (YC)") || !strings.Contains(out, "3/3") || !strings.Contains(out, "spent") || !strings.Contains(out, "2026-05-01 10:30Z") || !strings.Contains(out, "0/5") {
		t.Errorf("list:\n%s", out)
	}
	if _, out, _ = run("invite", "show", "garry (yc)"); !strings.Contains(out, "run-a") {
		t.Errorf("show:\n%s", out)
	}

	if code, _, errs := run("invite", "revoke", "garry"); code != 1 || !strings.Contains(errs, "full label") {
		t.Fatalf("revoke by part of a label: %d %q", code, errs)
	}
	if code, out, errs := run("invite", "revoke", "garry (yc)"); code != 0 || !strings.Contains(out, m[1]) {
		t.Fatalf("revoke by label: %d %s %s", code, out, errs)
	}
	if code, _, _ := run("invite", "revoke", "nobody"); code != 1 {
		t.Errorf("revoke of nobody: %d", code)
	}
	f, err := api.LoadTrialFile(config)
	if err != nil || len(f.Invites) != 2 || !f.Invites[0].Revoked || f.Invites[1].Revoked || f.Invites[1].Runs != 5 {
		t.Fatalf("file: %+v %v", f, err)
	}
	if _, out, _ = run("invite", "list"); !strings.Contains(out, "revoked") {
		t.Errorf("list after revoke:\n%s", out)
	}
}

func TestTrialCLITasksAndModels(t *testing.T) {
	run, config, _ := trialCLI(t)
	if code, _, errs := run("task", "add", "nope"); code != 1 || !strings.Contains(errs, "no task") {
		t.Fatalf("unknown task: %d %q", code, errs)
	}
	if code, _, errs := run("task", "add", "half"); code != 1 || !strings.Contains(errs, "no kavach arm") {
		t.Fatalf("a task without both arms: %d %q", code, errs)
	}
	code, out, errs := run("task", "add", "inc")
	if code != 0 || !strings.Contains(out, "inc: arms control, workflow, kavach") || !strings.Contains(out, "bench-inc:kavach-abc is not published") || !strings.Contains(out, "trial tasks: inc-control, inc-kavach") {
		t.Fatalf("task add: %d\n%s%s", code, out, errs)
	}
	if code, out, _ := run("task", "add", "half-control"); code != 0 || !strings.Contains(out, "inc-control, inc-kavach, half-control") {
		t.Fatalf("a folder name: %d %s", code, out)
	}
	if _, out, _ := run("task", "list"); !strings.Contains(out, "inc-kavach (arm kavach)") {
		t.Errorf("list:\n%s", out)
	}
	if code, out, _ := run("task", "remove", "inc"); code != 0 || strings.Contains(out, "inc-") {
		t.Fatalf("remove by base name: %d %s", code, out)
	}
	if code, _, errs := run("task", "remove", "half-control"); code != 1 || !strings.Contains(errs, "empty") {
		t.Errorf("emptying the list: %d %q", code, errs)
	}

	if code, _, errs := run("model", "add", "mystery/x"); code != 1 || !strings.Contains(errs, "provider") {
		t.Errorf("unknown provider: %d %q", code, errs)
	}
	if code, out, _ := run("model", "add", "openrouter/a/b"); code != 0 || !strings.Contains(out, "openrouter/a/b") {
		t.Errorf("model add: %d %s", code, out)
	}
	f, _ := api.LoadTrialFile(config)
	if len(f.Models) != 1 || len(f.Tasks) != 1 {
		t.Errorf("file: %+v", f)
	}
}

func TestTrialCLISeedsFromEnvFile(t *testing.T) {
	run, config, _ := trialCLI(t)
	envFile := filepath.Join(filepath.Dir(config), "trial.env")
	os.WriteFile(envFile, []byte("TRIAL_TASKS=half-control\nTRIAL_MODELS=openrouter/m/one\n"), 0o600)
	if code, out, _ := run("task", "add", "-env", envFile, "inc"); code != 0 || !strings.Contains(out, "half-control, inc-control, inc-kavach") {
		t.Fatalf("task add: %d %s", code, out)
	}
	if code, out, _ := run("model", "add", "-env", envFile, "openrouter/m/two"); code != 0 || !strings.Contains(out, "openrouter/m/one, openrouter/m/two") {
		t.Fatalf("model add: %d %s", code, out)
	}
}

func TestAPIRefusesToStartOnBrokenTrialConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "trial.json")
	os.WriteFile(bad, []byte("{nope"), 0o600)
	var out, errb bytes.Buffer
	code := cmdAPI([]string{"-trial", "-trial-config", bad, "-jobs", dir, "-tasks", dir, "-port", "0"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "refusing to start") {
		t.Errorf("exit %d: %s", code, errb.String())
	}
}

func TestAPICheckOnly(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-trial", "-check", "-trial-config", filepath.Join(dir, "trial.json"), "-jobs", dir, "-tasks", dir}
	var out, errb bytes.Buffer
	if code := cmdAPI(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "ok") {
		t.Errorf("exit %d: %s %s", code, out.String(), errb.String())
	}
}

func TestTrialCLIMatchesLabelPart(t *testing.T) {
	run, _, _ := trialCLI(t)
	run("invite", "add", "-label", "Garry (YC)")
	run("invite", "add", "-label", "Garth Co")
	if code, _, _ := run("invite", "show", "garry"); code != 0 {
		t.Errorf("a unique part of a label: %d", code)
	}
	if code, _, errs := run("invite", "show", "gar"); code != 1 || !strings.Contains(errs, "more than one") {
		t.Errorf("an ambiguous part: %d %q", code, errs)
	}
}
