package harbor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A console that restarts leaves the run alone, and a later one can stop it:
// Stop reaches Harbor through the helper, and what the redactor held back is
// written when Harbor exits.
func TestIsolatedRunSurvivesConsoleAndStops(t *testing.T) {
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	script := "#!/bin/sh\ntrap 'printf \"stopped $MY_KEY\"; exit 130' INT\necho started > " + dir + "/started\nwhile :; do sleep 0.05; done\n"
	os.WriteFile(harborCmd, []byte(script), 0o755)
	tasks := filepath.Join(dir, "tasks")
	os.MkdirAll(filepath.Join(tasks, "t"), 0o755)
	os.WriteFile(filepath.Join(tasks, "t", "task.toml"), nil, 0o644)
	jobs := filepath.Join(dir, "jobs")
	first := &Runner{Harbor: harborCmd, JobsDir: jobs, TasksDir: tasks}
	id, err := first.Start(RunRequest{Tasks: []string{"t"}, Env: map[string]string{"MY_KEY": "sk-visitor-0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); !exists(filepath.Join(dir, "started")) && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if !exists(first.file("home/"+id, "")) {
		t.Fatal("no home folder while running")
	}

	// A console that started later knows the run only by its pid file.
	second := &Runner{Harbor: harborCmd, JobsDir: jobs, TasksDir: tasks}
	if !second.alive(id) {
		t.Fatal("a new console does not see the run")
	}
	if err := second.Stop(id); err != nil {
		t.Fatal(err)
	}
	first.Wait(id)
	log, _ := os.ReadFile(first.file(id, ".log"))
	if !strings.Contains(string(log), "stopped [redacted]") || strings.Contains(string(log), "0123456789abcdef") {
		t.Fatalf("log %q", log)
	}
	if exists(first.file("home/"+id, "")) {
		t.Error("home survives")
	}
}

func TestSweepHomes(t *testing.T) {
	r := &Runner{Harbor: "harbor", JobsDir: t.TempDir()}
	os.MkdirAll(r.file("home/stale", ""), 0o700)
	r.SweepHomes()
	if exists(r.file("home/stale", "")) {
		t.Error("stale home kept")
	}
}
