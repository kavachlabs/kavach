package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// trialRoutes registers only what a public server offers. The rest of the
// API is not mounted, so a route added to the console later stays private.
func (s *Server) trialRoutes() {
	h := s.mux.HandleFunc
	h("GET /api/trial/session", s.getTrialSession)
	h("POST /api/trial/keys", s.postTrialKeys)
	h("DELETE /api/trial/keys", s.deleteTrialKeys)
	h("POST /api/trial/invite", s.postTrialInvite)
	h("GET /api/status", s.getTrialStatus)
	h("GET /api/tasks", s.getTrialTasks)
	h("GET /api/models", s.getModels)
	h("GET /api/jobs", s.getTrialJobs)
	h("POST /api/jobs", s.postTrialJob)
	h("GET /api/jobs/{id}", s.getJob)
	h("GET /api/jobs/{id}/compare", s.getCompare)
	h("POST /api/jobs/{id}/stop", s.stopTrialJob)
	h("POST /api/jobs/{id}/pause", s.pauseTrialJob)
	h("POST /api/jobs/{id}/resume", s.resumeTrialJob)
	h("POST /api/trials/{job}/{trial}/retry", s.retryTrialJob)
	h("GET /api/trials/{job}/{trial}", s.getTrialTrial)
	h("GET /api/trials/{job}/{trial}/steps", s.getSteps)
	h("GET /api/trials/{job}/{trial}/patch", s.getPatch)
	h("GET /api/trials/{job}/{trial}/grade", s.getGrade)
	h("GET /api/trials/{job}/{trial}/invariants", s.getInvariants)
	h("GET /api/trials/{job}/{trial}/invariants/source", s.getInvariantSource)
	h("GET /api/trials/{job}/{trial}/invariants/file", s.getInvariantFile)
	h("GET /api/trials/{job}/{trial}/incident", s.getIncident)
	h("GET /api/trials/{job}/{trial}/variants", s.getVariants)
	h("GET /api/trials/{job}/{trial}/variants/{round}/{id}", s.getVariant)
	h("GET /api/events", s.getEvents)
	// Settings, sync and the task environment answer 404 rather than the
	// mux's 405, whoever asks: settings hold the server's keys, sync its
	// backup bucket, and the task environment runs the server's own Docker
	// and names local paths. The routes that act on a run (pause, resume,
	// retry) are above, and 404 for a session that may not use them.
	for _, p := range []string{"/api/settings", "/api/tasks/{task}/environment", "/api/jobs/{id}/sync"} {
		for _, m := range []string{"GET", "POST"} {
			h(m+" "+p, func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not found") })
		}
	}
	for _, p := range []string{"/api/jobs/{id}/pause", "/api/jobs/{id}/resume", "/api/trials/{job}/{trial}/retry"} {
		h("GET "+p, func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not found") })
	}
	if s.cfg.Web != nil {
		s.mux.Handle("GET /", http.FileServerFS(s.cfg.Web))
	}
}

// getTrialStatus is /api/status without the server's paths, keys and Docker:
// visitors run on remote sandboxes, so only Harbor's version is told, and
// not its error, which could name a path.
func (s *Server) getTrialStatus(w http.ResponseWriter, r *http.Request) {
	s.statusMu.Lock()
	if time.Since(s.statusTime) > statusTTL {
		s.status = Status{Harbor: check(r.Context(), s.cfg.Harbor, "--version")}
		s.statusTime = time.Now()
	}
	harborCheck := s.status.Harbor
	s.statusMu.Unlock()
	if !harborCheck.OK {
		harborCheck = Check{Error: "harbor is not available"}
	}
	writeJSON(w, http.StatusOK, Status{
		Docker: Check{OK: true}, Harbor: harborCheck, Tasks: len(s.trial.allowedTasks()),
		Environments: []EnvironmentStatus{}, Sync: SyncStatus{Keys: []SyncKey{}},
	})
}

// getTrialTasks lists the tasks a visitor may run, without local paths.
func (s *Server) getTrialTasks(w http.ResponseWriter, r *http.Request) {
	all, err := harbor.FindTasks(s.cfg.TasksDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	allowed := s.trial.allowedTasks()
	if s.trial.hasKeys(r) {
		allowed = s.trial.allTasks()
	}
	out := []harbor.Task{}
	for _, task := range all {
		arms := []harbor.TaskArm{}
		for _, a := range task.Arms {
			if slices.Contains(allowed, a.Task) {
				a.Path = ""
				arms = append(arms, a)
			}
		}
		if len(arms) > 0 {
			task.Arms = arms
			out = append(out, task)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getTrialJobs lists trial runs and the showcase.
func (s *Server) getTrialJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.reader.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	v := s.trial.viewer(r)
	out := []harbor.Job{}
	for _, j := range jobs {
		if !s.trial.visible(j.ID) {
			continue
		}
		j.Trials = nil
		s.trial.shape(&j, v, s.trial.ownedBy(v, j.ID))
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) stopTrialJob(w http.ResponseWriter, r *http.Request) {
	if !s.trial.owns(r, r.PathValue("id")) {
		writeError(w, http.StatusForbidden, "only the visitor who started a run can stop it")
		return
	}
	s.stopJob(w, r)
}

func (s *Server) getTrialTrial(w http.ResponseWriter, r *http.Request) {
	t, err := s.reader.Trial(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	s.trial.shapeTrial(&t)
	writeJSON(w, http.StatusOK, t)
}
