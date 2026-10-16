package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
	"github.com/kavachlabs/kavach/journal"
)

const testdata = "../harbor/testdata"

func newServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.JobsDir == "" {
		cfg.JobsDir = copyJobs(t)
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = testdata + "/tasks"
	}
	s := New(cfg)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

// copyJobs copies the test jobs, so their files are fresh: a running job
// whose folder has not changed for 45 minutes counts as stopped.
func copyJobs(t *testing.T) string {
	t.Helper()
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.CopyFS(jobs, os.DirFS(testdata+"/jobs")); err != nil {
		t.Fatal(err)
	}
	return jobs
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func post(t *testing.T, url, contentType, body string, header ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestReadEndpoints(t *testing.T) {
	_, ts := newServer(t, Config{})

	var jobs []map[string]any
	if code := get(t, ts.URL+"/api/jobs", &jobs); code != 200 || len(jobs) != 7 {
		t.Fatalf("%d %v", code, jobs)
	}
	for _, j := range jobs {
		if _, ok := j["trials"]; ok {
			t.Fatalf("job list carries trials: %v", j)
		}
	}

	var job harbor.Job
	if code := get(t, ts.URL+"/api/jobs/opencode-glm", &job); code != 200 || len(job.Trials) != 4 || job.Status != harbor.JobRunning {
		t.Fatalf("%d %+v", code, job)
	}
	var rows []harbor.CompareRow
	if code := get(t, ts.URL+"/api/jobs/opencode-glm/compare", &rows); code != 200 || len(rows) != 2 || rows[1].Passes != 1 {
		t.Fatalf("%d %+v", code, rows)
	}
	var trial harbor.Trial
	if code := get(t, ts.URL+"/api/trials/opencode-glm/redis-10068-kavach__Pass001", &trial); code != 200 || trial.Kavach.Verdict != "fixed" {
		t.Fatalf("%d %+v", code, trial)
	}
	var steps []harbor.Step
	if code := get(t, ts.URL+"/api/trials/opencode-glm/redis-10068-control__Run0001/steps", &steps); code != 200 || len(steps) == 0 {
		t.Fatalf("%d %+v", code, steps)
	}
	var raw struct{ Tokens map[string]int64 }
	get(t, ts.URL+"/api/trials/opencode-glm/redis-10068-kavach__Pass001", &raw)
	if raw.Tokens["fresh"] != 1968602-1831936+34290 {
		t.Fatalf("tokens %v", raw.Tokens)
	}
	var grade harbor.Grade
	if code := get(t, ts.URL+"/api/trials/redis-nop/redis-10068-kavach__wRj9pCM/grade", &grade); code != 200 || grade.Failed != 1 {
		t.Fatalf("%d %+v", code, grade)
	}
	resp, err := http.Get(ts.URL + "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd/patch")
	if err != nil {
		t.Fatal(err)
	}
	patch, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(string(patch), "diff --git") {
		t.Fatalf("patch %d %s", resp.StatusCode, patch)
	}

	var tasks []harbor.Task
	if code := get(t, ts.URL+"/api/tasks", &tasks); code != 200 || len(tasks) != 1 || len(tasks[0].Arms) != 2 {
		t.Fatalf("%d %+v", code, tasks)
	}

	for _, path := range []string{
		"/api/jobs/nope", "/api/jobs/nope/compare", "/api/trials/opencode-glm/nope",
		"/api/trials/opencode-glm/nope/steps", "/api/trials/..%2Fopencode-glm/x/steps",
		"/api/trials/oracle-both/hello-world-control__zRmJZqb/patch", "/api/trials/oracle-both/hello-world-control__zRmJZqb/grade",
	} {
		var e map[string]string
		if code := get(t, ts.URL+path, &e); code != 404 || e["error"] == "" {
			t.Errorf("%s: %d %v", path, code, e)
		}
	}
}

func TestStatus(t *testing.T) {
	harborCmd := filepath.Join(t.TempDir(), "harbor")
	os.WriteFile(harborCmd, []byte("#!/bin/sh\necho 0.24.0\n"), 0o755)
	_, ts := newServer(t, Config{Harbor: harborCmd, EnvFile: filepath.Join(t.TempDir(), "missing")})
	var st Status
	if code := get(t, ts.URL+"/api/status", &st); code != 200 {
		t.Fatal(code)
	}
	if !st.Harbor.OK || st.Harbor.Version != "0.24.0" || st.Tasks != 1 || st.EnvFile || st.JobsDir == "" {
		t.Fatalf("%+v", st)
	}
	if st.Docker.OK == (st.Docker.Error != "") {
		t.Fatalf("docker check %+v", st.Docker)
	}
}

func TestModels(t *testing.T) {
	var hits atomic.Int32
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"data":[
			{"id":"nvidia/nemotron-3-ultra","name":"NVIDIA: Nemotron 3 Ultra","context_length":262144,
			 "pricing":{"prompt":"0.0000005","completion":"0.0000022"},"supported_parameters":["tools","temperature"]},
			{"id":"openrouter/auto","name":"Auto Router","context_length":2000000,
			 "pricing":{"prompt":"-1","completion":"-1"},"supported_parameters":[]}]}`))
	}))
	defer or.Close()
	_, ts := newServer(t, Config{ModelsURL: or.URL})

	var models []Model
	get(t, ts.URL+"/api/models?q=NEMOTRON", &models)
	if len(models) != 1 || models[0].ID != "nvidia/nemotron-3-ultra" || *models[0].PriceIn != 0.5 || *models[0].PriceOut != 2.2 ||
		!models[0].Tools || models[0].Context != 262144 {
		t.Fatalf("%+v", models)
	}
	get(t, ts.URL+"/api/models", &models)
	if len(models) != 2 || models[1].PriceIn != nil || models[1].Tools {
		t.Fatalf("%+v", models)
	}
	if hits.Load() != 1 {
		t.Fatalf("fetched %d times, want once an hour", hits.Load())
	}
}

func TestStartAndStopRun(t *testing.T) {
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	os.WriteFile(harborCmd, []byte("#!/bin/sh\ntrap 'exit 130' INT\nwhile :; do sleep 0.05; done\n"), 0o755)
	_, ts := newServer(t, Config{Harbor: harborCmd, JobsDir: filepath.Join(dir, "jobs")})
	url := ts.URL + "/api/jobs"

	body := `{"tasks":["hello-world-control","hello-world-kavach"],"agent":"oracle","name":"run1"}`
	if code, v := post(t, url, "text/plain", body); code != 415 {
		t.Fatalf("text/plain: %d %v", code, v)
	}
	if code, v := post(t, url, "application/json", body, "Origin", "https://example.com"); code != 403 {
		t.Fatalf("cross-origin: %d %v", code, v)
	}
	if code, v := post(t, url, "application/json", `{"tasks":["nope"]}`); code != 400 {
		t.Fatalf("bad task: %d %v", code, v)
	}
	if code, v := post(t, url, "application/json", `{"task":"hello-world-control"}`); code != 400 {
		t.Fatalf("unknown field: %d %v", code, v)
	}

	code, v := post(t, url, "application/json", body, "Origin", ts.URL)
	if trials, ok := v["trials"].([]any); code != 202 || v["id"] != "run1" || v["status"] != "running" || v["stoppable"] != true ||
		!ok || len(trials) != 0 || v["trials_total"] != 2.0 || len(v["tasks"].([]any)) != 2 {
		t.Fatalf("start: %d %v", code, v)
	}
	if code, v := post(t, url, "application/json", body); code != 409 {
		t.Fatalf("same name: %d %v", code, v)
	}
	if code, v := post(t, url+"/run1/stop", "", ""); code != 202 {
		t.Fatalf("stop: %d %v", code, v)
	}
	deadline := time.Now().Add(5 * time.Second)
	var job harbor.Job
	for get(t, url+"/run1", &job); job.Status != harbor.JobStopped && time.Now().Before(deadline); get(t, url+"/run1", &job) {
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != harbor.JobStopped || job.Stoppable {
		t.Fatalf("after stop %+v", job)
	}
	if code, _ := post(t, url+"/run1/stop", "", ""); code != 409 {
		t.Fatalf("second stop: %d", code)
	}
}

func TestHostCheck(t *testing.T) {
	s := New(Config{JobsDir: testdata + "/jobs"})
	for host, want := range map[string]int{"evil.example:4810": 403, "localhost:4810": 200, "127.0.0.1:4810": 200, "[::1]:4810": 200} {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		req.Host = host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", host, w.Code, want)
		}
	}
}

func TestWeb(t *testing.T) {
	web := fstest.MapFS{"index.html": {Data: []byte("<title>kavach bench</title>")}, "app.js": {Data: []byte("export {}")}}
	_, ts := newServer(t, Config{Web: web})
	for path, want := range map[string]string{"/": "<title>kavach bench</title>", "/app.js": "export {}"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != want {
			t.Errorf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
	var jobs []harbor.Job
	if code := get(t, ts.URL+"/api/jobs", &jobs); code != 200 || len(jobs) == 0 {
		t.Fatalf("API behind the web app: %d %v", code, jobs)
	}
	if code, _ := post(t, ts.URL+"/", "application/json", "{}"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example:4810"
	w := httptest.NewRecorder()
	New(Config{Web: web}).ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("web app on a foreign host: %d", w.Code)
	}

	_, bare := newServer(t, Config{})
	if code := get(t, bare.URL+"/", nil); code != 404 {
		t.Fatalf("no web app: / is %d", code)
	}
}

func TestEvents(t *testing.T) {
	jobs := copyJobs(t)
	s, ts := newServer(t, Config{JobsDir: jobs})
	s.events.interval = 20 * time.Millisecond

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatal(ct)
	}
	events := make(chan [2]string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			if n, ok := strings.CutPrefix(line, "event: "); ok {
				name = n
			} else if d, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{name, d}
			}
		}
		close(events)
	}()
	next := func() [2]string {
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		return [2]string{}
	}
	for i := 0; i < 7; i++ {
		if e := next(); e[0] != "job" {
			t.Fatalf("snapshot event %v", e)
		}
	}

	// The running nop trial finishes: its trial and its job change.
	trial := filepath.Join(jobs, "nop-running", "hello-world-control__p3PHoHF")
	result, err := os.ReadFile(filepath.Join(jobs, "nop-fail", "hello-world-control__p3PHoHF", "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the poller take its baseline
	if err := os.WriteFile(filepath.Join(trial, "result.json"), result, 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for len(got) < 2 {
		e := next()
		var v map[string]any
		json.Unmarshal([]byte(e[1]), &v)
		got[e[0]] = v
	}
	if got["trial"]["status"] != "fail" || got["trial"]["id"] != "hello-world-control__p3PHoHF" || got["job"]["id"] != "nop-running" || got["job"]["trials_done"] != 1.0 {
		t.Fatalf("%v", got)
	}
}

func TestPauseAndResumeRun(t *testing.T) {
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	os.WriteFile(harborCmd, []byte("#!/bin/sh\necho \"$@\" >> "+dir+"/args\ntrap 'exit 130' INT\nwhile :; do sleep 0.05; done\n"), 0o755)
	_, ts := newServer(t, Config{Harbor: harborCmd, JobsDir: filepath.Join(dir, "jobs")})
	url := ts.URL + "/api/jobs"
	wait := func(status string) harbor.Job {
		t.Helper()
		var job harbor.Job
		deadline := time.Now().Add(5 * time.Second)
		for get(t, url+"/run1", &job); job.Status != status && time.Now().Before(deadline); get(t, url+"/run1", &job) {
			time.Sleep(20 * time.Millisecond)
		}
		if job.Status != status {
			t.Fatalf("want %s: %+v", status, job)
		}
		return job
	}

	if code, v := post(t, url, "application/json", `{"tasks":["hello-world-control"],"agent":"oracle","name":"run1"}`); code != 202 {
		t.Fatalf("start: %d %v", code, v)
	}
	if code, v := post(t, url+"/run1/resume", "", ""); code != 409 {
		t.Fatalf("resume running: %d %v", code, v)
	}
	if code, v := post(t, ts.URL+"/api/trials/run1/hello-world-control__1/retry", "", ""); code != 409 {
		t.Fatalf("retry while running: %d %v", code, v)
	}
	var args []byte
	for deadline := time.Now().Add(5 * time.Second); strings.Count(string(args), "run -c") < 1 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		args, _ = os.ReadFile(filepath.Join(dir, "args"))
	}
	if code, v := post(t, url+"/run1/pause", "", ""); code != 202 {
		t.Fatalf("pause: %d %v", code, v)
	}
	if job := wait(harbor.JobPaused); !job.Resumable || job.Stoppable {
		t.Fatalf("paused %+v", job)
	}
	if code, v := post(t, url+"/run1/pause", "", ""); code != 409 {
		t.Fatalf("second pause: %d %v", code, v)
	}
	// Paused before Harbor wrote the job folder, the run starts again.
	if code, v := post(t, url+"/run1/resume", "", ""); code != 202 || v["status"] != "running" || v["stoppable"] != true {
		t.Fatalf("resume: %d %v", code, v)
	}
	for deadline := time.Now().Add(5 * time.Second); strings.Count(string(args), "run -c") < 2 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		args, _ = os.ReadFile(filepath.Join(dir, "args"))
	}
	if strings.Count(string(args), "run -c") != 2 {
		t.Fatalf("args %s", args)
	}
	post(t, url+"/run1/stop", "", "")
	wait(harbor.JobStopped)
	if code, v := post(t, ts.URL+"/api/trials/run1/hello-world-control__1/retry", "", ""); code != 404 {
		t.Fatalf("retry a missing attempt: %d %v", code, v)
	}
}

func TestSettings(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "bench", "agent", ".env")
	_, ts := newServer(t, Config{EnvFile: envPath})

	var st SettingsResponse
	if code := get(t, ts.URL+"/api/settings", &st); code != 200 || st.Exists || len(st.Keys) == 0 {
		t.Fatalf("initial settings: %d %+v", code, st)
	}

	code, v := post(t, ts.URL+"/api/settings", "application/json", `{"keys":{"OPENROUTER_API_KEY":"sk-or-v1-secret1234567890"}}`, "Origin", ts.URL)
	if code != 200 || v["exists"] != true {
		t.Fatalf("post settings: %d %v", code, v)
	}
	if code := get(t, ts.URL+"/api/settings", &st); code != 200 || !st.Exists || !st.Keys[0].Set || st.Keys[0].Masked != "sk-or-v1••••7890" {
		t.Fatalf("after save: %d %+v", code, st)
	}
	b, err := os.ReadFile(envPath)
	if err != nil || !strings.Contains(string(b), "OPENROUTER_API_KEY=sk-or-v1-secret1234567890\n") {
		t.Fatalf("env file: %v %q", err, string(b))
	}
}

func TestVariantEndpoints(t *testing.T) {
	jobs := copyJobs(t)
	trial := "/api/trials/opencode-glm/redis-10068-kavach__Pass001"
	rdir := filepath.Join(jobs, "opencode-glm", "redis-10068-kavach__Pass001", "artifacts", "logs", "artifacts", "contract", "rounds", "4")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rdir, "..", "..", "rounds.jsonl"), []byte(`{"round":4,"at":"2026-10-12T15:00:00Z","verdict":"variant_failed(12)@3","changed":1}`+"\n"), 0o644)
	meta := journal.Meta{Service: "httpx-cookies", Start: journal.StartGenesis, Variant: &journal.Variant{ID: 12, Mutation: "input seq 1 delivered twice", Incident: 3, Failure: "invariant: cookie-limits"}}
	recs := []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "traffic", Position: "1", Data: []byte(`{"client":"c2"}`)},
		{Type: journal.TypeClock, Seq: 2, UnixNanos: 1700000000000000000},
		{Type: journal.TypeInput, Seq: 3, Source: "traffic", Position: "1", Data: []byte(`{"client":"c2"}`)},
	}
	if err := journal.WriteFile(filepath.Join(rdir, "variant-12.kavach"), meta, recs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rdir, "variant-12.json"), []byte(`{"status":"invariant_violated","seq":3,"invariant":"cookie-limits","detail":"too many","steps":[{"seq":1,"outputs":[]},{"seq":3,"outputs":[]}]}`), 0o644)
	_, ts := newServer(t, Config{JobsDir: jobs})

	var list harbor.VariantList
	if code := get(t, ts.URL+trial+"/variants", &list); code != 200 || len(list.Rounds) != 1 || len(list.Variants) != 1 ||
		list.Variants[0].Round != 4 || list.Variants[0].ID != 12 || !list.Variants[0].HasResult || list.Variants[0].Variant.Mutation != "input seq 1 delivered twice" {
		t.Fatalf("%d %+v", code, list)
	}
	var none harbor.VariantList
	if code := get(t, ts.URL+"/api/trials/opencode-glm/redis-10068-control__Run0001/variants", &none); code != 200 || len(none.Variants) != 0 || none.Note == "" {
		t.Fatalf("%d %+v", code, none)
	}
	var v harbor.VariantReplay
	if code := get(t, ts.URL+trial+"/variants/4/12", &v); code != 200 || len(v.Steps) != 2 || v.Steps[0].Clock[0] != 1700000000000000000 ||
		v.New == nil || v.New.Verdict != "invariant_violated(cookie-limits)@3" {
		t.Fatalf("%d %+v", code, v)
	}
	for path, want := range map[string]int{
		trial + "/variants/4/3": 404, trial + "/variants/5/12": 404, "/api/trials/opencode-glm/nope/variants": 404,
		"/api/trials/opencode-glm/nope/variants/4/12": 404, "/api/trials/..%2Fopencode-glm/x/variants": 404,
		trial + "/variants/x/12": 400, trial + "/variants/4/1.5": 400, trial + "/variants/-1/12": 400,
	} {
		var e map[string]string
		if code := get(t, ts.URL+path, &e); code != want || e["error"] == "" {
			t.Errorf("%s: %d %v", path, code, e)
		}
	}
}

func TestInvariantSource(t *testing.T) {
	tasks := t.TempDir()
	task := filepath.Join(tasks, "redis-10068-kavach")
	integration := "diff --git a/src/kavach.c b/src/kavach.c\n--- /dev/null\n+++ b/src/kavach.c\n@@ -0,0 +1,3 @@\n+static const kavach_invariant invariants[] = {\n+    {\"stream-trim-contract\", trimInvariant, NULL},\n+};\n"
	for name, text := range map[string]string{
		"task.toml":                          "",
		"environment/docker-compose.yaml":    "services:\n  main:\n    build: ../docker\n",
		"docker/Dockerfile":                  "FROM scratch\n",
		"docker/redis-kavach.patch":          integration,
		"docker/not-a-source.txt":            "secret\n",
		"../redis-10068-control/task.toml":   "",
		"../redis-10068-control/secret.diff": "secret\n",
	} {
		p := filepath.Join(task, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs := copyJobs(t)
	_, ts := newServer(t, Config{JobsDir: jobs, TasksDir: tasks})
	trial := "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd"

	var inv harbor.Invariants
	if code := get(t, ts.URL+trial+"/invariants", &inv); code != 200 || len(inv.Sources) != 2 {
		t.Fatalf("%d %+v", code, inv)
	}
	agentPatch := filepath.Join(jobs, "redis-oracle", "redis-10068-kavach__Dw5kmkd", "artifacts", "logs", "artifacts", "patch.diff")
	if want := filepath.Join(task, "docker", "redis-kavach.patch"); inv.Sources[0] != want || inv.Sources[1] != agentPatch {
		t.Fatalf("sources %q, want %q and %q", inv.Sources, want, agentPatch)
	}
	if len(inv.Invariants) == 0 || len(inv.Invariants[0].Code) == 0 {
		t.Fatalf("invariants %+v", inv.Invariants)
	}
	if c := inv.Invariants[0].Code[0]; c.File != inv.Sources[0] || c.FileLine != 5 || c.FileHit != 6 {
		t.Errorf("snippet %+v", c)
	}

	for i, want := range []string{integration, ""} {
		resp, err := http.Get(ts.URL + trial + "/invariants/source?i=" + string(rune('0'+i)))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want == "" {
			want, _ := os.ReadFile(agentPatch)
			if string(b) != string(want) {
				t.Errorf("source 1 is not the agent's patch: %.80q", b)
			}
		} else if string(b) != want {
			t.Errorf("source %d: %q", i, b)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("source %d: %d %s", i, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}

	for path, want := range map[string]int{
		trial + "/invariants/source?i=2":                                                                     404,
		trial + "/invariants/source?i=99":                                                                    404,
		"/api/trials/oracle-both/hello-world-control__zRmJZqb/invariants/source?i=0":                         404,
		"/api/trials/redis-oracle/nope/invariants/source?i=0":                                                404,
		"/api/trials/..%2Fredis-oracle/x/invariants/source?i=0":                                              404,
		trial + "/invariants/source":                                                                         400,
		trial + "/invariants/source?i=-1":                                                                    400,
		trial + "/invariants/source?i=x":                                                                     400,
		trial + "/invariants/source?i=1.5":                                                                   400,
		trial + "/invariants/source?i=../../docker/not-a-source.txt":                                         400,
		trial + "/invariants/source?i=" + url.QueryEscape(filepath.Join(task, "docker", "not-a-source.txt")): 400,
	} {
		var e map[string]string
		if code := get(t, ts.URL+path, &e); code != want || e["error"] == "" {
			t.Errorf("%s: %d %v", path, code, e)
		}
	}
}

// fakeDocker is a Docker whose /work holds files, and records what ran.
type fakeDocker struct {
	running   bool
	created   time.Time
	files     map[string]string
	started   int
	removed   int
	applied   []string
	failApply bool
}

func (f *fakeDocker) Running(string) bool { return f.running }
func (f *fakeDocker) ImageCreated(image string) (time.Time, error) {
	if f.created.IsZero() {
		return time.Time{}, errors.New("no such image: " + image)
	}
	return f.created, nil
}
func (f *fakeDocker) Start(string) (string, error) { f.started++; return "c1", nil }
func (f *fakeDocker) Remove(string)                { f.removed++ }
func (f *fakeDocker) Exec(_ string, stdin []byte, args ...string) ([]byte, error) {
	if args[0] == "cat" {
		if s, ok := f.files[strings.TrimPrefix(args[len(args)-1], "/work/")]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	if slices.Contains(args, "apply") {
		f.applied = append(f.applied, string(stdin))
		if f.failApply {
			return nil, errors.New("patch does not apply")
		}
	}
	return nil, nil
}

func TestInvariantRealSource(t *testing.T) {
	tasks := t.TempDir()
	task := filepath.Join(tasks, "redis-10068-kavach")
	integration := "diff --git a/src/kavach.c b/src/kavach.c\n--- /dev/null\n+++ b/src/kavach.c\n@@ -0,0 +1,3 @@\n+static const kavach_invariant invariants[] = {\n+    {\"stream-trim-contract\", trimInvariant, NULL},\n+};\n"
	for name, text := range map[string]string{
		"task.toml":                                        "",
		"environment/docker-compose.yaml":                  "services:\n  main:\n    build:\n      context: .\n      dockerfile: tasks/redis-10068/Dockerfile\n      target: kavach\n",
		"environment/tasks/redis-10068/Dockerfile":         "FROM scratch\n",
		"environment/tasks/redis-10068/redis-kavach.patch": integration,
	} {
		p := filepath.Join(task, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := "static const kavach_invariant invariants[] = {\n    {\"stream-trim-contract\", trimInvariant, NULL},\n};\n"
	fd := &fakeDocker{created: time.Now().Add(24 * time.Hour), files: map[string]string{"src/kavach.c": file}}
	cache := t.TempDir()
	_, ts := newServer(t, Config{JobsDir: copyJobs(t), TasksDir: tasks, Docker: fd, CacheDir: cache})
	trial := "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd"

	var inv harbor.Invariants
	if code := get(t, ts.URL+trial+"/invariants", &inv); code != 200 {
		t.Fatalf("%d", code)
	}
	if inv.Source != "image" || !strings.Contains(inv.SourceNote, "kavach-bench/redis-10068:kavach was built") || !slices.Equal(inv.Files, []string{"src/kavach.c"}) {
		t.Fatalf("source %q note %q files %q", inv.Source, inv.SourceNote, inv.Files)
	}
	c := inv.Invariants[0].Code[0]
	if c.SrcFile != filepath.Join(cache, "redis-oracle", "redis-10068-kavach__Dw5kmkd", "image", "src", "kavach.c") || c.SrcLine != 1 || c.SrcHit != 2 {
		t.Errorf("snippet %+v", c)
	}
	if b, err := os.ReadFile(c.SrcFile); err != nil || string(b) != file {
		t.Errorf("cached file %q %v", b, err)
	}
	if len(fd.applied) != 1 || !strings.Contains(fd.applied[0], "t_stream.c") || fd.started != 1 || fd.removed != 1 {
		t.Errorf("docker: applied %d started %d removed %d", len(fd.applied), fd.started, fd.removed)
	}

	// A second request is served from the cache, with the same note.
	var again harbor.Invariants
	get(t, ts.URL+trial+"/invariants", &again)
	if fd.started != 1 || again.Source != "image" || again.SourceNote != inv.SourceNote {
		t.Errorf("second request: started %d %q %q", fd.started, again.Source, again.SourceNote)
	}

	resp, err := http.Get(ts.URL + trial + "/invariants/file?path=src/kavach.c")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != file {
		t.Errorf("file: %d %q", resp.StatusCode, b)
	}
	for path, want := range map[string]int{
		trial + "/invariants/file":                                   400,
		trial + "/invariants/file?path=":                             400,
		trial + "/invariants/file?path=src/t_stream.c":               404, // in a patch, not fetched
		trial + "/invariants/file?path=src/other.c":                  404,
		trial + "/invariants/file?path=../kavach.c":                  404,
		trial + "/invariants/file?path=src/../src/kavach.c":          404,
		trial + "/invariants/file?path=/etc/passwd":                  404,
		trial + "/invariants/file?path=" + url.QueryEscape(cache):    404,
		"/api/trials/redis-oracle/nope/invariants/file?path=a":       404,
		"/api/trials/..%2Fredis-oracle/x/invariants/file?path=src/a": 404,
	} {
		var e map[string]string
		if code := get(t, ts.URL+path, &e); code != want || e["error"] == "" {
			t.Errorf("%s: %d %v", path, code, e)
		}
	}

	// A running container is read instead, and nothing is started. The agent
	// has added a line above the code since the patch was written; the
	// snippet is found where it moved to.
	fd.running = true
	fd.files["src/kavach.c"] = "// live\n" + file
	var live harbor.Invariants
	get(t, ts.URL+trial+"/invariants", &live)
	if lc := live.Invariants[0].Code[0]; live.Source != "container" || fd.started != 1 || lc.SrcLine != 2 || lc.SrcHit != 3 {
		t.Errorf("live: %q started %d %+v", live.Source, fd.started, lc)
	}
	fd.running = false

	// Falling back: no image, an image that rejects the patch, no Docker.
	for name, mod := range map[string]func(){
		"missing": func() { fd.created = time.Time{}; os.RemoveAll(cache) },
		"apply":   func() { fd.created = time.Now(); fd.failApply = true; os.RemoveAll(cache) },
	} {
		mod()
		var fb harbor.Invariants
		get(t, ts.URL+trial+"/invariants", &fb)
		if fb.Source != "patch" || fb.Invariants[0].Code[0].SrcFile != "" || !strings.Contains(fb.SourceNote, "docker build -f") {
			t.Errorf("%s: %q %q", name, fb.Source, fb.SourceNote)
		}
	}
}
