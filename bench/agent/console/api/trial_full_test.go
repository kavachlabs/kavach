package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// callsScript records every invocation of the fake Harbor, with its
// environment, then waits to be stopped.
const callsScript = `D=$(dirname "$0")
echo "CALL $@" >> "$D/calls.txt"
env >> "$D/calls.txt"
echo END >> "$D/calls.txt"
trap 'exit 130' INT
while :; do sleep 0.05; done
`

// call waits for the n-th invocation of the fake Harbor and returns what it
// recorded.
func (e *trialEnv) call(n int) string {
	e.t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(filepath.Join(e.dir, "calls.txt"))
		if parts := strings.SplitAfter(string(b), "END\n"); len(parts) > n {
			return parts[n-1]
		}
	}
	e.t.Fatalf("harbor was not run %d times", n)
	return ""
}

func (e *trialEnv) calls() int {
	b, _ := os.ReadFile(filepath.Join(e.dir, "calls.txt"))
	return strings.Count(string(b), "END\n")
}

func (e *trialEnv) stop(c *http.Client, id string) {
	e.t.Helper()
	if code, v := e.do(c, "POST", "/api/jobs/"+id+"/stop", ""); code != 202 {
		e.t.Fatalf("stop: %d %v", code, v)
	}
	e.s.runner.Wait(id)
}

// ownServer is a trial with small caps, whose invite tasks are one arm, and
// a client holding its own keys.
func ownServer(t *testing.T, script string) (*trialEnv, *http.Client) {
	t.Helper()
	e := newTrialServer(t, script, func(_ *Config, tc *TrialConfig) {
		tc.Tasks = []string{"hello-world-kavach"}
		tc.InviteCodes = []string{"alpha-code"}
		tc.OwnMaxAttempts, tc.OwnMaxConcurrent, tc.OwnMaxTrials = 3, 4, 8
	})
	c := e.visitor()
	e.setKeys(c)
	return e, c
}

func TestTrialOwnKeysRunForm(t *testing.T) {
	e, c := ownServer(t, callsScript)

	_, raw, _ := e.raw(c, "GET", "/api/trial/session", "")
	if !strings.Contains(string(raw), `"max_concurrent": 4`) || !strings.Contains(string(raw), "hello-world-control") {
		t.Errorf("session lacks the own limits and every task: %s", raw)
	}
	_, raw, _ = e.raw(c, "GET", "/api/tasks", "")
	if !strings.Contains(string(raw), "hello-world-control") || strings.Contains(string(raw), e.cfg.TasksDir) {
		t.Errorf("tasks: %s", raw)
	}

	const ok = `"models":["openrouter/z-ai/glm-5.3"]`
	for _, body := range []string{
		`{"tasks":["hello-world-kavach"],"models":["openrouter/nope/nothing"]}`,
		`{"tasks":["hello-world-kavach"],"models":["anthropic/claude-sonnet-5.5"]}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3 --x"]}`,
		`{"tasks":["hello-world-kavach"]}`,
		`{"tasks":["hello-world-kavach"],"agent":"oracle",` + ok + `}`,
		`{"tasks":["hello-world-kavach"],"agent":"x.y:Evil",` + ok + `}`,
		`{"tasks":["/etc"],` + ok + `}`,
		`{"tasks":["nope"],` + ok + `}`,
		`{"tasks":[],` + ok + `}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"attempts":4}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"attempts":-1}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"concurrent":5}`,
		`{"tasks":["hello-world-kavach","hello-world-control"],` + ok + `,"attempts":3,"retries":2,"concurrent":2}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"retries":3}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"timeout_sec":7201}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"retries":6}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"environment":"docker"}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"environment":"modal"}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"environment":"vercel"}`,
		`{"tasks":["hello-world-kavach"],` + ok + `,"env":{"A":"b"}}`,
	} {
		if code, v := e.do(c, "POST", "/api/jobs", body); code != 400 {
			t.Errorf("%s: %d %v", body, code, v)
		}
	}
	if n := e.calls(); n != 0 {
		t.Fatalf("a refused request started harbor %d times", n)
	}

	// Anything within the caps starts: another arm than the invite's, a model
	// the invite does not offer, every option of the form.
	body := `{"tasks":["hello-world-control","hello-world-kavach"],"models":["openrouter/anthropic/claude-sonnet-5.5","openrouter/~moonshotai/kimi-latest"],` +
		`"attempts":1,"concurrent":4,"retries":1,"timeout_sec":1800,"environment":"daytona","agent":"opencode","name":"mine"}`
	code, v := e.do(c, "POST", "/api/jobs", body)
	id, _ := v["id"].(string)
	if code != 202 || id == "mine" || v["environment"] != "daytona" {
		t.Fatalf("start: %d %v", code, v)
	}
	b, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".json"))
	for _, want := range []string{`"n_concurrent_trials": 4`, `"max_retries": 1`, `"override_timeout_sec": 1800`, `"type": "daytona"`,
		`openrouter/anthropic/claude-sonnet-5.5`, `openrouter/~moonshotai/kimi-latest`, "hello-world-control", `"import_path": "bench.agent.harbor.agents:KavachOpenCode"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config lacks %s:\n%s", want, b)
		}
	}
	env := e.call(1)
	if !strings.Contains(env, "OPENROUTER_API_KEY="+modelKey) || !strings.Contains(env, "DAYTONA_API_KEY="+daytonaKey) || strings.Contains(env, "server-secret") || strings.Contains(env, "--env-file") {
		t.Errorf("harbor env:\n%s", env)
	}
	e.stop(c, id)

	// An agent without a model needs no model key.
	code, v = e.do(c, "POST", "/api/jobs", `{"tasks":["hello-world-control"],"agent":"oracle"}`)
	if code != 202 {
		t.Fatalf("oracle: %d %v", code, v)
	}
	if env := e.call(2); strings.Contains(env, "OPENROUTER_API_KEY") || !strings.Contains(env, "DAYTONA_API_KEY="+daytonaKey) {
		t.Errorf("oracle env:\n%s", env)
	}
	e.stop(c, v["id"].(string))

	// One live run per session still holds.
	code, v = e.do(c, "POST", "/api/jobs", `{"tasks":["hello-world-control"],"agent":"nop"}`)
	if code != 202 {
		t.Fatal(code, v)
	}
	if code, _ := e.do(c, "POST", "/api/jobs", `{"tasks":["hello-world-control"],"agent":"nop"}`); code != 429 {
		t.Errorf("second live run: %d", code)
	}
	e.stop(c, v["id"].(string))
}

func TestTrialOwnCapsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trial.json")
	os.WriteFile(path, []byte(`{"invites":[],"own_max_attempts":2,"own_max_concurrent":1,"own_max_trials":5}`), 0o600)
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.ConfigPath = path; tc.OwnMaxAttempts = 9 })
	c := e.visitor()
	e.setKeys(c)
	for _, body := range []string{
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"attempts":3}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"concurrent":2}`,
	} {
		if code, v := e.do(c, "POST", "/api/jobs", body); code != 400 {
			t.Errorf("%s: %d %v", body, code, v)
		}
	}
	// Unset, the caps are 10 attempts, 4 at once, 40 attempts in all.
	e2 := newTrialServer(t, "", nil)
	if l := e2.s.trial.settings().own; l.MaxAttempts != 10 || l.MaxConcurrent != 4 || l.MaxTrials != 40 || l.MaxTimeoutMin != 120 || l.MaxRetries != 2 || l.MaxRetryReqs != 5 {
		t.Errorf("defaults %+v", l)
	}
	if l := TrialFromEnv(func(k string) string {
		return map[string]string{"TRIAL_OWN_MAX_ATTEMPTS": "7", "TRIAL_OWN_MAX_CONCURRENT": "3", "TRIAL_OWN_MAX_TRIALS": "20"}[k]
	}); l.OwnMaxAttempts != 7 || l.OwnMaxConcurrent != 3 || l.OwnMaxTrials != 20 {
		t.Errorf("env %+v", l)
	}
}

// startOwn starts a run on the cheapest request and returns its id.
func (e *trialEnv) startOwn(c *http.Client) string {
	e.t.Helper()
	code, v := e.do(c, "POST", "/api/jobs", `{"tasks":["hello-world-control"],"models":["openrouter/z-ai/glm-5.3"]}`)
	if code != 202 {
		e.t.Fatalf("start: %d %v", code, v)
	}
	return v["id"].(string)
}

func TestTrialOwnKeysPauseResumeRetry(t *testing.T) {
	e, c := ownServer(t, callsScript)
	id := e.startOwn(c)
	e.call(1)

	_, raw, _ := e.raw(c, "GET", "/api/jobs/"+id, "")
	if json := string(raw); !strings.Contains(json, `"stoppable": true`) {
		t.Fatalf("owner: %s", raw)
	}
	// Someone else's session may neither see the controls nor use them.
	other := e.visitor()
	e.setKeys(other)
	_, raw, _ = e.raw(other, "GET", "/api/jobs/"+id, "")
	if s := string(raw); strings.Contains(s, `"resumable": true`) || strings.Contains(s, `"stoppable": true`) {
		t.Errorf("other session sees controls: %s", raw)
	}
	for _, r := range [][2]string{{"POST", "/api/jobs/" + id + "/pause"}, {"POST", "/api/jobs/" + id + "/resume"}, {"POST", "/api/trials/" + id + "/x/retry"}} {
		for name, cl := range map[string]*http.Client{"other": other, "no session": e.visitor()} {
			if code, _ := e.do(cl, r[0], r[1], ""); code != 404 {
				t.Errorf("%s %s by %s: %d", r[0], r[1], name, code)
			}
		}
	}
	if code, _ := e.do(other, "POST", "/api/jobs/"+id+"/stop", ""); code != 403 {
		t.Errorf("stop by other: %d", code)
	}

	// Resume while it runs is refused (a live run is the one allowed); pause stops it and marks it paused.
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 429 {
		t.Errorf("resume while running: %d", code)
	}
	if code, v := e.do(c, "POST", "/api/jobs/"+id+"/pause", ""); code != 202 {
		t.Fatalf("pause: %d %v", code, v)
	}
	e.s.runner.Wait(id)
	_, raw, _ = e.raw(c, "GET", "/api/jobs/"+id, "")
	if s := string(raw); !strings.Contains(s, `"status": "paused"`) || !strings.Contains(s, `"resumable": true`) {
		t.Fatalf("paused: %s", raw)
	}

	// Resume starts Harbor again on the session's keys, never the server's.
	if code, v := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 202 {
		t.Fatalf("resume: %d %v", code, v)
	}
	second := e.call(2)
	if !strings.Contains(second, "OPENROUTER_API_KEY="+modelKey) || !strings.Contains(second, "DAYTONA_API_KEY="+daytonaKey) {
		t.Errorf("resume env lacks the session's keys:\n%s", second)
	}
	for _, leak := range []string{"server-secret", "r2-", "--env-file", "HOME=" + os.Getenv("HOME") + "\n"} {
		if strings.Contains(second, leak) {
			t.Errorf("resume leaks %q:\n%s", leak, second)
		}
	}
	e.stop(c, id)

	// With the folder Harbor writes, resume is `harbor jobs resume`; retry
	// does the same for one attempt.
	dir := filepath.Join(e.jobs, id)
	os.MkdirAll(filepath.Join(dir, id+"__A"), 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"job_name":"`+id+`"}`), 0o644)
	os.WriteFile(filepath.Join(dir, id+"__A", "config.json"), []byte("{}"), 0o644)
	if code, v := e.do(c, "POST", "/api/trials/"+id+"/"+id+"__A/retry", ""); code != 202 {
		t.Fatalf("retry: %d %v", code, v)
	}
	third := e.call(3)
	if !strings.Contains(third, "CALL jobs resume -p "+dir) || !strings.Contains(third, "OPENROUTER_API_KEY="+modelKey) || strings.Contains(third, "server-secret") {
		t.Errorf("retry call:\n%s", third)
	}
	if code, _ := e.do(c, "POST", "/api/trials/"+id+"/nope/retry", ""); code != 429 {
		t.Errorf("retry while running: %d", code)
	}
	if e.s.trial.jobSecrets[id] == nil {
		t.Errorf("no secrets kept for the resumed run")
	}
	e.stop(c, id)
	if code, _ := e.do(c, "POST", "/api/trials/"+id+"/nope/retry", ""); code != 404 {
		t.Errorf("retry of a missing attempt: %d", code)
	}
}

func TestTrialOwnKeysResumeNeedsKeys(t *testing.T) {
	e, c := ownServer(t, callsScript)
	id := e.startOwn(c)
	e.call(1)
	e.stop(c, id)

	for name, forget := range map[string]func(){
		"forgotten": func() { e.do(c, "DELETE", "/api/trial/keys", "") },
		"expired": func() {
			e.s.trial.mu.Lock()
			for _, s := range e.s.trial.sessions {
				s.keys = nil
			}
			e.s.trial.mu.Unlock()
		},
	} {
		e.setKeys(c)
		forget()
		code, v := e.do(c, "POST", "/api/jobs/"+id+"/resume", "")
		if msg, _ := v["error"].(string); code != 409 || !strings.Contains(msg, "enter your keys again to resume") {
			t.Errorf("%s: resume without keys: %d %v", name, code, v)
		}
		if code, _ := e.do(c, "POST", "/api/trials/"+id+"/x/retry", ""); code != 409 {
			t.Errorf("%s: retry without keys: %d", name, code)
		}
		if n := e.calls(); n != 1 {
			t.Fatalf("%s: harbor ran again (%d calls) without the session's keys", name, n)
		}
		// The run is still the session's: the controls show, and refuse.
		_, raw, _ := e.raw(c, "GET", "/api/jobs/"+id, "")
		if !strings.Contains(string(raw), `"resumable": true`) {
			t.Errorf("%s: %s", name, raw)
		}
	}

	// Keys for the other sandbox cannot resume a run that used the first.
	e.do(c, "DELETE", "/api/trial/keys", "")
	body := `{"model_key":"` + modelKey + `","provider":"modal","modal_token_id":"ak-visitor-token-id-0003","modal_token_secret":"as-visitor-token-secret-0004"}`
	if code, v := e.do(c, "POST", "/api/trial/keys", body); code != 200 {
		t.Fatal(code, v)
	}
	if code, v := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 409 || !strings.Contains(v["error"].(string), "daytona") {
		t.Errorf("resume on other sandbox: %d %v", code, v)
	}
	if n := e.calls(); n != 1 {
		t.Fatalf("harbor ran again: %d", n)
	}
}

func TestTrialOwnKeysLimitsStillApply(t *testing.T) {
	e, c := ownServer(t, callsScript)
	id := e.startOwn(c)
	e.call(1)
	e.stop(c, id)
	id2 := e.startOwn(c)
	e.call(2)

	// A second live run, by resume, is the same as by start.
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 429 {
		t.Errorf("resume beside a live run: %d", code)
	}
	e.stop(c, id2)
	// Another address cannot resume it; neither can one over the global cap.
	e.s.trial.cfg.MaxRunning = 1
	other := e.visitor()
	e.setKeys(other)
	idO := e.startOwn(other)
	e.call(3)
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 429 {
		t.Errorf("resume over the global cap: %d", code)
	}
	e.stop(other, idO)
}

func TestTrialInviteSessionStaysLimited(t *testing.T) {
	e, _ := ownServer(t, callsScript)
	c := e.visitor()
	e.invite(c)
	for _, body := range []string{
		`{"tasks":["hello-world-control"],"models":["openrouter/z-ai/glm-5.3"]}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/anthropic/claude-sonnet-5.5"]}`,
		`{"tasks":["hello-world-kavach"],"models":["openrouter/z-ai/glm-5.3"],"attempts":3}`,
	} {
		if code, v := e.do(c, "POST", "/api/jobs", body); code != 400 {
			t.Errorf("%s: %d %v", body, code, v)
		}
	}
	id := e.start(c)
	e.call(1)
	env := e.call(1)
	if !strings.Contains(env, "sk-or-v1-server-secret-9999") || strings.Contains(env, modelKey) {
		t.Errorf("invite env:\n%s", env)
	}
	_, raw, _ := e.raw(c, "GET", "/api/trial/session", "")
	if strings.Contains(string(raw), "hello-world-control") || strings.Contains(string(raw), `"own"`) {
		t.Errorf("invite session sees the own-key limits: %s", raw)
	}
	e.stop(c, id)
	dir := filepath.Join(e.jobs, id)
	os.MkdirAll(filepath.Join(dir, id+"__A"), 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"job_name":"`+id+`"}`), 0o644)
	os.WriteFile(filepath.Join(dir, id+"__A", "config.json"), []byte("{}"), 0o644)
	_, raw, _ = e.raw(c, "GET", "/api/jobs/"+id, "")
	if s := string(raw); strings.Contains(s, `"resumable": true`) || !strings.Contains(s, `"retryable": true`) {
		t.Errorf("invite owner may retry, not resume: %s", raw)
	}
	for _, r := range [][2]string{{"POST", "/api/jobs/" + id + "/pause"}, {"POST", "/api/jobs/" + id + "/resume"}} {
		if code, _ := e.do(c, r[0], r[1], ""); code != 404 {
			t.Errorf("%s %s: %d", r[0], r[1], code)
		}
	}
	// Another session may not retry it.
	other := e.visitor()
	e.invite(other)
	if code, _ := e.do(other, "POST", "/api/trials/"+id+"/"+id+"__A/retry", ""); code != 404 {
		t.Errorf("another invite session retried: %d", code)
	}
	_, before, _ := e.raw(c, "GET", "/api/trial/session", "")
	// The owner retries an attempt on the operator's keys, using no run.
	if code, v := e.do(c, "POST", "/api/trials/"+id+"/"+id+"__A/retry", ""); code != 202 {
		t.Fatalf("retry: %d %v", code, v)
	}
	if env := e.call(2); !strings.Contains(env, "sk-or-v1-server-secret-9999") || !strings.Contains(env, "jobs resume") {
		t.Errorf("retry env:\n%s", env)
	}
	if _, after, _ := e.raw(c, "GET", "/api/trial/session", ""); string(after) != string(before) {
		t.Errorf("a retry used an invite run:\n%s\n%s", before, after)
	}
	e.stop(c, id)
	if n := e.calls(); n != 2 {
		t.Errorf("harbor ran %d times", n)
	}
}

// What stays locked for everyone, whoever asks and whatever the keys.
func TestTrialLockedRoutes(t *testing.T) {
	e, own := ownServer(t, callsScript)
	invited := e.visitor()
	e.invite(invited)
	id := e.startOwn(own)
	for name, c := range map[string]*http.Client{"own keys": own, "invite": invited, "no session": e.visitor()} {
		for _, r := range [][2]string{
			{"GET", "/api/settings"}, {"POST", "/api/settings"},
			{"POST", "/api/jobs/" + id + "/sync"}, {"GET", "/api/jobs/" + id + "/sync"},
			{"GET", "/api/tasks/hello-world-kavach/environment"}, {"POST", "/api/tasks/hello-world-kavach/environment"},
		} {
			if code, _ := e.do(c, r[0], r[1], `{"keys":{}}`); code != 404 {
				t.Errorf("%s: %s %s: %d", name, r[0], r[1], code)
			}
		}
	}
	e.stop(own, id)
}

func TestTrialOwnRetryRequestsCapped(t *testing.T) {
	e := newTrialServer(t, callsScript, func(_ *Config, tc *TrialConfig) { tc.OwnMaxRetryReqs = 2 })
	c := e.visitor()
	e.setKeys(c)
	id := e.startOwn(c)
	e.call(1)
	e.stop(c, id)
	dir := filepath.Join(e.jobs, id)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"job_name":"`+id+`"}`), 0o644)
	n := 0
	retry := func() int {
		// A distinct attempt each time: the moved folder is named by the second.
		n++
		trial := id + "__" + string(rune('A'+n))
		os.MkdirAll(filepath.Join(dir, trial), 0o755)
		os.WriteFile(filepath.Join(dir, trial, "config.json"), []byte("{}"), 0o644)
		code, _ := e.do(c, "POST", "/api/trials/"+id+"/"+trial+"/retry", "")
		return code
	}
	for i := 1; i <= 2; i++ {
		if code := retry(); code != 202 {
			t.Fatalf("retry %d: %d", i, code)
		}
		e.call(1 + i)
		e.stop(c, id)
	}
	if code := retry(); code != 429 {
		t.Errorf("third retry: %d", code)
	}
	if n := readRetryRequests(e.jobs, id); n != 2 {
		t.Errorf("persisted count %d", n)
	}
	// Resume is not a retry request.
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 202 {
		t.Errorf("resume: %d", code)
	}
	e.call(4)
	e.stop(c, id)
}

func TestTrialRelaunchStartsInAFreshHome(t *testing.T) {
	e, c := ownServer(t, callsScript)
	id := e.startOwn(c)
	e.call(1)
	e.stop(c, id)
	home := filepath.Join(e.jobs, ".bench", "home", id)
	os.MkdirAll(home, 0o700)
	stale := filepath.Join(home, "stale")
	os.WriteFile(stale, []byte("x"), 0o600)
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/resume", ""); code != 202 {
		t.Fatal(code)
	}
	e.call(2)
	if _, err := os.Stat(stale); err == nil {
		t.Error("resume reused the old home folder")
	}
	e.stop(c, id)
}
