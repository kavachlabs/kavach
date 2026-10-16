package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		Retries:    2,
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
		Retry      struct {
			Max int `json:"max_retries"`
		} `json:"retry"`
		Agents []struct {
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
	if cfg.JobName != "demo" || cfg.JobsDir != runner.JobsDir || cfg.Attempts != 3 || cfg.Concurrent != 1 || cfg.Retry.Max != 2 ||
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
		{Tasks: []string{"hello-world-control"}, Retries: 6},
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

func TestStartLiveAgent(t *testing.T) {
	runner, dir := fakeHarbor(t, `pwd > "$OUT/pwd"; echo "$PYTHONPATH" > "$OUT/pythonpath"`)
	t.Setenv("OUT", t.TempDir())
	runner.Root = t.TempDir()
	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-kavach"}, Models: []string{"openrouter/a/b"}, Name: "live"}); err != nil {
		t.Fatal(err)
	}
	runner.Wait("live")
	var cfg struct {
		Agents []map[string]any `json:"agents"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if err := json.Unmarshal(b, &cfg); err != nil || len(cfg.Agents) != 1 || cfg.Agents[0]["import_path"] != LiveAgent || cfg.Agents[0]["name"] != nil {
		t.Errorf("agents %s", b)
	}
	out := os.Getenv("OUT")
	for file, want := range map[string]string{"pwd": runner.Root, "pythonpath": runner.Root} {
		got, _ := os.ReadFile(filepath.Join(out, file))
		if g, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got))); g != mustEval(t, want) {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if _, agent, _, _ := runner.planned("live"); agent != "opencode" {
		t.Errorf("planned agent %q, want opencode", agent)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPauseAndResume(t *testing.T) {
	runner, dir := fakeHarbor(t, `
echo "$UNUSED" > "`+"$JOBS"+`/../env"
mkdir -p "$JOBS/demo/demo__1" "$JOBS/demo/demo__2"
[ -f "$JOBS/demo/config.json" ] || echo '{}' > "$JOBS/demo/config.json"
echo '{}' > "$JOBS/demo/demo__1/config.json"
echo '{"task_name":"hello-world-control","exception_info":{"exception_type":"CancelledError"}}' > "$JOBS/demo/demo__1/result.json"
echo '{}' > "$JOBS/demo/demo__2/config.json"
echo '{"task_name":"hello-world-control","verifier_result":{"rewards":{"reward":1}}}' > "$JOBS/demo/demo__2/result.json"
trap 'echo stopping; exit 130' INT
while :; do sleep 0.05; done
`)
	jobs := filepath.Join(dir, "jobs")
	t.Setenv("JOBS", jobs)
	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Agent: "oracle", Attempts: 2, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	reader := &Reader{Dir: jobs, Runs: runner}
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(jobs, "demo", "demo__2", "result.json"))
		return err == nil
	})
	if err := runner.Resume("demo"); !errors.Is(err, ErrNotStopped) {
		t.Fatalf("resume while running: %v", err)
	}

	if err := runner.Pause("demo"); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	j, _ := reader.Job("demo")
	// The cancelled attempt is left out: resuming runs it again.
	if j.Status != JobPaused || !j.Resumable || j.Stoppable || j.Error != "" || len(j.Trials) != 1 || j.Trials[0].Status != Pass {
		t.Fatalf("paused %+v", j)
	}
	if err := runner.Pause("demo"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second pause: %v", err)
	}
	if j, _ := reader.Job("demo"); j.Status != JobPaused {
		t.Fatalf("after second pause %+v", j)
	}

	// An attempt of a killed Harbor, which wrote no result.json.
	os.MkdirAll(filepath.Join(jobs, "demo", "demo__3"), 0o755)
	os.WriteFile(filepath.Join(jobs, "demo", "demo__3", "config.json"), []byte("{}"), 0o644)
	if err := runner.Resume("demo"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(jobs, "demo", "demo__3")) {
		t.Error("resume kept the killed attempt")
	}
	// Without job_name, harbor jobs resume starts a new job folder.
	var cfg struct {
		JobName string `json:"job_name"`
	}
	if readJSON(filepath.Join(jobs, "demo", "config.json"), &cfg); cfg.JobName != "demo" {
		t.Errorf("job_name %q", cfg.JobName)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "env"))
		return strings.TrimSpace(string(b)) == "1"
	})
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if got := strings.TrimSpace(string(args)); got != "jobs resume -p "+filepath.Join(jobs, "demo") {
		t.Errorf("args %q", got)
	}
	// A later console finds the resumed Harbor by its job folder.
	later := &Reader{Dir: jobs, Runs: &Runner{Harbor: runner.Harbor, JobsDir: jobs}}
	if j, _ := later.Job("demo"); j.Status != JobRunning || !j.Stoppable || j.Resumable {
		t.Fatalf("resumed %+v", j)
	}
	if err := runner.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	if j, _ := reader.Job("demo"); j.Status != JobStopped || !j.Resumable {
		t.Fatalf("stopped %+v", j)
	}

	os.WriteFile(filepath.Join(jobs, "demo", "result.json"), []byte(`{"started_at":"2026-10-12T10:00:00Z","finished_at":"2026-10-12T10:05:00Z"}`), 0o644)
	if err := runner.Resume("demo"); !errors.Is(err, ErrNotStopped) {
		t.Fatalf("resume finished: %v", err)
	}
	if err := runner.Resume("other"); !errors.Is(err, ErrNotStopped) {
		t.Fatalf("resume unknown: %v", err)
	}
}

func TestRetry(t *testing.T) {
	runner, dir := fakeHarbor(t, `
if [ "$1" = run ]; then
	mkdir -p "$JOBS/demo/demo__1"
	echo '{}' > "$JOBS/demo/config.json"
	echo '{}' > "$JOBS/demo/demo__1/config.json"
	echo '{"task_name":"hello-world-control","verifier_result":{"rewards":{"reward":0}}}' > "$JOBS/demo/demo__1/result.json"
	echo '{"started_at":"2026-10-12T10:00:00Z","finished_at":"2026-10-12T10:05:00Z"}' > "$JOBS/demo/result.json"
	exit 0
fi
mkdir -p "$JOBS/demo/demo__2"
echo '{}' > "$JOBS/demo/demo__2/config.json"
trap 'echo stopping; exit 130' INT
while :; do sleep 0.05; done
`)
	jobs := filepath.Join(dir, "jobs")
	t.Setenv("JOBS", jobs)
	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Agent: "oracle", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	reader := &Reader{Dir: jobs, Runs: runner}
	if j, _ := reader.Job("demo"); j.Status != JobDone || !j.Retryable || j.Resumable || len(j.Trials) != 1 || j.Trials[0].Status != Fail {
		t.Fatalf("finished %+v", j)
	}
	for _, c := range [][2]string{{"demo", "demo__7"}, {"demo", ".."}, {"demo", "config.json"}, {"other", "demo__1"}} {
		if err := runner.Retry(c[0], c[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("retry %s/%s: %v", c[0], c[1], err)
		}
	}

	if err := runner.Retry("demo", "demo__1"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(jobs, "demo", "demo__1")) {
		t.Error("the retried attempt is still in the job's folder")
	}
	kept, _ := filepath.Glob(filepath.Join(jobs, ".bench", "retried", "demo", "demo__1.*", "result.json"))
	if len(kept) != 1 {
		t.Errorf("kept %v", kept)
	}
	waitFor(t, func() bool { return exists(filepath.Join(jobs, "demo", "demo__2")) })
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if got := strings.TrimSpace(string(args)); got != "jobs resume -p "+filepath.Join(jobs, "demo") {
		t.Errorf("args %q", got)
	}
	j, _ := reader.Job("demo")
	if j.Retryable || !j.Stoppable {
		t.Fatalf("retrying %+v", j)
	}
	if err := runner.Retry("demo", "demo__2"); !errors.Is(err, ErrNotStopped) {
		t.Errorf("retry while running: %v", err)
	}
	if err := runner.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	// The retried run has no other jobs listed for it.
	if js, _ := reader.Jobs(); len(js) != 1 {
		t.Errorf("jobs %+v", js)
	}
}

func TestNewName(t *testing.T) {
	runner := &Runner{JobsDir: t.TempDir()}
	seen := map[string]bool{}
	for range 50 {
		n := runner.newName()
		if !namePattern.MatchString(n) || !regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+-[a-z0-9]{7}$`).MatchString(n) || seen[n] {
			t.Fatalf("name %q", n)
		}
		seen[n] = true
	}
}
