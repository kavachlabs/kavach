package harbor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeRunner sets up a Runner whose Harbor is a script and a task for it.
func fakeRunner(t *testing.T, script string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	os.WriteFile(harborCmd, []byte("#!/bin/sh\n"+script), 0o755)
	tasks := filepath.Join(dir, "tasks")
	os.MkdirAll(filepath.Join(tasks, "t"), 0o755)
	os.WriteFile(filepath.Join(tasks, "t", "task.toml"), nil, 0o644)
	return &Runner{Harbor: harborCmd, JobsDir: filepath.Join(dir, "jobs"), TasksDir: tasks}, dir
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Killed outright, the helper leaves Harbor running in its process group;
// the run still counts as live and can be stopped.
func TestKilledHelperLeavesHarborLiveAndStoppable(t *testing.T) {
	r, dir := fakeRunner(t, "trap 'echo stopped > $0.stopped; exit 130' INT\necho up > $0.up\nwhile :; do sleep 0.05; done\n")
	id, err := r.Start(RunRequest{Tasks: []string{"t"}, Env: map[string]string{"MY_KEY": "sk-visitor-0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "harbor", func() bool { return exists(filepath.Join(dir, "harbor.up")) })
	b, _ := os.ReadFile(r.file(id, ".pid"))
	helper, _ := strconv.Atoi(string(b))
	if err := syscall.Kill(helper, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r.Wait(id)

	later := &Runner{Harbor: r.Harbor, JobsDir: r.JobsDir, TasksDir: r.TasksDir}
	for name, rn := range map[string]*Runner{"same console": r, "later console": later} {
		if !rn.Alive(id) {
			t.Errorf("%s: the orphaned Harbor does not count as live", name)
		}
		if _, alive, _ := rn.state(id); !alive {
			t.Errorf("%s: the job does not show as running", name)
		}
	}
	if err := later.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "harbor to stop", func() bool { return !r.Alive(id) })
	if !exists(filepath.Join(dir, "harbor.stopped")) {
		t.Error("Harbor was not interrupted")
	}
	if err := r.Stop(id); err != ErrNotRunning {
		t.Errorf("stop after exit: %v", err)
	}
}

// A process group that merely has the recorded number is not the run's.
func TestReusedGroupIsNotTheRun(t *testing.T) {
	r, _ := fakeRunner(t, "exit 0\n")
	os.MkdirAll(r.file("", ""), 0o755)
	os.WriteFile(r.file("x", ".json"), []byte("{}"), 0o644)
	os.WriteFile(r.file("x", ".pid"), []byte(strconv.Itoa(syscall.Getpgrp())), 0o644)
	if r.Alive("x") {
		t.Error("an unrelated group counts as live")
	}
	if err := r.Stop("x"); err != ErrNotRunning {
		t.Errorf("stop: %v", err)
	}
}

func TestHelperScrubsTheJobFolder(t *testing.T) {
	r, dir := fakeRunner(t, `J=`+"$(dirname $0)"+`/jobs/scrubjob/t__1
mkdir -p $J/agent
printf 'before %s after "%s"' "$MY_KEY" "$MY_KEY" > $J/agent/trace.txt
chmod 600 $J/agent/trace.txt
head -c 65530 /dev/zero | tr '\0' 'x' > $J/big.bin
printf '%s' "$MY_KEY" >> $J/big.bin
printf 'clean' > $J/clean.txt
exit 0
`)
	key := "sk-visitor-0123456789abcdef"
	id, err := r.Start(RunRequest{Name: "scrubjob", Tasks: []string{"t"}, Env: map[string]string{"MY_KEY": key}})
	if err != nil {
		t.Fatal(err)
	}
	r.Wait(id)
	job := filepath.Join(dir, "jobs", "scrubjob")
	trace, _ := os.ReadFile(filepath.Join(job, "t__1", "agent", "trace.txt"))
	if string(trace) != `before [redacted] after "[redacted]"` {
		t.Errorf("trace %q", trace)
	}
	if st, _ := os.Stat(filepath.Join(job, "t__1", "agent", "trace.txt")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	big, _ := os.ReadFile(filepath.Join(job, "t__1", "big.bin"))
	if strings.Contains(string(big), "0123456789") || !strings.HasSuffix(string(big), "[redacted]") {
		t.Errorf("big file tail %q", big[len(big)-40:])
	}
	if clean, _ := os.ReadFile(filepath.Join(job, "t__1", "clean.txt")); string(clean) != "clean" {
		t.Errorf("clean file %q", clean)
	}
	entries, _ := os.ReadDir(filepath.Join(job, "t__1"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scrub-") {
			t.Errorf("temporary file %s left", e.Name())
		}
	}
}
