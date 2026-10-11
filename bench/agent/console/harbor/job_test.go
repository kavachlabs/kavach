package harbor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdata/jobs holds real Harbor 0.24 job folders (trimmed: logs and long
// tracebacks dropped, paths rewritten):
//
//	oracle-both   oracle on hello-world-control and -kavach, 2 attempts
//	              each: one pass, three verifier timeouts
//	nop-fail      nop on hello-world-control: reward 0
//	stopped       oracle stopped by SIGTERM mid-trial
//	nop-running   nop-fail copied while its trial was running
//	redis-nop     nop on both arms of redis-10068: grade.json, grade.log,
//	              kavach.json, an empty patch
//	redis-oracle  oracle on both arms of redis-10068: the upstream patch
//
// and opencode-glm, built by gen/fixture.py in the same layout from a real
// OpenCode attempt, with kavach.json and opencode-live.json.
//
// Tests read a fresh copy: a job nobody writes to for staleAfter counts as
// stopped, and the checked-in files are older than that.
var jobsDir string

func TestMain(m *testing.M) {
	if spec := os.Getenv("BENCH_TEST_START"); spec != "" {
		os.Exit(startAndExit(spec))
	}
	dir, err := os.MkdirTemp("", "harbor-test-")
	if err != nil {
		panic(err)
	}
	jobsDir = filepath.Join(dir, "jobs")
	if err := os.CopyFS(jobsDir, os.DirFS("testdata/jobs")); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func readJob(t *testing.T, id string) Job {
	t.Helper()
	j, err := (&Reader{Dir: jobsDir}).Job(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func byID(t *testing.T, j Job, id string) Trial {
	t.Helper()
	for _, tr := range j.Trials {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("no trial %s in %s", id, j.ID)
	return Trial{}
}

func TestJobs(t *testing.T) {
	jobs, err := (&Reader{Dir: jobsDir}).Jobs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		got[j.ID] = j.Status
	}
	want := map[string]string{
		"oracle-both": JobDone, "nop-fail": JobDone, "stopped": JobStopped,
		"nop-running": JobRunning, "opencode-glm": JobRunning,
		"redis-nop": JobDone, "redis-oracle": JobDone,
	}
	if len(got) != len(want) {
		t.Fatalf("jobs %v, want %v", got, want)
	}
	for id, s := range want {
		if got[id] != s {
			t.Errorf("%s: status %q, want %q", id, got[id], s)
		}
	}
	for i := 1; i < len(jobs); i++ {
		if jobs[i].Started.After(*jobs[i-1].Started) {
			t.Errorf("jobs not newest first: %s before %s", jobs[i-1].ID, jobs[i].ID)
		}
	}
}

func TestFinishedJob(t *testing.T) {
	j := readJob(t, "oracle-both")
	if j.Agent != "oracle" || j.Done != 4 || j.Total != 4 || j.Finished == nil || len(j.Models) != 0 {
		t.Fatalf("job %+v", j)
	}
	if strings.Join(j.Tasks, ",") != "hello-world-control,hello-world-kavach" {
		t.Fatalf("tasks %v", j.Tasks)
	}
	pass := byID(t, j, "hello-world-control__zRmJZqb")
	if pass.Status != Pass || *pass.Reward != 1 || pass.Arm != "control" || pass.Seconds == nil || pass.Cost != nil || pass.Error != "" {
		t.Fatalf("passing trial %+v", pass)
	}
	timeout := byID(t, j, "hello-world-kavach__wLJvooh")
	if timeout.Status != Error || timeout.Reward != nil || timeout.Arm != "kavach" ||
		timeout.Error != "VerifierTimeoutError: Verifier execution timed out after 120.0 seconds" {
		t.Fatalf("timed out trial %+v", timeout)
	}
	attempts := map[string][]int{}
	for _, tr := range j.Trials {
		attempts[tr.Task] = append(attempts[tr.Task], tr.Attempt)
	}
	for task, n := range attempts {
		if len(n) != 2 || n[0] != 1 || n[1] != 2 {
			t.Errorf("%s attempts %v", task, n)
		}
	}

	fail := byID(t, readJob(t, "nop-fail"), "hello-world-control__p3PHoHF")
	if fail.Status != Fail || *fail.Reward != 0 {
		t.Fatalf("nop trial %+v", fail)
	}
}

func TestStoppedJob(t *testing.T) {
	j := readJob(t, "stopped")
	if j.Status != JobStopped || j.Finished != nil || j.Done != 1 {
		t.Fatalf("job %+v", j)
	}
	if tr := j.Trials[0]; tr.Status != Error || tr.Error != "CancelledError" {
		t.Fatalf("trial %+v", tr)
	}
}

func TestRunningJob(t *testing.T) {
	j := readJob(t, "nop-running")
	if j.Status != JobRunning || j.Done != 0 || j.Total != 1 || j.Finished != nil || j.Stoppable {
		t.Fatalf("job %+v", j)
	}
	tr := j.Trials[0]
	if tr.Status != Running || tr.Started == nil || tr.Finished != nil || tr.Seconds != nil || tr.Steps != 0 {
		t.Fatalf("trial %+v", tr)
	}
}

// A job whose Harbor died without finishing it: running trials are
// interrupted once the job is known to have stopped.
func TestRunningTrialOfStoppedJob(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, filepath.Join(jobsDir, "oracle-both"), filepath.Join(dir, "oracle-both"))
	job := filepath.Join(dir, "oracle-both")
	if err := os.Remove(filepath.Join(job, "hello-world-kavach__aRGYpB6", "result.json")); err != nil {
		t.Fatal(err)
	}
	r := &Reader{Dir: dir}
	j, _ := r.Job("oracle-both")
	if tr := byID(t, j, "hello-world-kavach__aRGYpB6"); tr.Status != Error || tr.Error != "interrupted" {
		t.Fatalf("trial of a finished job %+v", tr)
	}

	// The same trial while the job is still going.
	b, _ := os.ReadFile(filepath.Join(job, "result.json"))
	b = []byte(strings.Replace(string(b), `"finished_at": "2026-10-11T11:20:55.349949"`, `"finished_at": null`, 1))
	if err := os.WriteFile(filepath.Join(job, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	j, _ = r.Job("oracle-both")
	if tr := byID(t, j, "hello-world-kavach__aRGYpB6"); j.Status != JobRunning || tr.Status != Running || j.Done != 3 {
		t.Fatalf("job %s %d done, trial %+v", j.Status, j.Done, tr)
	}
}

func TestOpenCodeJob(t *testing.T) {
	j := readJob(t, "opencode-glm")
	if j.Agent != "opencode" || j.Total != 4 || j.Done != 2 || strings.Join(j.Models, ",") != "openrouter/z-ai/glm-5.3" {
		t.Fatalf("job %+v", j)
	}

	pass := byID(t, j, "redis-10068-kavach__Pass001")
	want := Tokens{Input: 1968602 - 1831936, Cached: 1831936, Output: 34290, Reasoning: 31997}
	if pass.Status != Pass || pass.Attempt != 1 || pass.Steps != 41 || pass.Tokens != want || *pass.Cost != 0.082 || *pass.Seconds != 249 {
		t.Fatalf("passing trial %+v", pass)
	}
	if k := pass.Kavach; k == nil || k.Verdict != "fixed" || k.Old != "invariant_violated(stream-trim-contract)@73" || !strings.HasPrefix(k.Variants, "49 of 64") {
		t.Fatalf("kavach.json %+v", pass.Kavach)
	}
	if o := pass.OpenCode; o == nil || o.URL != "http://127.0.0.1:41234" || o.Session != "ses_ed6b6dacfffe25MOZ8B7cm5kqa" ||
		o.Finished == nil || o.Finished.Sub(*o.Started).Seconds() != 249 || filepath.Base(o.Export) != "opencode-session.json" {
		t.Fatalf("opencode-live.json %+v", pass.OpenCode)
	}

	run := byID(t, j, "redis-10068-control__Run0001")
	want = Tokens{Input: 34487, Cached: 91136, Output: 11555, Reasoning: 11211}
	if run.Status != Running || run.Steps != 8 || run.Tokens != want || run.Cost == nil || *run.Cost < 0.0159 || *run.Cost > 0.0161 {
		t.Fatalf("running trial %+v", run)
	}
	if o := run.OpenCode; o == nil || o.Finished != nil || o.Export != "" || o.URL == "" {
		t.Fatalf("running trial's opencode %+v", run.OpenCode)
	}
	if run.Kavach != nil {
		t.Fatalf("kavach.json without the file: %+v", run.Kavach)
	}

	failed := byID(t, j, "redis-10068-control__Err0001")
	if failed.Status != Error || !strings.Contains(failed.Error, "429 rate limited") || failed.Steps != 1 {
		t.Fatalf("errored trial %+v", failed)
	}

	// Both control trials are attempts of the same task and model; the second
	// kavach attempt has not started.
	if failed.Attempt != 1 || run.Attempt != 2 {
		t.Fatalf("control attempts %d, %d", failed.Attempt, run.Attempt)
	}
	var queued []Trial
	for _, tr := range j.Trials {
		if tr.Status == Queued {
			queued = append(queued, tr)
		}
	}
	if len(queued) != 1 || queued[0].ID != "" || queued[0].Arm != "kavach" || queued[0].Attempt != 2 || queued[0].Model != "openrouter/z-ai/glm-5.3" {
		t.Fatalf("queued %+v", queued)
	}
}

func TestNotFound(t *testing.T) {
	r := &Reader{Dir: jobsDir}
	for _, id := range []string{"..", "../jobs", "nope", ".", ""} {
		if _, err := r.Job(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Job(%q): %v", id, err)
		}
	}
	if _, err := r.Trial("oracle-both", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Trial: %v", err)
	}
	if _, err := r.Steps("oracle-both", "../nop-fail"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Steps: %v", err)
	}
}

func TestSplitArm(t *testing.T) {
	for in, want := range map[string][2]string{
		"redis-10068-kavach":  {"redis-10068", "kavach"},
		"redis-10068-control": {"redis-10068", "control"},
		"hello-world":         {"hello-world", ""},
		"-kavach":             {"-kavach", ""},
		"kavach":              {"kavach", ""},
	} {
		if b, a := SplitArm(in); b != want[0] || a != want[1] {
			t.Errorf("SplitArm(%q) = %q, %q", in, b, a)
		}
	}
}

func TestFindTasks(t *testing.T) {
	tasks, err := FindTasks("testdata/tasks")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Name != "hello-world" || len(tasks[0].Arms) != 2 ||
		tasks[0].Arms[0].Arm != "control" || tasks[0].Arms[1].Task != "hello-world-kavach" ||
		tasks[0].Title != `Create a file called hello.txt with "Hello, world!" as the content.` {
		t.Fatalf("%+v", tasks)
	}
	if tasks, err := FindTasks("testdata/none"); err != nil || len(tasks) != 0 {
		t.Fatalf("missing dir: %v %v", tasks, err)
	}
}

func TestCompare(t *testing.T) {
	rows := Compare(readJob(t, "opencode-glm").Trials)
	if len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
	c, k := rows[0], rows[1]
	if c.Arm != "control" || c.Task != "redis-10068" || c.Attempts != 0 || c.Errors != 1 || c.Pending != 1 || c.MedianSeconds != nil || c.MeanCost != nil {
		t.Fatalf("control %+v", c)
	}
	if k.Arm != "kavach" || k.Passes != 1 || k.Attempts != 1 || k.Pending != 1 || *k.MedianSeconds != 249 ||
		*k.MedianFresh != 1968602-1831936+34290 || *k.MedianCached != 1831936 || *k.MeanCost != 0.082 {
		t.Fatalf("kavach %+v", k)
	}

	rows = Compare(readJob(t, "oracle-both").Trials)
	if len(rows) != 2 || rows[0].Task != "hello-world" || rows[0].Model != "" ||
		rows[0].Passes != 1 || rows[0].Attempts != 1 || rows[0].Errors != 1 || rows[0].MedianFresh == nil ||
		rows[1].Arm != "kavach" || rows[1].Attempts != 0 || rows[1].Errors != 2 {
		t.Fatalf("%+v", rows)
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{3, 1, 2}); m != 2 {
		t.Error(m)
	}
	if m := median([]int64{4, 1, 3, 2}); m != 2 {
		t.Error(m)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := os.CopyFS(to, os.DirFS(from))
	if err != nil {
		t.Fatal(err)
	}
}

func TestPatch(t *testing.T) {
	r := &Reader{Dir: jobsDir}
	p, err := r.Patch("redis-oracle", "redis-10068-control__VVTwpDP")
	if err != nil || !strings.HasPrefix(string(p), "diff --git a/src/t_stream.c b/src/t_stream.c") {
		t.Fatalf("%q %v", p, err)
	}
	if p, err := r.Patch("redis-nop", "redis-10068-control__gtowRJg"); err != nil || len(p) != 0 {
		t.Fatalf("nop patch %q %v", p, err)
	}
	if _, err := r.Patch("oracle-both", "hello-world-control__zRmJZqb"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no patch: %v", err)
	}
}

func TestGrade(t *testing.T) {
	r := &Reader{Dir: jobsDir}
	g, err := r.Grade("redis-nop", "redis-10068-control__gtowRJg")
	if err != nil {
		t.Fatal(err)
	}
	var grade struct {
		Hidden     string `json:"hidden"`
		KavachDiff string `json:"kavach_diff"`
	}
	json.Unmarshal(g.Grade, &grade)
	if grade.Hidden != "fail" || grade.KavachDiff != "invariant_violated(stream-trim-contract)@73" {
		t.Fatalf("grade.json %s", g.Grade)
	}
	if g.Failed != 1 || g.Passed != len(g.Tests)-1 || g.Passed == 0 {
		t.Fatalf("%d passed, %d failed of %d", g.Passed, g.Failed, len(g.Tests))
	}
	for _, tr := range g.Tests {
		if !tr.OK && tr.Name != "XTRIM with MINID option, big delta from master record" {
			t.Errorf("failed test %q", tr.Name)
		}
		if tr.OK && tr.Name == "XTRIM with MINID option" {
			return
		}
	}
	t.Error("no passing XTRIM with MINID option")
}

func TestGradeFromPytest(t *testing.T) {
	dir := t.TempDir()
	trial := filepath.Join(dir, "job", "t__1")
	os.MkdirAll(filepath.Join(trial, "verifier"), 0o755)
	os.WriteFile(filepath.Join(trial, "config.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(trial, "verifier", "grade.json"), []byte(`{"hidden":"fail"}`), 0o644)
	os.WriteFile(filepath.Join(trial, "verifier", "test-stdout.txt"), []byte(
		"=== short test summary info ===\nPASSED ../tests/test_state.py::test_a\nFAILED ../tests/test_state.py::test_b - AssertionError: no\n"), 0o644)
	g, err := (&Reader{Dir: dir}).Grade("job", "t__1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Passed != 1 || g.Failed != 1 || g.Tests[1].Name != "../tests/test_state.py::test_b" {
		t.Fatalf("%+v", g)
	}
	if _, err := (&Reader{Dir: jobsDir}).Grade("oracle-both", "hello-world-control__zRmJZqb"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no grade.json: %v", err)
	}
}

func TestTokensJSON(t *testing.T) {
	b, _ := json.Marshal(Tokens{Input: 100, Cached: 1000, Output: 20, Reasoning: 5})
	if string(b) != `{"input":100,"cached":1000,"output":20,"reasoning":5,"fresh":120}` {
		t.Fatal(string(b))
	}
}

// A job not started by bench, whose folder nobody has written to for
// staleAfter, has lost its Harbor.
func TestStaleJob(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, filepath.Join(jobsDir, "nop-running"), filepath.Join(dir, "nop-running"))
	old := time.Now().Add(-staleAfter - time.Minute)
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		return os.Chtimes(path, old, old)
	})
	j, err := (&Reader{Dir: dir}).Job("nop-running")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != JobStopped || !strings.Contains(j.Error, "stopped writing") || j.Trials[0].Status != Error {
		t.Fatalf("%+v", j)
	}
}

func TestTitleDropsTaskLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instruction.md")
	os.WriteFile(path, []byte("\n# Task: XTRIM MINID deletes entries above the threshold\n\nYou are…\n"), 0o644)
	if got := firstLine(path); got != "XTRIM MINID deletes entries above the threshold" {
		t.Fatalf("%q", got)
	}
}

// Every trial finished but Harbor never marked the job finished.
func TestUnfinishedJobWithAllTrialsDone(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, filepath.Join(jobsDir, "oracle-both"), filepath.Join(dir, "oracle-both"))
	job := filepath.Join(dir, "oracle-both")
	b, _ := os.ReadFile(filepath.Join(job, "result.json"))
	b = []byte(strings.Replace(string(b), `"finished_at": "2026-10-11T11:20:55.349949"`, `"finished_at": null`, 1))
	os.WriteFile(filepath.Join(job, "result.json"), b, 0o644)
	r := &Reader{Dir: dir}
	if j, _ := r.Job("oracle-both"); j.Status != JobRunning {
		t.Fatalf("just written: %s", j.Status)
	}
	old := time.Now().Add(-finishGrace - time.Minute)
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { return os.Chtimes(path, old, old) })
	if j, _ := r.Job("oracle-both"); j.Status != JobStopped || !strings.Contains(j.Error, "after the last trial") {
		t.Fatalf("%+v", j)
	}
}
