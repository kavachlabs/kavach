package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// AdminHandler serves the operator's routes on a trial server. It has no
// authentication of its own: serve it only where the operator alone can
// reach it, a unix socket only the server's user can open (bench api
// -admin-socket). Behind a tunnel every request comes from loopback, so it
// must never be mounted on the public listener.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	if s.trial != nil {
		mux.HandleFunc("POST /retry", s.adminRetry)
	}
	return mux
}

// AdminRetry is a request to retry attempts of one run.
type AdminRetry struct {
	Job    string   `json:"job"`
	Trials []string `json:"trials"`
}

// AdminRetried is what happened to one attempt: started now, queued until
// the run ends, or why not.
type AdminRetried struct {
	Trial  string `json:"trial"`
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
}

// adminRetry retries attempts of an invited run on the server's keys, as a
// visitor's Retry does, without the visitor's limits or its cap on retries.
// A run on a visitor's own keys is refused: the server holds no keys for it.
func (s *Server) adminRetry(w http.ResponseWriter, r *http.Request) {
	t := s.trial
	var req AdminRetry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Job == "" || len(req.Trials) == 0 {
		writeError(w, http.StatusBadRequest, "want {\"job\": RUN, \"trials\": [ATTEMPT, ...]}")
		return
	}
	if !plainName(req.Job) {
		writeError(w, http.StatusBadRequest, "%q is not a run", req.Job)
		return
	}
	if !t.inviteRun(req.Job) {
		writeError(w, http.StatusConflict, "%s was not started on an invite code: it ran on a visitor's own keys, which this server does not keep", req.Job)
		return
	}
	env, err := t.inviteRunEnv(req.Job)
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	var out []AdminRetried
	for _, name := range req.Trials {
		res := AdminRetried{Trial: name}
		trial, err := t.resolveTrial(req.Job, name)
		if err == nil {
			res.Trial = trial
			res.Queued, err = s.runner.QueueRetry(req.Job, trial, env)
		}
		switch {
		case errors.Is(err, harbor.ErrNotFound):
			res.Error = "no such attempt in this run"
		case errors.Is(err, harbor.ErrNotStopped):
			res.Error = "the run is in another bench process"
		case err != nil:
			res.Error = err.Error()
		default:
			t.mu.Lock()
			for _, v := range env {
				t.jobSecrets[req.Job] = append(t.jobSecrets[req.Job], v)
			}
			t.mu.Unlock()
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveTrial is the attempt of run job that name names: its full folder
// name, or the part after "__" when only one attempt ends so.
func (t *trial) resolveTrial(job, name string) (string, error) {
	if !plainName(name) {
		return "", harbor.ErrNotFound
	}
	dir := filepath.Join(t.jobs, job)
	if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
		return name, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", harbor.ErrNotFound
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "__"+name) {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return "", harbor.ErrNotFound
	case 1:
		return found[0], nil
	}
	return "", errors.New("more than one attempt ends in " + name + ": " + strings.Join(found, ", "))
}

// plainName is a single path element that is not hidden.
func plainName(s string) bool {
	return s != "" && !strings.ContainsAny(s, `/\`) && !strings.HasPrefix(s, ".")
}
