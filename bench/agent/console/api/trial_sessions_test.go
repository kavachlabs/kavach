package api

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestTrialSessionsSurviveRestart(t *testing.T) {
	e := newTrialServer(t, "", func(_ *Config, tc *TrialConfig) { tc.InviteCodes = []string{"alpha-code"} })
	own, inv := e.visitor(), e.visitor()
	e.setKeys(own)
	run := e.start(own)
	e.invite(inv)

	b, err := os.ReadFile(sessionsFile(e.jobs))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(sessionsFile(e.jobs)); fi.Mode().Perm() != 0o600 {
		t.Errorf("sessions.json mode %v", fi.Mode().Perm())
	}
	u, _ := url.Parse(e.ts.URL)
	secrets := []string{modelKey, daytonaKey, "alpha-code"}
	for _, c := range own.Jar.Cookies(u) {
		secrets = append(secrets, c.Value)
	}
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			t.Errorf("sessions.json holds %q", s)
		}
	}

	// The same cookies resolve to the same sessions on a new server, without
	// their keys (cookies are not kept apart by port).
	s2, ts2 := newServer(t, e.cfg)
	e2 := &trialEnv{t: t, s: s2, ts: ts2, dir: e.dir, jobs: e.jobs, cfg: e.cfg}
	_, v := e2.do(own, "GET", "/api/trial/session", "")
	if runs, _ := v["runs"].([]any); v["has_keys"] != false || len(runs) != 1 || runs[0] != run {
		t.Errorf("own session after a restart: %v", v)
	}
	if _, v := e2.do(inv, "GET", "/api/trial/session", ""); v["invite"] != true {
		t.Errorf("invite session after a restart: %v", v)
	}
	if code, _ := e2.do(e2.visitor(), "POST", "/api/jobs/"+run+"/stop", ""); code == 202 {
		t.Errorf("a stranger stopped the run")
	}
	e2.stop(own, run)
}
