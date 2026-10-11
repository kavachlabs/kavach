package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHarbor writes a stand-in for the harbor command: it saves its
// arguments and job config, writes the job folder's config.json and waits for
// SIGINT, then exits 130 as Harbor does.
func fakeHarbor(t *testing.T, script string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "harbor")
	body := `#!/bin/sh
echo "$@" > "` + dir + `/args"
cp "$3" "` + dir + `/config.json"
` + script
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(dir, "jobs")
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("UNUSED=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Runner{Harbor: bin, JobsDir: jobs, TasksDir: "testdata/tasks", EnvFile: env}, dir
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

func TestStartAndStop(t *testing.T) {
	runner, dir := fakeHarbor(t, `
mkdir -p "$JOBS/demo" && echo '{}' > "$JOBS/demo/config.json"
trap 'echo stopping; exit 130' INT
while :; do sleep 0.05; done
`)
	t.Setenv("JOBS", filepath.Join(dir, "jobs"))
	abs, _ := filepath.Abs("testdata/tasks/hello-world-kavach")
	id, err := runner.Start(RunRequest{
		Tasks:      []string{"hello-world-control", abs},
		Models:     []string{"openrouter/a/b", "openrouter/c/d"},
		Attempts:   3,
		TimeoutSec: 600,
		Name:       "demo",
	})
	if err != nil || id != "demo" {
		t.Fatal(id, err)
	}
	reader := &Reader{Dir: runner.JobsDir, Runs: runner}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "jobs", "demo")); return err == nil })

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.HasSuffix(strings.TrimSpace(string(args)), "-y --env-file "+runner.EnvFile) {
		t.Errorf("args %s", args)
	}
	var cfg struct {
		JobName    string `json:"job_name"`
		JobsDir    string `json:"jobs_dir"`
		Attempts   int    `json:"n_attempts"`
		Concurrent int    `json:"n_concurrent_trials"`
		Agents     []struct {
			Name    string  `json:"name"`
			Model   string  `json:"model_name"`
			Timeout float64 `json:"override_timeout_sec"`
		} `json:"agents"`
		Tasks []struct{ Path string } `json:"tasks"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.JobName != "demo" || cfg.JobsDir != runner.JobsDir || cfg.Attempts != 3 || cfg.Concurrent != 1 ||
		len(cfg.Agents) != 2 || cfg.Agents[1].Name != "opencode" || cfg.Agents[1].Model != "openrouter/c/d" || cfg.Agents[0].Timeout != 600 ||
		len(cfg.Tasks) != 2 || cfg.Tasks[0].Path != filepath.Join(filepath.Dir(abs), "hello-world-control") || cfg.Tasks[1].Path != abs {
		t.Fatalf("config %s", b)
	}

	j, err := reader.Job("demo")
	if err != nil || j.Status != JobRunning || !j.Stoppable {
		t.Fatalf("%+v %v", j, err)
	}
	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Name: "demo"}); !errors.Is(err, ErrExists) {
		t.Fatalf("second start: %v", err)
	}

	if err := runner.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	if j, _ = reader.Job("demo"); j.Status != JobStopped || j.Stoppable {
		t.Fatalf("after stop %+v", j)
	}
	if err := runner.Stop("demo"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

// A run that fails before Harbor writes its folder is still listed, with
// Harbor's output as its error.
func TestStartFails(t *testing.T) {
	runner, _ := fakeHarbor(t, `echo "Error: model not found" >&2; exit 1`)
	runner.EnvFile = ""
	id, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Agent: "pkg.mod:Agent"})
	if err != nil {
		t.Fatal(err)
	}
	runner.Wait(id)
	jobs, err := (&Reader{Dir: runner.JobsDir, Runs: runner}).Jobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != id || jobs[0].Status != JobStopped || !strings.Contains(jobs[0].Error, "model not found") ||
		!strings.Contains(jobs[0].Error, "exit status 1") || jobs[0].Started == nil {
		t.Fatalf("%+v", jobs)
	}
}

func TestStartRejects(t *testing.T) {
	runner, _ := fakeHarbor(t, "")
	for _, req := range []RunRequest{
		{},
		{Tasks: []string{"no-such-task"}},
		{Tasks: []string{"hello-world-control"}, Name: "../x"},
		{Tasks: []string{"hello-world-control"}, Attempts: 1000},
		{Tasks: []string{"hello-world-control"}, TimeoutSec: -1},
	} {
		if _, err := runner.Start(req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if err := runner.Stop("nope"); !errors.Is(err, ErrNotRunning) {
		t.Error(err)
	}
}

// startAndExit is the test binary acting as a console that starts a run and
// exits at once: spec is "<harbor> <jobs> <tasks>".
func startAndExit(spec string) int {
	f := strings.Fields(spec)
	r := &Runner{Harbor: f[0], JobsDir: f[1], TasksDir: f[2]}
	if _, err := r.Start(RunRequest{Tasks: []string{"hello-world-control"}, Agent: "oracle", Name: "outlive"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// A run keeps going after the console that started it exits, and writes
// its output: to a pipe, Harbor would die on its first write.
func TestRunOutlivesConsole(t *testing.T) {
	runner, _ := fakeHarbor(t, `
until [ -e "$JOBS/go" ]; do sleep 0.05; done
i=0; while [ $i -lt 200 ]; do echo "progress line $i"; i=$((i+1)); done
mkdir -p "$JOBS/outlive"
echo '{"started_at": "2026-10-11T10:00:00Z", "finished_at": "2026-10-11T10:01:00Z", "n_total_trials": 0}' > "$JOBS/outlive/result.json"
`)
	t.Setenv("JOBS", runner.JobsDir)
	tasks, _ := filepath.Abs(runner.TasksDir)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "BENCH_TEST_START="+runner.Harbor+" "+runner.JobsDir+" "+tasks)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// The console has exited; a new one finds the run, then sees it finish.
	later := &Runner{Harbor: runner.Harbor, JobsDir: runner.JobsDir}
	reader := &Reader{Dir: runner.JobsDir, Runs: later}
	if j, err := reader.Job("outlive"); err != nil || j.Status != JobRunning || !j.Stoppable {
		t.Fatalf("while running: %+v %v", j, err)
	}
	os.WriteFile(filepath.Join(runner.JobsDir, "go"), nil, 0o644)
	waitFor(t, func() bool { j, _ := reader.Job("outlive"); return j.Status == JobDone })
	log, _ := os.ReadFile(filepath.Join(runner.JobsDir, ".bench", "outlive.log"))
	if !strings.Contains(string(log), "progress line 199") {
		t.Fatalf("log %q", log)
	}
}

// A later console can tell a run is still going, and stop it.
func TestStopFromLaterConsole(t *testing.T) {
	runner, _ := fakeHarbor(t, `
trap 'echo stopping; exit 130' INT
while :; do sleep 0.05; done
`)
	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Agent: "oracle", Name: "long"}); err != nil {
		t.Fatal(err)
	}
	later := &Runner{Harbor: runner.Harbor, JobsDir: runner.JobsDir}
	reader := &Reader{Dir: runner.JobsDir, Runs: later}
	j, err := reader.Job("long")
	// Before Harbor writes the job folder, the plan comes from the request.
	if err != nil || j.Status != JobRunning || !j.Stoppable || j.Total != 1 || len(j.Tasks) != 1 || j.Tasks[0] != "hello-world-control" || j.Agent != "oracle" {
		t.Fatalf("%+v %v", j, err)
	}
	if err := later.Stop("long"); err != nil {
		t.Fatal(err)
	}
	runner.Wait("long")
	if j, _ = reader.Job("long"); j.Status != JobStopped || j.Stoppable || j.Error == "" {
		t.Fatalf("after stop %+v", j)
	}
	if err := later.Stop("long"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second stop: %v", err)
	}
}
