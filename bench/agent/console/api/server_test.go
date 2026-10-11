package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

const testdata = "../harbor/testdata"

func newServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.JobsDir == "" {
		cfg.JobsDir = testdata + "/jobs"
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = testdata + "/tasks"
	}
	s := New(cfg)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
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
	if code != 202 || v["id"] != "run1" || v["status"] != "running" || v["stoppable"] != true {
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

func TestEvents(t *testing.T) {
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.CopyFS(jobs, os.DirFS(testdata+"/jobs")); err != nil {
		t.Fatal(err)
	}
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
