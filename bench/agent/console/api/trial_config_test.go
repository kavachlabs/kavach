package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTrialFile replaces the file and moves its mtime on, so a reload does
// not depend on the filesystem's clock granularity.
func writeTrialFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Duration(time.Now().UnixNano()%1000+1) * time.Hour)
	os.Chtimes(path, later, later)
}

func TestTrialConfigFileFallsBackToEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trial.json")
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) {
		tc.ConfigPath, tc.InviteCodes, tc.InviteRuns = path, []string{"env-code-1"}, 2
	})
	c := e.visitor()
	if code, v := e.do(c, "POST", "/api/trial/invite", `{"code":"env-code-1"}`); code != 200 || v["invite_runs_left"] != 2.0 {
		t.Fatalf("env code without a file: %d %v", code, v)
	}
	// A file adds a partner, with a budget of their own, and limits the models.
	writeTrialFile(t, path, `{"invites":[{"code":"partner-code-1","label":"P","runs":7,"created":"2026-01-01T00:00:00Z"}],"models":["openrouter/other/model"],"max_attempts":1}`)
	c2 := e.visitor()
	if code, v := e.do(c2, "POST", "/api/trial/invite", `{"code":"partner-code-1"}`); code != 200 || v["invite_runs_left"] != 7.0 {
		t.Fatalf("file code: %d %v", code, v)
	}
	_, v := e.do(c2, "GET", "/api/trial/session", "")
	lim := v["limits"].(map[string]any)
	if lim["max_attempts"] != 1.0 || lim["models"].([]any)[0] != "openrouter/other/model" {
		t.Errorf("limits: %v", lim)
	}
	// The environment's code still works beside the file's.
	if code, _ := e.do(e.visitor(), "POST", "/api/trial/invite", `{"code":"env-code-1"}`); code != 200 {
		t.Errorf("env code beside a file: %d", code)
	}
	// Removing the file returns to the environment alone.
	os.Remove(path)
	if code, _ := e.do(e.visitor(), "POST", "/api/trial/invite", `{"code":"partner-code-1"}`); code != 403 {
		t.Errorf("file code after the file went: %d", code)
	}
}

func TestTrialConfigMalformedKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trial.json")
	writeTrialFile(t, path, `{"invites":[{"code":"partner-code-1","label":"P","runs":3,"created":"2026-01-01T00:00:00Z"}]}`)
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.ConfigPath = path })
	if code, _ := e.do(e.visitor(), "POST", "/api/trial/invite", `{"code":"partner-code-1"}`); code != 200 {
		t.Fatalf("good file: %d", code)
	}
	for _, bad := range []string{
		`{"invites":[`,
		`{"invites":[],"modles":["x/y"]}`,
		`{"invites":[{"code":"","label":"open","runs":9}]}`,
		`{"invites":[{"code":"a b c d e f g h","runs":9}]}`,
	} {
		writeTrialFile(t, path, bad)
		if code, _ := e.do(e.visitor(), "POST", "/api/trial/invite", `{"code":"partner-code-1"}`); code != 200 {
			t.Errorf("after %s the last good config was lost: %d", bad, code)
		}
		if code, _ := e.do(e.visitor(), "POST", "/api/trial/invite", `{"code":""}`); code != 403 {
			t.Errorf("after %s an empty code opened the trial: %d", bad, code)
		}
	}
}

func TestTrialInviteBudgetsPersistAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trial.json")
	writeTrialFile(t, path, `{"invites":[
	  {"code":"alpha-code-1","label":"Alpha","runs":1,"created":"2026-01-01T00:00:00Z"},
	  {"code":"beta-code-01","label":"Beta","runs":2,"created":"2026-01-01T00:00:00Z"}]}`)
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.ConfigPath = path })
	alpha, beta := e.visitor(), e.visitor()
	e.do(alpha, "POST", "/api/trial/invite", `{"code":"alpha-code-1"}`)
	e.do(beta, "POST", "/api/trial/invite", `{"code":"beta-code-01"}`)

	id := e.start(alpha)
	e.do(alpha, "POST", "/api/jobs/"+id+"/stop", "")
	e.s.runner.Wait(id)
	if code, v := e.do(alpha, "POST", "/api/jobs", runBody); code != 403 || !strings.Contains(v["error"].(string), "no runs left") {
		t.Fatalf("alpha's budget is 1: %d %v", code, v)
	}
	// Beta's budget is its own.
	id = e.start(beta)
	e.do(beta, "POST", "/api/jobs/"+id+"/stop", "")
	e.s.runner.Wait(id)

	b, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", "invites-used.json"))
	var use map[string]InviteUse
	if err := json.Unmarshal(b, &use); err != nil || strings.Contains(string(b), "alpha-code-1") {
		t.Fatalf("usage file %q %v", b, err)
	}
	if u := use[InviteDigest("beta-code-01")]; u.Count != 1 || len(u.Jobs) != 1 || u.Last.IsZero() {
		t.Errorf("beta's usage: %+v", u)
	}

	// A restart keeps the counts, and revoking cuts off a session that
	// redeemed the code before.
	cfg := e.cfg
	cfg.Trial = &TrialConfig{Models: e.cfg.Trial.Models, ConfigPath: path, RatePerMin: 1000}
	s2, ts2 := newServer(t, cfg)
	if got := s2.trial.used[InviteDigest("alpha-code-1")].runs(); got != 1 {
		t.Fatalf("alpha's count after a restart: %d", got)
	}
	e2 := &trialEnv{t: t, s: s2, ts: ts2, dir: e.dir, jobs: e.jobs, cfg: cfg}
	b2 := e2.visitor()
	if code, v := e2.do(b2, "POST", "/api/trial/invite", `{"code":"beta-code-01"}`); code != 200 || v["invite_runs_left"] != 1.0 {
		t.Fatalf("beta after a restart: %d %v", code, v)
	}
	writeTrialFile(t, path, `{"invites":[
	  {"code":"alpha-code-1","label":"Alpha","runs":1,"created":"2026-01-01T00:00:00Z"},
	  {"code":"beta-code-01","label":"Beta","runs":2,"created":"2026-01-01T00:00:00Z","revoked":true}]}`)
	code, v := e2.do(b2, "POST", "/api/jobs", runBody)
	if code != 403 || !strings.Contains(v["error"].(string), "revoked") {
		t.Fatalf("a redeemed session after revoking: %d %v", code, v)
	}
	if code, _ := e2.do(e2.visitor(), "POST", "/api/trial/invite", `{"code":"beta-code-01"}`); code != 403 {
		t.Errorf("a revoked code redeemed: %d", code)
	}
	if _, v := e2.do(b2, "GET", "/api/trial/session", ""); v["invite_runs_left"] != 0.0 {
		t.Errorf("a revoked invite shows runs left: %v", v)
	}
}

func TestTrialLegacyInviteCountsCarryOver(t *testing.T) {
	jobs := t.TempDir()
	os.MkdirAll(filepath.Join(jobs, ".bench"), 0o755)
	os.WriteFile(filepath.Join(jobs, ".bench", "trial-invites.json"), []byte(`{"`+InviteDigest("old-code-1")+`":2}`), 0o600)
	if got := ReadInviteUse(jobs)[InviteDigest("old-code-1")].runs(); got != 2 {
		t.Errorf("legacy count: %d", got)
	}
}

func TestUpdateTrialFileKeepsModeAndRefusesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trial.json")
	if err := UpdateTrialFile(path, func(f *TrialFile) error { f.Models = []string{"openrouter/a/b"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Errorf("new file mode %v", st.Mode())
	}
	os.Chmod(path, 0o640)
	if err := UpdateTrialFile(path, func(f *TrialFile) error { f.Tasks = []string{"x"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", st.Mode())
	}
	before, _ := os.ReadFile(path)
	if err := UpdateTrialFile(path, func(f *TrialFile) error { f.Models = []string{"nope"}; return nil }); err == nil {
		t.Error("an invalid model was written")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a refused edit changed the file")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".trial.json.tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
