package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// A session with its own keys gets the console's run form and run controls
// for its own runs. Its runs bill its own provider accounts but still run as
// Harbor processes on this machine, so what it may ask for is capped here,
// and everything is checked on the server: the form is not a guard.

// ownLimits is what a session with its own keys may ask for.
type ownLimits struct {
	MaxAttempts   int `json:"max_attempts"`       // per task and model
	MaxConcurrent int `json:"max_concurrent"`     // attempts at once in one run
	MaxTrials     int `json:"max_trials"`         // (retries + 1) x tasks x models x attempts
	MaxRetries    int `json:"max_retries"`        // Harbor's retries of an attempt that errors
	MaxRetryReqs  int `json:"max_retry_requests"` // retry requests per run
	MaxTimeoutMin int `json:"max_timeout_min"`
}

// ownMaxTimeoutMin is the longest agent timeout an own-key run may set: the
// form's longest choice. A run whose tasks have a longer timeout of their own
// keeps it by leaving this unset.
const ownMaxTimeoutMin = 120

// openRouterModel is an OpenRouter model id as the run form sends it. The
// name ends up in the agent's command line and config, so it is not free text.
var openRouterModel = regexp.MustCompile(`^openrouter/[A-Za-z0-9~][A-Za-z0-9._~+-]*/[A-Za-z0-9][A-Za-z0-9._+-]*(?::[A-Za-z0-9._-]+)?$`)

// allTasks is every task arm in the tasks directory.
func (t *trial) allTasks() []string {
	all, _ := harbor.FindTasks(t.tasks)
	var out []string
	for _, task := range all {
		for _, a := range task.Arms {
			out = append(out, a.Task)
		}
	}
	return out
}

// hasKeys reports whether the request's session holds its own keys.
func (t *trial) hasKeys(r *http.Request) bool {
	_, sess := t.lookup(r)
	if sess == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return sess.keys != nil
}

// postOwnJob starts a run for a session with its own keys.
func (s *Server) postOwnJob(w http.ResponseWriter, r *http.Request, sess *session, id, invite string, rr harbor.RunRequest, token string) {
	t := s.trial
	t.mu.Lock()
	keys := sess.keys
	t.mu.Unlock()
	if re := s.checkOwnRun(r.Context(), &rr, keys); re != nil {
		writeError(w, re.code, "%s", re.msg)
		return
	}
	if t.cfg.TurnstileSecret != "" {
		if re := t.verifyTurnstile(r.Context(), token, t.clientIP(r)); re != nil {
			writeError(w, re.code, "%s", re.msg)
			return
		}
	}
	name, re := t.startRun(sess, id, t.clientIP(r), invite, rr)
	if re != nil {
		writeError(w, re.code, "%s", re.msg)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, name)
}

// checkOwnRun validates rr against the own-key limits and fills in what the
// server decides: the name, the sandbox, the environment Harbor gets.
func (s *Server) checkOwnRun(ctx context.Context, rr *harbor.RunRequest, k *visitorKeys) *trialRunError {
	t := s.trial
	lim := t.settings().own
	bad := func(format string, args ...any) *trialRunError {
		return &trialRunError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
	}
	switch rr.Agent {
	case "", "opencode":
		rr.Agent = harbor.LiveAgent
	case "oracle", "nop":
	default:
		return bad("agent must be opencode, oracle or nop")
	}
	withModel := rr.Agent == harbor.LiveAgent
	if len(rr.Tasks) == 0 {
		return bad("no tasks")
	}
	all := t.allTasks()
	for _, task := range rr.Tasks {
		if !slices.Contains(all, task) {
			return bad("task %q is not available in the trial", task)
		}
	}
	if withModel && len(rr.Models) == 0 {
		return bad("add a model")
	}
	if !withModel && len(rr.Models) > 0 {
		return bad("the %s agent takes no model", rr.Agent)
	}
	if len(rr.Models) > 0 {
		list, err := s.models.get(ctx)
		if err != nil {
			return &trialRunError{http.StatusBadGateway, "OpenRouter's model list could not be fetched to check the model: try again"}
		}
		for _, m := range rr.Models {
			if !openRouterModel.MatchString(m) || !slices.ContainsFunc(list, func(x Model) bool { return "openrouter/"+x.ID == m }) {
				return bad("model %q is not an OpenRouter model", m)
			}
		}
	}
	if rr.Attempts == 0 {
		rr.Attempts = 1
	}
	if rr.Attempts < 1 || rr.Attempts > lim.MaxAttempts {
		return bad("attempts must be 1-%d in the trial", lim.MaxAttempts)
	}
	if rr.Concurrent == 0 {
		rr.Concurrent = 1
	}
	if rr.Concurrent < 1 || rr.Concurrent > lim.MaxConcurrent {
		return bad("attempts at once must be 1-%d in the trial", lim.MaxConcurrent)
	}
	if rr.TimeoutSec < 0 || rr.TimeoutSec > float64(lim.MaxTimeoutMin*60) {
		return bad("the agent timeout must be at most %d minutes in the trial", lim.MaxTimeoutMin)
	}
	if rr.Retries < 0 || rr.Retries > lim.MaxRetries {
		return bad("retries must be 0-%d in the trial", lim.MaxRetries)
	}
	// Each retry of an erroring attempt is another attempt on the visitor's accounts.
	if n := (rr.Retries + 1) * len(rr.Tasks) * max(len(rr.Models), 1) * rr.Attempts; n > lim.MaxTrials {
		return bad("a run may have at most %d attempts, retries included, in the trial; this one has %d", lim.MaxTrials, n)
	}
	if rr.Environment != "" && rr.Environment != k.provider {
		return bad("attempts run on %s, the sandbox your keys are for; docker would run on this server and is not available", k.provider)
	}
	rr.Environment, rr.Name = k.provider, ""
	rr.Env = ownEnv(k, withModel)
	return nil
}

// controlOf returns the session that asked, if it may pause, resume and retry
// job id: it owns the job and is not an invite's. Otherwise it writes the 404
// the route had before sessions could do this.
func (s *Server) controlOf(w http.ResponseWriter, r *http.Request, id string) (*session, bool) {
	t := s.trial
	sid, sess := t.lookup(r)
	if sess != nil {
		t.mu.Lock()
		invite := sess.invite
		t.mu.Unlock()
		if invite == "" && t.ownedBy(viewer{owner: digest(sid)}, id) {
			return sess, true
		}
	}
	writeError(w, http.StatusNotFound, "not found")
	return nil, false
}

func (s *Server) pauseTrialJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.controlOf(w, r, r.PathValue("id")); ok {
		s.pauseJob(w, r)
	}
}

func (s *Server) resumeTrialJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.controlOf(w, r, id)
	if !ok {
		return
	}
	s.relaunch(w, r, sess, id, false, func(env map[string]string) error { return s.runner.ResumeWithEnv(id, env) })
}

func (s *Server) retryTrialJob(w http.ResponseWriter, r *http.Request) {
	id, trial := r.PathValue("job"), r.PathValue("trial")
	// A retry asked for while the run goes on waits for it to end.
	retry := func(env map[string]string) error {
		_, err := s.runner.QueueRetry(id, trial, env)
		return err
	}
	if sess, ok := s.inviteRetrier(r, id); ok {
		s.relaunchInvite(w, r, sess, id, retry)
		return
	}
	sess, ok := s.controlOf(w, r, id)
	if !ok {
		return
	}
	s.relaunch(w, r, sess, id, true, retry)
}

// inviteRetrier returns the session that asked, if it is an invite's and
// owns job id, which was started on an invite: such a session may retry the
// run's attempts on the operator's keys, though not pause or resume it.
func (s *Server) inviteRetrier(r *http.Request, id string) (*session, bool) {
	t := s.trial
	sid, sess := t.lookup(r)
	if sess == nil {
		return nil, false
	}
	t.mu.Lock()
	invite := sess.invite
	t.mu.Unlock()
	return sess, invite != "" && t.ownedBy(viewer{owner: digest(sid)}, id) && t.inviteRun(id)
}

// inviteRun reports whether job id was started on an invite code.
func (t *trial) inviteRun(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range t.used {
		if slices.Contains(u.Jobs, id) {
			return true
		}
	}
	return false
}

// relaunchInvite retries an attempt of an invited run on the operator's keys,
// as starting it did. A retry uses none of the invite's runs; the per-run cap
// on retry requests and the limits on live runs apply as for a visitor's keys.
func (s *Server) relaunchInvite(w http.ResponseWriter, r *http.Request, sess *session, id string, do func(env map[string]string) error) {
	t := s.trial
	t.mu.Lock()
	invite := sess.invite
	t.mu.Unlock()
	if inv, ok := t.settings().invite(invite); !ok || inv.Revoked {
		writeError(w, http.StatusForbidden, "this invite code has been revoked")
		return
	}
	env, err := t.inviteRunEnv(id)
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.relaunchWith(w, r, sess, id, env, do)
}

// inviteRunEnv is the environment for running invited run id again: the
// server's keys for its model, as starting it had, on the same provider.
func (t *trial) inviteRunEnv(id string) (map[string]string, error) {
	var cfg struct {
		Environment struct {
			Type string `json:"type"`
		} `json:"environment"`
		Agents []struct {
			Model string `json:"model_name"`
		} `json:"agents"`
	}
	b, err := os.ReadFile(filepath.Join(t.jobs, ".bench", id+".json"))
	if err == nil {
		err = json.Unmarshal(b, &cfg)
	}
	if err != nil || len(cfg.Agents) == 0 {
		return nil, errors.New("the run's record could not be read")
	}
	env, provider, err := t.runEnv(cfg.Agents[0].Model)
	if err == nil && provider != cfg.Environment.Type {
		err = errors.New("this run used " + cfg.Environment.Type + ", and invite runs now use " + provider)
	}
	return env, err
}

// relaunch starts Harbor again for run id with the session's own keys, which
// is what starting did: if the session has none any more it refuses, and
// never falls back to the server's env file or keys.
func (s *Server) relaunch(w http.ResponseWriter, r *http.Request, sess *session, id string, isRetry bool, do func(env map[string]string) error) {
	t := s.trial
	t.mu.Lock()
	keys := sess.keys
	t.mu.Unlock()
	env, re := t.relaunchEnv(keys, id)
	if re != nil {
		writeError(w, re.code, "%s", re.msg)
		return
	}
	if isRetry {
		s.relaunchWith(w, r, sess, id, env, do)
		return
	}
	s.relaunchResult(w, r, id, t.relaunchRun(sess, t.clientIP(r), id, false, env, func() error { return do(env) }))
}

// relaunchWith retries an attempt of run id with env, under relaunchRun's limits.
func (s *Server) relaunchWith(w http.ResponseWriter, r *http.Request, sess *session, id string, env map[string]string, do func(env map[string]string) error) {
	t := s.trial
	s.relaunchResult(w, r, id, t.relaunchRun(sess, t.clientIP(r), id, true, env, func() error { return do(env) }))
}

func (s *Server) relaunchResult(w http.ResponseWriter, r *http.Request, id string, err error) {
	var lim *trialRunError
	switch {
	case errors.As(err, &lim):
		writeError(w, lim.code, "%s", lim.msg)
	case errors.Is(err, harbor.ErrNotFound):
		writeError(w, http.StatusNotFound, "%s: no such attempt in a run bench started", id)
	case errors.Is(err, harbor.ErrNotStopped):
		writeError(w, http.StatusConflict, "%s is running, finished or not a run bench started", id)
	case err != nil:
		writeError(w, http.StatusInternalServerError, "restarting harbor: %v", err)
	default:
		s.writeJob(w, r, http.StatusAccepted, id)
	}
}

func retryRequestsFile(jobs, id string) string { return filepath.Join(jobs, ".bench", id+".retries") }

// readRetryRequests is how many retries of run id were requested, kept with
// the run's other bookkeeping so a restart does not reset it.
func readRetryRequests(jobs, id string) int {
	b, _ := os.ReadFile(retryRequestsFile(jobs, id))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// relaunchEnv is the environment for running job id again on keys. The job's
// own record says where it ran and with which models, and the keys must be
// ones that can do the same.
func (t *trial) relaunchEnv(keys *visitorKeys, id string) (map[string]string, *trialRunError) {
	if keys == nil {
		return nil, &trialRunError{http.StatusConflict, "this server no longer holds your keys: enter your keys again to resume"}
	}
	var cfg struct {
		Environment struct {
			Type string `json:"type"`
		} `json:"environment"`
		Agents []struct {
			Model string `json:"model_name"`
		} `json:"agents"`
	}
	b, err := os.ReadFile(filepath.Join(t.jobs, ".bench", id+".json"))
	if err == nil {
		err = json.Unmarshal(b, &cfg)
	}
	if err != nil {
		return nil, &trialRunError{http.StatusConflict, "the run's record could not be read"}
	}
	if cfg.Environment.Type != keys.provider {
		return nil, &trialRunError{http.StatusConflict, "this run used " + cfg.Environment.Type + ", and your keys are for " + keys.provider}
	}
	withModel := false
	for _, a := range cfg.Agents {
		if a.Model == "" {
			continue
		}
		withModel = true
		if !openRouterModel.MatchString(a.Model) {
			return nil, &trialRunError{http.StatusConflict, "this run used " + a.Model + ", which your OpenRouter key cannot call"}
		}
	}
	return ownEnv(keys, withModel), nil
}

// relaunchRun runs start, which starts Harbor again for job id, under the
// limits starting a run has. A retry of a run that is still going is queued
// (QueueRetry) and starts nothing now, so the limits on live runs, which
// the run met when it started, are not asked again.
func (t *trial) relaunchRun(sess *session, ip, id string, isRetry bool, env map[string]string, start func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, live := t.runner.QueuedRetries(id); !isRetry || !live {
		if re := t.admit(sess, ip); re != nil {
			return re
		}
	}
	var asked int
	if isRetry {
		asked = readRetryRequests(t.jobs, id)
		if asked >= t.settings().own.MaxRetryReqs {
			return &trialRunError{http.StatusTooManyRequests, "this run has used its retries; start a new run"}
		}
	}
	if err := start(); err != nil {
		return err
	}
	if isRetry {
		// Best effort: a count that cannot be written only lets one more through.
		os.WriteFile(retryRequestsFile(t.jobs, id), []byte(strconv.Itoa(asked+1)), 0o644)
	}
	t.runIP[id] = ip
	for _, v := range env {
		t.jobSecrets[id] = append(t.jobSecrets[id], v)
	}
	return nil
}
