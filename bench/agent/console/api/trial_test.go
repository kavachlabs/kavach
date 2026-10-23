package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	modelKey   = "sk-or-v1-visitor-secret-0001"
	daytonaKey = "dtn-visitor-secret-0002"
)

type trialEnv struct {
	t    *testing.T
	s    *Server
	ts   *httptest.Server
	dir  string // holds the fake harbor's output
	jobs string
	cfg  Config
}

// newTrial serves a trial whose Harbor is a script that records its
// environment and arguments, then waits to be stopped.
func newTrialServer(t *testing.T, script string, mutate func(*Config, *TrialConfig)) *trialEnv {
	t.Helper()
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	if script == "" {
		script = "env > " + dir + "/env.txt\necho \"$@\" > " + dir + "/args.txt\ntrap 'exit 130' INT\nwhile :; do sleep 0.05; done\n"
	}
	os.WriteFile(harborCmd, []byte("#!/bin/sh\n[ \"$1\" = --version ] && { echo 'harbor 0.24.0'; exit 0; }\n"+script), 0o755)
	envFile := filepath.Join(dir, "server.env")
	os.WriteFile(envFile, []byte("OPENROUTER_API_KEY=sk-or-v1-server-secret-9999\nDAYTONA_API_KEY=dtn-server-secret-8888\nR2_SECRET_ACCESS_KEY=r2-server-secret-7777\n"), 0o600)
	t.Setenv("R2_ACCESS_KEY_ID", "r2-process-secret-6666")
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"z-ai/glm-5.3"},{"id":"anthropic/claude-sonnet-5.5"},{"id":"~moonshotai/kimi-latest"},{"id":"qwen/qwen3-coder:free"}]}`)
	}))
	t.Cleanup(or.Close)
	cfg := Config{Harbor: harborCmd, JobsDir: copyJobs(t), TasksDir: testdata + "/tasks", EnvFile: envFile, ModelsURL: or.URL}
	tc := &TrialConfig{
		Models: []string{"openrouter/z-ai/glm-5.3"}, MaxAttempts: 2, MaxRunning: 3, InviteRuns: 5, RatePerMin: 1000,
		Showcase: []string{"oracle-both"},
	}
	if mutate != nil {
		mutate(&cfg, tc)
	}
	cfg.Trial = tc
	s, ts := newServer(t, cfg)
	// Runs outlive the console, as they do in production, so a test's fake
	// Harbor and its log helper would still be writing into the temporary
	// directories when they are removed. Cleanups run last first, so this
	// stops them before those directories go.
	t.Cleanup(func() { stopProcesses(dir, cfg.JobsDir) })
	return &trialEnv{t: t, s: s, ts: ts, dir: dir, jobs: cfg.JobsDir, cfg: cfg}
}

// stopProcesses kills every process whose command line names one of dirs,
// and waits for them to go.
func stopProcesses(dirs ...string) {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		left := false
		for _, d := range dirs {
			if exec.Command("pkill", "-KILL", "-f", d).Run() == nil {
				left = true
			}
		}
		if !left {
			return
		}
	}
}

func (e *trialEnv) visitor() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *trialEnv) do(c *http.Client, method, path, body string, header ...string) (int, map[string]any) {
	e.t.Helper()
	code, raw, _ := e.raw(c, method, path, body, header...)
	var v map[string]any
	json.Unmarshal(raw, &v)
	return code, v
}

func (e *trialEnv) raw(c *http.Client, method, path, body string, header ...string) (int, []byte, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (e *trialEnv) setKeys(c *http.Client) {
	e.t.Helper()
	body := `{"model_key":"` + modelKey + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`
	if code, v := e.do(c, "POST", "/api/trial/keys", body); code != 200 || v["has_keys"] != true {
		e.t.Fatalf("keys: %d %v", code, v)
	}
}

const runBody = `{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"attempts":1}`

func (e *trialEnv) invite(c *http.Client) {
	e.t.Helper()
	if code, v := e.do(c, "POST", "/api/trial/invite", `{"code":"alpha-code"}`); code != 200 || v["invite"] != true {
		e.t.Fatalf("invite: %d %v", code, v)
	}
}

func (e *trialEnv) start(c *http.Client) string {
	e.t.Helper()
	code, v := e.do(c, "POST", "/api/jobs", runBody)
	if code != 202 {
		e.t.Fatalf("start: %d %v", code, v)
	}
	return v["id"].(string)
}

func (e *trialEnv) waitFile(name string) string {
	e.t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(e.dir, name)); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	e.t.Fatalf("%s was not written", name)
	return ""
}

func TestTrialSession(t *testing.T) {
	e := newTrialServer(t, "", nil)
	c := e.visitor()

	code, raw, h := e.raw(c, "GET", "/api/trial/session", "")
	var v sessionView
	json.Unmarshal(raw, &v)
	if code != 200 || v.HasKeys || v.Invite || len(v.Runs) != 0 || h.Get("Set-Cookie") != "" ||
		!slices.Equal(v.Limits.Models, []string{"openrouter/z-ai/glm-5.3"}) || v.Limits.MaxAttempts != 2 ||
		!slices.Equal(v.Limits.Tasks, []string{"hello-world-control", "hello-world-kavach"}) {
		t.Fatalf("fresh session: %d %s", code, raw)
	}

	body := `{"model_key":"` + modelKey + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`
	code, raw, h = e.raw(c, "POST", "/api/trial/keys", body)
	cookie := h.Get("Set-Cookie")
	if code != 200 || !strings.HasPrefix(cookie, "kv_session=") || !strings.Contains(cookie, "HttpOnly") ||
		!strings.Contains(cookie, "SameSite=Lax") || strings.Contains(cookie, "Secure") {
		t.Fatalf("keys: %d %s %q", code, raw, cookie)
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(cookie, "kv_session="), ";")
	if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
		t.Fatalf("session id %q", id)
	}
	if strings.Contains(string(raw), modelKey) || strings.Contains(string(raw), daytonaKey) {
		t.Fatalf("keys echoed: %s", raw)
	}
	_, raw, _ = e.raw(c, "GET", "/api/trial/session", "")
	json.Unmarshal(raw, &v)
	if !v.HasKeys || v.Provider != "daytona" || strings.Contains(string(raw), modelKey) {
		t.Fatalf("session: %s", raw)
	}

	// Another visitor does not see them; https makes the cookie Secure.
	c2 := e.visitor()
	_, raw, _ = e.raw(c2, "GET", "/api/trial/session", "")
	json.Unmarshal(raw, &v)
	if v.HasKeys {
		t.Fatalf("keys leaked across sessions: %s", raw)
	}
	_, _, h = e.raw(e.visitor(), "POST", "/api/trial/keys", body, "X-Forwarded-Proto", "https")
	if sc := h.Get("Set-Cookie"); !strings.HasPrefix(sc, "__Host-kv_session=") || !strings.Contains(sc, "Secure") || !strings.Contains(sc, "Path=/") || strings.Contains(sc, "Domain") {
		t.Fatalf("cookie %q is not a __Host- cookie", sc)
	}

	if code, _, _ := e.raw(c, "DELETE", "/api/trial/keys", ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	_, raw, _ = e.raw(c, "GET", "/api/trial/session", "")
	json.Unmarshal(raw, &v)
	if v.HasKeys {
		t.Fatalf("keys remain after delete: %s", raw)
	}

	// An expired session is gone.
	e.setKeys(c)
	e.s.trial.mu.Lock()
	for _, s := range e.s.trial.sessions {
		s.last = time.Now().Add(-25 * time.Hour)
	}
	e.s.trial.mu.Unlock()
	_, raw, _ = e.raw(c, "GET", "/api/trial/session", "")
	json.Unmarshal(raw, &v)
	if v.HasKeys {
		t.Fatalf("idle session survived: %s", raw)
	}
}

func TestTrialKeyValidation(t *testing.T) {
	e := newTrialServer(t, "", nil)
	c := e.visitor()
	long := strings.Repeat("a", 513)
	for _, body := range []string{
		`{"model_key":"` + modelKey + `","provider":"docker"}`,
		`{"model_key":"` + modelKey + `","provider":"daytona"}`,
		`{"model_key":"` + modelKey + `","provider":"modal","modal_token_id":"ak-1234567890abcdefghij"}`,
		`{"model_key":"short","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`,
		`{"model_key":"` + long + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`,
		`{"model_key":"sk-or-v1\nx-0001","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`,
		`{"model_key":"sk-or-v1 x-0001","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`,
		`{"model_key":"` + modelKey + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `","x":1}`,
	} {
		if code, v := e.do(c, "POST", "/api/trial/keys", body); code != 400 {
			t.Errorf("%.80s: %d %v", body, code, v)
		}
	}
	if code, _ := e.do(c, "POST", "/api/trial/keys", `{"model_key":"`+modelKey+`","provider":"modal","modal_token_id":"ak-1234567890abcdefghij","modal_token_secret":"as-1234567890abcdefghij"}`); code != 200 {
		t.Errorf("modal keys: %d", code)
	}
}

func TestTrialRun(t *testing.T) {
	t.Setenv("R2_SECRET_ACCESS_KEY", "r2-parent-secret-5555")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent-secret-4444")
	e := newTrialServer(t, "", nil)
	c := e.visitor()

	if code, _ := e.do(c, "POST", "/api/jobs", runBody); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	e.setKeys(c)
	id := e.start(c)

	env := e.waitFile("env.txt")
	args := e.waitFile("args.txt")
	for _, want := range []string{"OPENROUTER_API_KEY=" + modelKey, "DAYTONA_API_KEY=" + daytonaKey} {
		if !strings.Contains(env, want) {
			t.Errorf("harbor env lacks %s:\n%s", want, env)
		}
	}
	for _, leak := range []string{"r2-parent-secret", "r2-process-secret", "r2-server-secret", "sk-or-v1-server-secret", "dtn-server-secret", "sk-ant-parent-secret"} {
		if strings.Contains(env, leak) {
			t.Errorf("harbor env inherits %s:\n%s", leak, env)
		}
	}
	if strings.Contains(args, "--env-file") {
		t.Errorf("visitor run passed an env file: %s", args)
	}

	var cfg struct {
		JobName     string                   `json:"job_name"`
		Attempts    int                      `json:"n_attempts"`
		Concurrent  int                      `json:"n_concurrent_trials"`
		Retry       struct{ MaxRetries int } `json:"retry"`
		Agents      []map[string]any         `json:"agents"`
		Environment struct{ Type string }    `json:"environment"`
	}
	b, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".json"))
	json.Unmarshal(b, &cfg)
	if cfg.Environment.Type != "daytona" || cfg.Concurrent != 1 || cfg.Retry.MaxRetries != 0 || cfg.Attempts != 1 ||
		len(cfg.Agents) != 1 || cfg.Agents[0]["import_path"] != "bench.agent.harbor.agents:KavachOpenCode" ||
		cfg.Agents[0]["model_name"] != "openrouter/z-ai/glm-5.3" || cfg.Agents[0]["override_timeout_sec"] != nil {
		t.Fatalf("config %s", b)
	}

	// No key reaches the disk, whatever the file.
	filepath.WalkDir(e.jobs, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(modelKey)) || bytes.Contains(b, []byte(daytonaKey)) {
				t.Errorf("%s holds a key", p)
			}
		}
		return nil
	})
	owner, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".owner"))
	_, raw, _ := e.raw(c, "GET", "/api/trial/session", "")
	var v sessionView
	json.Unmarshal(raw, &v)
	if len(owner) != sha256.Size*2 || !slices.Equal(v.Runs, []string{id}) {
		t.Fatalf("owner %q, session %s", owner, raw)
	}

	// One live run per session.
	if code, v := e.do(c, "POST", "/api/jobs", runBody); code != 429 {
		t.Fatalf("second run: %d %v", code, v)
	}
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/stop", ""); code != 202 {
		t.Fatalf("stop: %d", code)
	}
	e.s.runner.Wait(id)
	e.start(c)
}

func TestTrialInviteLimits(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) {
		tc.MaxRunning = 1
		tc.Tasks = []string{"hello-world-kavach"}
		tc.InviteCodes = []string{"alpha-code"}
	})
	c := e.visitor()
	e.invite(c)
	for _, body := range []string{
		`{"tasks":["hello-world-kavach"],"models":["openrouter/anthropic/claude-sonnet-5.5"]}`,
		`{"tasks":["hello-world-control"],"models":["openrouter/z-ai/glm-5.3"]}`,
		`{"tasks":["/etc"],"models":["openrouter/z-ai/glm-5.3"]}`,
		`{"tasks":[],"models":["openrouter/z-ai/glm-5.3"]}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"attempts":3}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"x":1}`,
	} {
		if code, v := e.do(c, "POST", "/api/jobs", body); code != 400 {
			t.Errorf("%s: %d %v", body, code, v)
		}
	}
	if code, _ := e.do(c, "POST", "/api/jobs", runBody, "Origin", "https://evil.example"); code != 403 {
		t.Errorf("cross-origin: %d", code)
	}

	// The caller's own settings are overridden.
	body := `{"tasks":["hello-world-kavach"],"attempts":2,"agent":"nop","environment":"docker","name":"mine","concurrent":9,"retries":5,"timeout_sec":1}`
	code, v := e.do(c, "POST", "/api/jobs", body)
	id, _ := v["id"].(string)
	if code != 202 || id == "mine" || v["environment"] != "daytona" || v["agent"] == "nop" {
		t.Fatalf("forced settings: %d %v", code, v)
	}
	b, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".json"))
	if !strings.Contains(string(b), `"max_retries": 2`) || strings.Contains(string(b), "override_timeout_sec") || strings.Contains(string(b), `"n_concurrent_trials": 9`) {
		t.Errorf("config %s", b)
	}

	// The global cap.
	c2 := e.visitor()
	e.invite(c2)
	if code, v := e.do(c2, "POST", "/api/jobs", runBody); code != 429 {
		t.Errorf("global cap: %d %v", code, v)
	}
}

func TestTrialOwnership(t *testing.T) {
	e := newTrialServer(t, "", nil)
	a, b := e.visitor(), e.visitor()
	e.setKeys(a)
	e.setKeys(b)
	id := e.start(a)

	var job map[string]any
	_, raw, _ := e.raw(b, "GET", "/api/jobs/"+id, "")
	json.Unmarshal(raw, &job)
	if job["stoppable"] != false || job["resumable"] != false || job["retryable"] != false {
		t.Errorf("other visitor sees %v", job)
	}
	_, raw, _ = e.raw(a, "GET", "/api/jobs/"+id, "")
	json.Unmarshal(raw, &job)
	if job["stoppable"] != true {
		t.Errorf("owner sees %v", job)
	}
	if code, _ := e.do(b, "POST", "/api/jobs/"+id+"/stop", ""); code != 403 {
		t.Errorf("stop by other: %d", code)
	}
	if code, _ := e.do(e.visitor(), "POST", "/api/jobs/"+id+"/stop", ""); code != 403 {
		t.Errorf("stop without session: %d", code)
	}

	// The list holds the trial run and the showcase, nothing else.
	_, raw, _ = e.raw(b, "GET", "/api/jobs", "")
	var list []map[string]any
	json.Unmarshal(raw, &list)
	var ids []string
	for _, j := range list {
		ids = append(ids, j["id"].(string))
	}
	slices.Sort(ids)
	if want := []string{"oracle-both", id}; !slices.Equal(ids, want) && !slices.Equal(ids, []string{id, "oracle-both"}) {
		t.Errorf("list %v, want %v", ids, want)
	}
	for _, path := range []string{
		"/api/jobs/nop-fail", "/api/jobs/nop-fail/compare", "/api/trials/opencode-glm/redis-10068-kavach__Pass001",
		"/api/trials/opencode-glm/x/steps", "/api/jobs/stopped",
	} {
		if code, _ := e.do(b, "GET", path, ""); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := e.do(b, "GET", "/api/jobs/oracle-both", ""); code != 200 {
		t.Errorf("showcase job: %d", code)
	}

	for _, r := range [][2]string{
		{"GET", "/api/settings"}, {"POST", "/api/settings"}, {"POST", "/api/jobs/" + id + "/sync"},
		{"GET", "/api/tasks/hello-world-kavach/environment"},
	} {
		if code, _ := e.do(a, r[0], r[1], `{"keys":{}}`); code != 404 {
			t.Errorf("%s %s: %d", r[0], r[1], code)
		}
	}
	_, raw, _ = e.raw(a, "GET", "/api/tasks", "")
	if strings.Contains(string(raw), e.cfg.TasksDir) {
		t.Errorf("tasks name a local path: %s", raw)
	}
	_, raw, _ = e.raw(a, "GET", "/api/status", "")
	if strings.Contains(string(raw), e.cfg.TasksDir) || strings.Contains(string(raw), e.jobs) {
		t.Errorf("status names a local path: %s", raw)
	}
	if !strings.Contains(string(raw), "0.24.0") {
		t.Errorf("status lacks Harbor's version: %s", raw)
	}

	// Ownership survives a restart; the list still shows the run.
	s2 := New(e.cfg)
	if !s2.trial.visible(id) || s2.trial.visible("nop-fail") {
		t.Errorf("owners after restart")
	}

	if code, _ := e.do(a, "POST", "/api/jobs/"+id+"/stop", ""); code != 202 {
		t.Errorf("stop by owner: %d", code)
	}
}

func TestTrialEvents(t *testing.T) {
	e := newTrialServer(t, "", nil)
	a := e.visitor()
	e.setKeys(a)
	id := e.start(a)
	resp, err := a.Get(e.ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	got := string(buf[:n])
	if !strings.Contains(got, `"id": "`+id+`"`) && !strings.Contains(got, `"id":"`+id+`"`) {
		t.Errorf("events lack the run: %s", got)
	}
	if strings.Contains(got, "nop-fail") || !strings.Contains(got, "oracle-both") || !strings.Contains(got, `"stoppable":true`) {
		t.Errorf("events: %s", got)
	}
}

func TestTrialInvite(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) {
		tc.InviteCodes = []string{"alpha-code", "beta-code"}
		tc.InviteRuns = 2
	})
	c := e.visitor()
	if code, _ := e.do(c, "POST", "/api/trial/invite", `{"code":"nope"}`); code != 403 {
		t.Fatalf("bad code: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/jobs", runBody); code != 401 {
		t.Fatalf("no invite: %d", code)
	}
	code, v := e.do(c, "POST", "/api/trial/invite", `{"code":"alpha-code"}`)
	if code != 200 || v["invite"] != true || v["has_keys"] != false || v["provider"] != "daytona" || v["invite_runs_left"] != 2.0 {
		t.Fatalf("invite: %d %v", code, v)
	}
	id := e.start(c)
	env := e.waitFile("env.txt")
	if !strings.Contains(env, "OPENROUTER_API_KEY=sk-or-v1-server-secret-9999") || !strings.Contains(env, "DAYTONA_API_KEY=dtn-server-secret-8888") {
		t.Errorf("invite run lacks the server's keys:\n%s", env)
	}
	if strings.Contains(env, "r2-server-secret") || strings.Contains(env, "r2-process-secret") {
		t.Errorf("invite run inherits R2 keys:\n%s", env)
	}
	if strings.Contains(e.waitFile("args.txt"), "--env-file") {
		t.Errorf("invite run passed the env file")
	}
	e.do(c, "POST", "/api/jobs/"+id+"/stop", "")
	e.s.runner.Wait(id)
	id = e.start(c)
	e.do(c, "POST", "/api/jobs/"+id+"/stop", "")
	e.s.runner.Wait(id)
	if code, v := e.do(c, "POST", "/api/jobs", runBody); code != 403 {
		t.Fatalf("budget: %d %v", code, v)
	}

	// The count survives a restart, and the file holds no code.
	b, err := os.ReadFile(filepath.Join(e.jobs, ".bench", "invites-used.json"))
	if err != nil || bytes.Contains(b, []byte("alpha-code")) {
		t.Fatalf("invites file %q %v", b, err)
	}
	cfg := e.cfg
	cfg.Trial = &TrialConfig{Models: e.cfg.Trial.Models, InviteCodes: []string{"alpha-code", "beta-code"}, InviteRuns: 2}
	_, ts2 := newServer(t, cfg)
	c2 := e.visitor()
	resp, err := c2.Post(ts2.URL+"/api/trial/invite", "application/json", strings.NewReader(`{"code":"alpha-code"}`))
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("after restart: %v %v", resp, err)
	}
	resp, err = c2.Post(ts2.URL+"/api/trial/invite", "application/json", strings.NewReader(`{"code":"beta-code"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("other code after restart: %v %v", resp, err)
	}
}

func TestTrialTurnstile(t *testing.T) {
	var seen []string
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = append(seen, r.Form.Get("secret")+"|"+r.Form.Get("response")+"|"+r.Form.Get("remoteip"))
		json.NewEncoder(w).Encode(map[string]bool{"success": r.Form.Get("response") == "good"})
	}))
	defer verifier.Close()
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) {
		tc.TurnstileSecret, tc.TurnstileSiteKey, tc.TurnstileURL = "ts-secret", "site-key", verifier.URL
	})
	c := e.visitor()
	e.setKeys(c)
	_, raw, _ := e.raw(c, "GET", "/api/trial/session", "")
	if !strings.Contains(string(raw), `"turnstile_site_key": "site-key"`) || strings.Contains(string(raw), "ts-secret") {
		t.Errorf("session: %s", raw)
	}
	body := func(tok string) string {
		return `{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"turnstile_token":"` + tok + `"}`
	}
	if code, _ := e.do(c, "POST", "/api/jobs", runBody); code != 400 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/jobs", body("bad")); code != 403 {
		t.Errorf("bad token: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/jobs", body("good")); code != 202 {
		t.Errorf("good token: %d", code)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[1], "ts-secret|good|127.0.0.1") {
		t.Errorf("verifier saw %v", seen)
	}
}

// An invited run is not checked: the code says who runs, and caps their runs.
func TestTrialTurnstileSkipsInvites(t *testing.T) {
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an invited run was sent to Turnstile")
		json.NewEncoder(w).Encode(map[string]bool{"success": false})
	}))
	defer verifier.Close()
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) {
		tc.TurnstileSecret, tc.TurnstileSiteKey, tc.TurnstileURL = "ts-secret", "site-key", verifier.URL
		tc.Tasks, tc.InviteCodes = []string{"hello-world-kavach"}, []string{"alpha-code"}
	})
	c := e.visitor()
	e.invite(c)
	if _, raw, _ := e.raw(c, "GET", "/api/trial/session", ""); !strings.Contains(string(raw), `"turnstile_site_key": ""`) {
		t.Errorf("an invite session is shown the check: %s", raw)
	}
	e.start(c)
}

func TestTrialRateLimit(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.RatePerMin = 3; tc.TrustCF = true })
	c := e.visitor()
	for i := 0; i < 3; i++ {
		if code, _ := e.do(c, "POST", "/api/trial/invite", `{"code":"x"}`, "CF-Connecting-IP", "1.2.3.4"); code != 403 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code, _, h := e.raw(c, "POST", "/api/trial/invite", `{"code":"x"}`, "CF-Connecting-IP", "1.2.3.4"); code != 429 || h.Get("Retry-After") == "" {
		t.Errorf("over the limit: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/trial/invite", `{"code":"x"}`, "CF-Connecting-IP", "5.6.7.8"); code != 403 {
		t.Errorf("another address: %d", code)
	}
	// Reads are not limited.
	if code, _ := e.do(c, "GET", "/api/trial/session", "", "CF-Connecting-IP", "1.2.3.4"); code != 200 {
		t.Errorf("read: %d", code)
	}
}

func TestTrialOrigin(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.Origin = "https://try.example" })
	c := e.visitor()
	keys := `{"model_key":"` + modelKey + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`
	if code, _ := e.do(c, "POST", "/api/trial/keys", keys, "Origin", "https://try.example"); code != 200 {
		t.Errorf("public origin: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/trial/keys", keys, "Origin", e.ts.URL); code != 200 {
		t.Errorf("same origin: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/trial/keys", keys, "Origin", "https://evil.example"); code != 403 {
		t.Errorf("other origin: %d", code)
	}
	// A public host is fine: no loopback check.
	if code, _ := e.do(c, "GET", "/api/trial/session", "", "Host", "try.example"); code != 200 {
		t.Errorf("host: %d", code)
	}
}
