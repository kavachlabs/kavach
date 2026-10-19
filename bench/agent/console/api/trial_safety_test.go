package api

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTrialBadKeyRunFailsNormally(t *testing.T) {
	// Harbor fails at once and prints the key it was given, as a provider's
	// error might.
	e := newTrialServer(t, "echo \"401 unauthorized for $OPENROUTER_API_KEY\"; exit 1\n", nil)
	c := e.visitor()
	e.setKeys(c)
	id := e.start(c)
	e.s.runner.Wait(id)

	var job map[string]any
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		_, raw, _ := e.raw(c, "GET", "/api/jobs/"+id, "")
		json.Unmarshal(raw, &job)
		if job["status"] == "stopped" {
			break
		}
	}
	if job["status"] != "stopped" || job["error"] != "the model provider rejected the key" {
		t.Fatalf("job %v", job)
	}

	// The key is gone from the log on disk whatever happens to the session:
	// it was redacted as Harbor wrote it.
	log, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".log"))
	if bytes.Contains(log, []byte(modelKey)) || !bytes.Contains(log, []byte("[redacted]")) {
		t.Errorf("log %q", log)
	}
	e.do(c, "DELETE", "/api/trial/keys", "")
	e.s.trial.mu.Lock()
	clear(e.s.trial.sessions)
	e.s.trial.mu.Unlock()
	for _, path := range []string{"/api/jobs", "/api/jobs/" + id, "/api/events"} {
		resp, err := e.visitor().Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1<<16)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if bytes.Contains(buf[:n], []byte(modelKey)) {
			t.Errorf("%s serves the key", path)
		}
	}
	filepath.WalkDir(e.jobs, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(modelKey)) {
				t.Errorf("%s holds the key", p)
			}
		}
		return nil
	})
}

func TestClassifyErrors(t *testing.T) {
	const stale = "Harbor stopped writing to this job 45m0s ago without finishing it"
	for in, want := range map[string]string{
		"":    "",
		stale: stale,
		"litellm.AuthenticationError: OpenrouterException - 401": "the model provider rejected the key",
		"daytona.errors.DaytonaError: 401 Unauthorized":          "the sandbox provider rejected the key",
		"Traceback /Users/op/x.py\nboom\nharbor: exit status 3":  "Harbor exited with an error (code 3)",
		"something odd": "Harbor exited with an error",
		"Downloading  403.3kB/40.21MB\nDownloading  403MB/520.4MB\nharbor: exit status 1": "Harbor exited with an error (code 1)",
		"I've been forbidden from touching authentication.py; permission denied":          "Harbor exited with an error",
		"RuntimeError: docker compose up failed:  c1be Downloading  403.3kB/40.21MB":      "the sandbox failed to start",
		"EnvironmentStartTimeoutError: Environment start timed out after 120s":            "the sandbox timed out starting",
		"httpx: HTTP/1.1 401 Unauthorized from daytona":                                   "the sandbox provider rejected the key",
		"modal.exception.AuthError: Token ID is invalid":                                  "the sandbox provider rejected the key",
		"Error code: 401 - {'error': {'message': 'No auth credentials found'}}":           "the model provider rejected the key",
		"openai.APIStatusError: status code 403":                                          "the model provider rejected the key",
		"Invalid API key provided":                                                        "the model provider rejected the key",
		"OpenrouterException: User not found.":                                            "the model provider rejected the key",
	} {
		if got := classifyJobError(in); got != want {
			t.Errorf("job error %q: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"AgentTimeoutError: ran 3600s in /Users/op/jobs/x": "AgentTimeoutError",
		"CancelledError":                 "CancelledError",
		"interrupted":                    "interrupted",
		"a long free text sentence":      "the attempt failed",
		"RuntimeError: 401 Unauthorized": "the model provider rejected the key",
		"RuntimeError: docker compose up failed: 403MB/520.4MB": "the sandbox failed to start",
	} {
		if got := classifyTrialError(in); got != want {
			t.Errorf("trial error %q: %q, want %q", in, got, want)
		}
	}
}

// A key that is JSON syntax must not change what anyone else is served.
func TestTrialKeyCannotCorruptResponses(t *testing.T) {
	e := newTrialServer(t, "", nil)
	evil := e.visitor()
	body := `{"model_key":"\"status\":\"status\":\"status\":","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`
	if code, v := e.do(evil, "POST", "/api/trial/keys", body); code != 200 {
		t.Fatalf("keys: %d %v", code, v)
	}
	for _, path := range []string{"/api/jobs", "/api/jobs/oracle-both", "/api/trial/session", "/api/tasks"} {
		_, raw, _ := e.raw(e.visitor(), "GET", path, "")
		if !json.Valid(raw) {
			t.Errorf("%s is not JSON: %.200s", path, raw)
		}
	}
	_, raw, _ := e.raw(e.visitor(), "GET", "/api/jobs", "")
	if !strings.Contains(string(raw), `"status"`) {
		t.Errorf("jobs: %s", raw)
	}
}

func TestTrialNoHostPathsInResponses(t *testing.T) {
	root := t.TempDir()
	showcase := []string{"opencode-glm", "redis-oracle", "redis-nop", "oracle-both", "nop-fail", "stopped"}
	e := newTrialServer(t, "", func(cfg *Config, tc *TrialConfig) {
		cfg.Root = root
		tc.Showcase = showcase
	})
	home, _ := os.UserHomeDir()
	forbidden := []string{e.jobs, e.cfg.TasksDir, root, home}
	var paths []string
	for _, id := range showcase {
		paths = append(paths, "/api/jobs/"+id, "/api/jobs/"+id+"/compare")
		var job struct {
			Trials []struct{ ID string } `json:"trials"`
		}
		get(t, e.ts.URL+"/api/jobs/"+id, &job)
		for _, tr := range job.Trials {
			base := "/api/trials/" + id + "/" + tr.ID
			for _, suffix := range []string{"", "/steps", "/patch", "/grade", "/incident", "/variants", "/invariants", "/invariants/source?i=0"} {
				paths = append(paths, base+suffix)
			}
		}
	}
	paths = append(paths, "/api/jobs", "/api/tasks", "/api/status", "/api/trial/session")
	served := 0
	for _, path := range paths {
		_, raw, _ := e.raw(e.visitor(), "GET", path, "")
		served += len(raw)
		for _, f := range forbidden {
			if f != "" && bytes.Contains(raw, []byte(f)) {
				t.Errorf("GET %s names %s", path, f)
			}
		}
	}
	if served == 0 {
		t.Fatal("nothing served")
	}

	// The fields themselves are cleared, not only scrubbed.
	var job struct {
		Trials []struct {
			ID       string
			OpenCode *struct{ URL, Web, Export string } `json:"opencode"`
		} `json:"trials"`
	}
	get(t, e.ts.URL+"/api/jobs/opencode-glm", &job)
	sawOpenCode := false
	for _, jt := range job.Trials {
		if jt.OpenCode == nil {
			continue
		}
		sawOpenCode = true
		if *jt.OpenCode != (struct{ URL, Web, Export string }{}) {
			t.Errorf("%s: opencode %+v", jt.ID, *jt.OpenCode)
		}
	}
	if !sawOpenCode {
		t.Errorf("no trial has an opencode block; the test checks nothing")
	}
}

func TestTrialSessionRules(t *testing.T) {
	e := newTrialServer(t, "", nil)
	keys := `{"model_key":"` + modelKey + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`

	// Entering keys always starts a new session; nothing is carried over.
	c := e.visitor()
	_, _, h := e.raw(c, "POST", "/api/trial/keys", keys)
	first := h.Get("Set-Cookie")
	_, _, h = e.raw(c, "POST", "/api/trial/keys", keys)
	second := h.Get("Set-Cookie")
	if first == "" || second == "" || strings.Split(first, ";")[0] == strings.Split(second, ";")[0] {
		t.Errorf("session id reused: %q %q", first, second)
	}
	if len(h.Values("Set-Cookie")) != 1 {
		t.Errorf("cookies %q", h.Values("Set-Cookie"))
	}
	e.s.trial.mu.Lock()
	n := len(e.s.trial.sessions)
	e.s.trial.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions, want the new one only", n)
	}

	// Using the session refreshes its cookie; having none gets none.
	_, _, h = e.raw(c, "GET", "/api/trial/session", "")
	if sc := h.Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=86400") {
		t.Errorf("no refresh: %q", sc)
	}
	_, _, h = e.raw(e.visitor(), "GET", "/api/trial/session", "")
	if h.Get("Set-Cookie") != "" {
		t.Errorf("a visitor with no session got a cookie")
	}

	// One address holds at most sessionsPerIP sessions; the oldest goes.
	e.s.trial.mu.Lock()
	clear(e.s.trial.sessions)
	e.s.trial.mu.Unlock()
	var ids []string
	for i := 0; i < sessionsPerIP+3; i++ {
		_, _, h := e.raw(e.visitor(), "POST", "/api/trial/keys", keys)
		id, _, _ := strings.Cut(strings.TrimPrefix(h.Get("Set-Cookie"), "kv_session="), ";")
		ids = append(ids, id)
		time.Sleep(time.Millisecond)
	}
	e.s.trial.mu.Lock()
	defer e.s.trial.mu.Unlock()
	if len(e.s.trial.sessions) != sessionsPerIP {
		t.Errorf("%d sessions from one address, want %d", len(e.s.trial.sessions), sessionsPerIP)
	}
	if e.s.trial.sessions[ids[0]] != nil || e.s.trial.sessions[ids[len(ids)-1]] == nil {
		t.Errorf("not the oldest evicted")
	}
}

func TestTrialSessionTableFull(t *testing.T) {
	e := newTrialServer(t, "", nil)
	e.s.trial.mu.Lock()
	for i := 0; i < maxSessions; i++ {
		e.s.trial.sessions[strconv.Itoa(i)] = &session{ip: strconv.Itoa(i), last: time.Now().Add(-time.Duration(i) * time.Second)}
	}
	e.s.trial.mu.Unlock()
	c := e.visitor()
	e.setKeys(c)
	e.s.trial.mu.Lock()
	defer e.s.trial.mu.Unlock()
	if len(e.s.trial.sessions) != maxSessions || e.s.trial.sessions[strconv.Itoa(maxSessions-1)] != nil {
		t.Errorf("the least recently used session was not the one evicted (%d sessions)", len(e.s.trial.sessions))
	}
}

func TestTrialClientIP(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.TrustCF = true })
	for remote, want := range map[string]string{
		"127.0.0.1:5000": "9.9.9.9", // the tunnel on this machine
		"[::1]:5000":     "9.9.9.9",
		"8.8.8.8:5000":   "8.8.8.8", // anyone else: the header is theirs
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("CF-Connecting-IP", "9.9.9.9")
		if got := e.s.trial.clientIP(r); got != want {
			t.Errorf("from %s: %s, want %s", remote, got, want)
		}
	}
}

func TestTrialOneRunPerAddress(t *testing.T) {
	e := newTrialServer(t, "", nil)
	a, b := e.visitor(), e.visitor()
	e.setKeys(a)
	id := e.start(a)
	e.setKeys(b)
	if code, v := e.do(b, "POST", "/api/jobs", runBody); code != 429 || !strings.Contains(v["error"].(string), "address") {
		t.Fatalf("second session, same address: %d %v", code, v)
	}
	e.do(a, "POST", "/api/jobs/"+id+"/stop", "")
	e.s.runner.Wait(id)
	e.start(b)
}

func TestTrialKeyTooShort(t *testing.T) {
	e := newTrialServer(t, "", nil)
	body := `{"model_key":"` + strings.Repeat("k", minKeyLen-1) + `","provider":"daytona","daytona_api_key":"` + daytonaKey + `"}`
	if code, _ := e.do(e.visitor(), "POST", "/api/trial/keys", body); code != 400 {
		t.Errorf("short key: %d", code)
	}
}

func TestTrialInviteProviderNeedsVariables(t *testing.T) {
	// A credentials file in the operator's home is out of reach of a run,
	// which has a home of its own.
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".modal.toml"), []byte("[default]\n"), 0o600)
	t.Setenv("HOME", home)
	e := newTrialServer(t, "", func(cfg *Config, _ *TrialConfig) {
		os.WriteFile(cfg.EnvFile, []byte("OPENROUTER_API_KEY=sk-or-v1-server-secret-9999\n"), 0o600)
	})
	if p := e.s.trial.inviteProvider(); p != "" {
		t.Errorf("invite provider %q from ~/.modal.toml", p)
	}
}
