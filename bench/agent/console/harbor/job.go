// Package harbor reads the job folders Harbor writes, starts and stops Harbor
// runs, and turns an agent's logs into steps a console can show.
//
// A job folder is <jobs>/<job>/ with config.json, lock.json (every trial the
// job will run) and result.json (written when the job starts, updated as
// trials finish, finished_at set at the end). Each trial is
// <job>/<task>__<id>/ with config.json and lock.json from the start, agent/
// and verifier/ mounted into the containers (so they fill while it runs), and
// result.json only once the trial has finished.
package harbor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Trial statuses.
const (
	Queued  = "queued"
	Running = "running"
	Pass    = "pass"
	Fail    = "fail"
	Error   = "error"
)

// Job statuses.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobStopped = "stopped" // Harbor was stopped or exited before finishing
	JobPausing = "pausing" // paused; Harbor is cancelling the attempts in progress
	JobPaused  = "paused"  // stopped to be resumed
)

// ErrNotFound is returned for a job or trial that does not exist.
var ErrNotFound = errors.New("not found")

// Tokens counts an attempt's tokens. Input excludes the cached input.
type Tokens struct {
	Input     int64 `json:"input"`
	Cached    int64 `json:"cached"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
}

// Fresh is the input not served from cache plus the output: what the
// attempt cost in new tokens. The cached input re-reads the conversation
// on every call, so it dwarfs the rest.
func (t Tokens) Fresh() int64 { return t.Input + t.Output }

// MarshalJSON adds the fresh total.
func (t Tokens) MarshalJSON() ([]byte, error) {
	type plain Tokens
	return json.Marshal(struct {
		plain
		Fresh int64 `json:"fresh"`
	}{plain(t), t.Fresh()})
}

func (t *Tokens) add(u Tokens) {
	t.Input += u.Input
	t.Cached += u.Cached
	t.Output += u.Output
	t.Reasoning += u.Reasoning
}

// Job is one Harbor job: every model and arm of one run.
type Job struct {
	ID       string     `json:"id"`
	Started  *time.Time `json:"started"`
	Finished *time.Time `json:"finished"`
	Status   string     `json:"status"`
	Tasks    []string   `json:"tasks"`
	Agent    string     `json:"agent"`
	Models   []string   `json:"models"`
	// Environment is where the trials run (docker, daytona, ...), from the
	// job's config; empty when the config does not say.
	Environment string  `json:"environment,omitempty"`
	Done        int     `json:"trials_done"`
	Total       int     `json:"trials_total"`
	Cost        float64 `json:"cost"`
	Stoppable   bool    `json:"stoppable"` // started by this process and still running
	Resumable   bool    `json:"resumable"` // started by bench, stopped or paused, not finished
	Retryable   bool    `json:"retryable"` // started by bench, not running, with a job folder: its attempts can be run again
	Error       string  `json:"error,omitempty"`
	// Sync is the job's backup to R2; absent when the reader has no Syncer.
	Sync   *SyncState `json:"sync,omitempty"`
	Trials []Trial    `json:"trials,omitempty"`
}

// Trial is one attempt of one model on one task.
type Trial struct {
	ID       string     `json:"id"` // "" while queued
	Job      string     `json:"job"`
	Task     string     `json:"task"`
	Arm      string     `json:"arm"`
	Model    string     `json:"model"`
	Attempt  int        `json:"attempt"`
	Status   string     `json:"status"`
	Reward   *float64   `json:"reward"`
	Started  *time.Time `json:"started"`
	Finished *time.Time `json:"finished"`
	Seconds  *float64   `json:"seconds"` // the agent's run time, once finished
	Steps    int        `json:"steps"`   // model calls
	Tokens   Tokens     `json:"tokens"`
	Cost     *float64   `json:"cost"`
	Error    string     `json:"error,omitempty"`
	Kavach   *Kavach    `json:"kavach"`
	OpenCode *OpenCode  `json:"opencode"`
}

// Kavach is the verifier's kavach.json: Kavach's verdict on the agent's fix.
type Kavach struct {
	Verdict  string `json:"verdict"`
	Features string `json:"features,omitempty"`
	Variants string `json:"variants"`
	Old      string `json:"old"`
}

// OpenCode is the agent's opencode-live.json: where the attempt's OpenCode
// server listens. Export is the path of opencode-session.json, when present.
type OpenCode struct {
	URL     string `json:"url"`
	Session string `json:"session"`
	// Web is the session's page in OpenCode's web UI. The server's own
	// address opens on a project list, which a session in a container's
	// folder is not part of.
	Web      string     `json:"web,omitempty"`
	Started  *time.Time `json:"started"`
	Finished *time.Time `json:"finished"`
	Export   string     `json:"export,omitempty"`
}

// Arms are the benchmark arms in display order.
var Arms = []string{"control", "workflow", "kavach", "kavach-implement"}

// SplitArm splits a task name into its base name and arm: redis-10068-kavach
// is redis-10068 in the kavach arm. A name with no arm suffix has arm "".
func SplitArm(task string) (base, arm string) {
	for _, a := range []string{"kavach-implement", "workflow", "control", "kavach"} {
		if b, ok := strings.CutSuffix(task, "-"+a); ok && b != "" {
			return b, a
		}
	}
	return task, ""
}

// CompareArms reports whether arm a comes before arm b in benchmark order
// (control, workflow, kavach, kavach-implement, then lexical order).
func CompareArms(a, b string) bool {
	ra, rb := armRank(a), armRank(b)
	if ra != rb {
		return ra < rb
	}
	return a < b
}

func armRank(arm string) int {
	for i, a := range Arms {
		if a == arm {
			return i
		}
	}
	return len(Arms)
}

// Reader reads the job folders under Dir.
type Reader struct {
	Dir  string
	Runs *Runner // the runs this process started; may be nil
	// Sync, when set, supplies each job's Sync state.
	Sync *Syncer

	// Docker and CacheDir, when set, let RealSource fetch the real files a
	// trial's invariants live in.
	Docker   Docker
	CacheDir string

	mu    sync.Mutex
	cache map[string]cachedTrial // by trial directory
}

type cachedTrial struct {
	key   string
	trial Trial
}

// Harbor's files, only the fields read here.
type (
	jobResult struct {
		StartedAt  string  `json:"started_at"`
		FinishedAt *string `json:"finished_at"`
		NTotal     int     `json:"n_total_trials"`
		Stats      struct {
			NCancelled int `json:"n_cancelled_trials"`
		} `json:"stats"`
	}
	agentConfig struct {
		Name       string `json:"name"`
		ImportPath string `json:"import_path"`
		ModelName  string `json:"model_name"`
	}
	lockTrial struct {
		Task struct {
			Name string `json:"name"`
		} `json:"task"`
		Agent agentConfig `json:"agent"`
	}
	jobLock struct {
		Trials []lockTrial `json:"trials"`
	}
	timing struct {
		StartedAt  string `json:"started_at"`
		FinishedAt string `json:"finished_at"`
	}
	trialResult struct {
		Config struct {
			Agent agentConfig `json:"agent"`
		} `json:"config"`
		AgentResult *struct {
			Input  *int64   `json:"n_input_tokens"` // includes the cached input
			Cache  *int64   `json:"n_cache_tokens"`
			Output *int64   `json:"n_output_tokens"`
			Cost   *float64 `json:"cost_usd"`
		} `json:"agent_result"`
		VerifierResult *struct {
			Rewards map[string]float64 `json:"rewards"`
		} `json:"verifier_result"`
		ExceptionInfo *struct {
			Type    string `json:"exception_type"`
			Message string `json:"exception_message"`
		} `json:"exception_info"`
		StartedAt      string  `json:"started_at"`
		FinishedAt     string  `json:"finished_at"`
		AgentExecution *timing `json:"agent_execution"`
	}
	liveFile struct {
		URL      string `json:"url"`
		Session  string `json:"session"`
		Started  string `json:"started"`
		Finished string `json:"finished"`
		Dir      string `json:"dir"`
	}
)

// Jobs returns every job, newest first.
func (r *Reader) Jobs() ([]Job, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil && !(errors.Is(err, os.ErrNotExist) && r.Runs != nil) {
		return nil, err
	}
	seen := map[string]bool{}
	var jobs []Job
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !isJobDir(filepath.Join(r.Dir, e.Name())) {
			continue
		}
		seen[e.Name()] = true
		jobs = append(jobs, r.readJob(e.Name()))
	}
	// A run whose Harbor has not written its folder yet, or never will.
	for _, id := range r.Runs.ids() {
		if !seen[id] {
			jobs = append(jobs, r.readJob(id))
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool { return after(jobs[i].Started, jobs[j].Started) })
	return jobs, nil
}

// Job returns one job with its trials.
func (r *Reader) Job(id string) (Job, error) {
	if !validName(id) {
		return Job{}, ErrNotFound
	}
	if !isJobDir(filepath.Join(r.Dir, id)) && !r.Runs.launched(id) {
		return Job{}, ErrNotFound
	}
	return r.readJob(id), nil
}

// Trial returns one trial of a job.
func (r *Reader) Trial(job, id string) (Trial, error) {
	j, err := r.Job(job)
	if err != nil {
		return Trial{}, err
	}
	for _, t := range j.Trials {
		if t.ID == id && id != "" {
			return t, nil
		}
	}
	return Trial{}, ErrNotFound
}

// Steps returns a trial's steps so far.
func (r *Reader) Steps(job, id string) ([]Step, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return nil, err
	}
	return readSteps(dir)
}

func isJobDir(dir string) bool {
	for _, f := range []string{"config.json", "lock.json", "result.json"} {
		if exists(filepath.Join(dir, f)) {
			return true
		}
	}
	return false
}

func isTrialDir(dir string) bool { return exists(filepath.Join(dir, "config.json")) }

// validName reports whether s is a single path element that is safe to join.
func validName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`) && !strings.HasPrefix(s, ".")
}

func (r *Reader) readJob(id string) Job {
	dir := filepath.Join(r.Dir, id)
	j := Job{ID: id, Status: JobRunning, Tasks: []string{}, Models: []string{}}
	var res jobResult
	hasResult := readJSON(filepath.Join(dir, "result.json"), &res) == nil
	var lock jobLock
	_ = readJSON(filepath.Join(dir, "lock.json"), &lock)

	if hasResult {
		j.Started = parseTime(res.StartedAt)
		if res.FinishedAt != nil {
			j.Finished = parseTime(*res.FinishedAt)
		}
	} else if st, err := os.Stat(dir); err == nil {
		t := st.ModTime()
		j.Started = &t
	}

	var cfg struct {
		Environment struct {
			Type string `json:"type"`
		} `json:"environment"`
	}
	if readJSON(filepath.Join(dir, "config.json"), &cfg) != nil || cfg.Environment.Type == "" {
		if r.Runs.launched(id) {
			readJSON(r.Runs.file(id, ".json"), &cfg)
		}
	}
	j.Environment = cfg.Environment.Type

	launched, alive, out := r.Runs.state(id)
	j.Retryable = launched && !alive && exists(filepath.Join(dir, "config.json"))
	switch {
	case launched && alive:
		// Before a retry's Harbor rewrites result.json, it still says
		// when the run first finished.
		j.Finished = nil
		j.Stoppable = true
		if r.Runs.paused(id) {
			j.Status = JobPausing
		}
	case j.Finished != nil:
		j.Status = JobDone
	case launched:
		j.Resumable = true
		j.Status = JobStopped
		if r.Runs.paused(id) {
			j.Status = JobPaused
		} else if !hasResult {
			j.Error = out
		}
	case res.Stats.NCancelled > 0:
		j.Status = JobStopped
	case time.Since(lastWrite(dir)) > staleAfter:
		j.Status = JobStopped
		j.Error = "Harbor stopped writing to this job " + staleAfter.String() + " ago without finishing it"
	}
	if launched && j.Started == nil {
		j.Started = r.Runs.started(id)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && isTrialDir(filepath.Join(dir, e.Name())) {
			t := r.readTrial(id, filepath.Join(dir, e.Name()))
			// Resuming runs the cancelled attempts again, from the start.
			if j.Status == JobPaused && (t.Status == Running || strings.HasPrefix(t.Error, "CancelledError")) {
				continue
			}
			if t.Status == Running && j.Status != JobRunning && j.Status != JobPausing {
				t.Status, t.Error = Error, "interrupted"
			}
			j.Trials = append(j.Trials, t)
		}
	}
	j.Trials = number(j.Trials, lock.Trials, id, j.Status == JobRunning || j.Status == JobPausing || j.Status == JobPaused)

	planned := 0
	if len(lock.Trials) == 0 && launched {
		var tasks, models []string
		tasks, j.Agent, models, planned = r.Runs.planned(id)
		for _, t := range tasks {
			j.Tasks = appendNew(j.Tasks, t)
		}
		for _, m := range models {
			j.Models = appendNew(j.Models, m)
		}
	}
	for _, t := range lock.Trials {
		if j.Agent == "" {
			j.Agent = agentName(t.Agent)
		}
		j.Tasks = appendNew(j.Tasks, t.Task.Name)
		j.Models = appendNew(j.Models, t.Agent.ModelName)
	}
	for _, t := range j.Trials {
		j.Tasks = appendNew(j.Tasks, t.Task)
		j.Models = appendNew(j.Models, t.Model)
		switch t.Status {
		case Pass, Fail, Error:
			j.Done++
		}
		if t.Cost != nil {
			j.Cost += *t.Cost
		}
	}
	j.Total = max(len(lock.Trials), res.NTotal, len(j.Trials), planned)
	// Harbor writes finished_at within seconds of its last trial; a job
	// with every trial done and no finished_at for longer lost its Harbor.
	if j.Status == JobRunning && !launched && j.Total > 0 && j.Done == j.Total && time.Since(lastWrite(dir)) > finishGrace {
		j.Status = JobStopped
		j.Error = "Harbor exited after the last trial without finishing the job"
	}
	j.Sync = r.Sync.State(id)
	return j
}

// staleAfter is how long a job that bench did not start (so has no pid to
// check) may go without a write to its folder before it counts as stopped.
// Harbor writes as trials start and finish and agents log every event; the
// longest quiet stretch is building a task's image (redis-10068 allows 30
// minutes).
const staleAfter = 45 * time.Minute

// finishGrace is how long a job whose trials have all finished may wait for
// Harbor to mark it finished.
const finishGrace = 2 * time.Minute

// lastWrite is the newest modification time in a job's folder. OpenCode's
// own database (agent/opencode) is skipped: it is large and written only
// along with the event log.
func lastWrite(dir string) time.Time {
	var last time.Time
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "opencode" {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(last) {
			last = info.ModTime()
		}
		return nil
	})
	return last
}

func startOf(t Trial) time.Time {
	if t.Started == nil {
		return time.Time{}
	}
	return *t.Started
}

func agentName(a agentConfig) string {
	name := a.Name
	if name == "" {
		name = a.ImportPath
	}
	if name == LiveAgent {
		// harbor run --agent <import path> records it as the name.
		return "opencode"
	}
	return name
}

func appendNew(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

func compareTask(a, b string) bool {
	ba, aa := SplitArm(a)
	bb, ab := SplitArm(b)
	if ba != bb {
		return ba < bb
	}
	if aa != ab {
		return CompareArms(aa, ab)
	}
	return a < b
}

// number gives each trial its attempt number within its task and model, in
// the order they started, and adds the trials the job has yet to start.
func number(trials []Trial, planned []lockTrial, job string, running bool) []Trial {
	sort.SliceStable(trials, func(i, k int) bool {
		a, b := trials[i], trials[k]
		if a.Task != b.Task {
			return compareTask(a.Task, b.Task)
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if sa, sb := startOf(a), startOf(b); !sa.Equal(sb) {
			return sa.Before(sb)
		}
		return a.ID < b.ID
	})
	type key struct{ task, model string }
	count := map[key]int{}
	for i := range trials {
		k := key{trials[i].Task, trials[i].Model}
		count[k]++
		trials[i].Attempt = count[k]
	}
	if !running {
		return trials
	}
	plan := map[key]int{}
	var order []key
	for _, p := range planned {
		k := key{p.Task.Name, p.Agent.ModelName}
		if plan[k] == 0 {
			order = append(order, k)
		}
		plan[k]++
	}
	for _, k := range order {
		for n := count[k] + 1; n <= plan[k]; n++ {
			_, arm := SplitArm(k.task)
			trials = append(trials, Trial{Job: job, Task: k.task, Arm: arm, Model: k.model, Attempt: n, Status: Queued})
		}
	}
	sort.SliceStable(trials, func(i, k int) bool {
		a, b := trials[i], trials[k]
		if a.Task != b.Task {
			return compareTask(a.Task, b.Task)
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Attempt < b.Attempt
	})
	return trials
}

// trialFiles are the files a trial is read from; a change to any of them
// invalidates the cached trial.
var trialFiles = []string{
	"lock.json", "result.json",
	"agent/trajectory.json", "agent/opencode.txt", "agent/opencode-live.txt", "agent/opencode-live.json", "agent/opencode-session.json",
	"verifier/kavach.json",
}

func (r *Reader) readTrial(job, dir string) Trial {
	var key strings.Builder
	for _, f := range trialFiles {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil {
			fmt.Fprintf(&key, "%s:%d:%d;", f, st.Size(), st.ModTime().UnixNano())
		}
	}
	r.mu.Lock()
	c, ok := r.cache[dir]
	r.mu.Unlock()
	if ok && c.key == key.String() {
		return c.trial
	}
	t := loadTrial(job, dir)
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedTrial{}
	}
	r.cache[dir] = cachedTrial{key.String(), t}
	r.mu.Unlock()
	return t
}

func loadTrial(job, dir string) Trial { return trialFrom(job, dir, os.DirFS(dir)) }

// trialFrom reads the trial in dir from fsys, which holds dir's files.
func trialFrom(job, dir string, fsys fs.FS) Trial {
	name := filepath.Base(dir)
	t := Trial{ID: name, Job: job, Status: Running}
	var lock lockTrial
	if readJSONFS(fsys, "lock.json", &lock) == nil {
		t.Task, t.Model = lock.Task.Name, lock.Agent.ModelName
	}
	if t.Task == "" {
		if i := strings.LastIndex(name, "__"); i > 0 {
			t.Task = name[:i]
		}
	}
	_, t.Arm = SplitArm(t.Task)
	// Until result.json exists, the trial started when Harbor wrote its config.
	if st, err := fs.Stat(fsys, "config.json"); err == nil {
		s := st.ModTime()
		t.Started = &s
	}

	if steps, err := stepsFrom(fsys); err == nil {
		var cost float64
		var costed bool
		for _, s := range steps {
			if s.Kind != KindUsage {
				continue
			}
			t.Steps++
			if s.Tokens != nil {
				t.Tokens.add(*s.Tokens)
			}
			if s.Cost != nil {
				cost += *s.Cost
				costed = true
			}
		}
		if costed {
			t.Cost = &cost
		}
	}

	var res trialResult
	if readJSONFS(fsys, "result.json", &res) == nil {
		if t.Model == "" {
			t.Model = res.Config.Agent.ModelName
		}
		if s := parseTime(res.StartedAt); s != nil {
			t.Started = s
		}
		t.Finished = parseTime(res.FinishedAt)
		if e := res.AgentExecution; e != nil {
			s, f := parseTime(e.StartedAt), parseTime(e.FinishedAt)
			if s != nil && f != nil {
				sec := f.Sub(*s).Seconds()
				t.Seconds = &sec
			}
		}
		// Harbor's totals win over the log's: they cover the whole run.
		if a := res.AgentResult; a != nil && a.Input != nil {
			t.Tokens.Cached = deref(a.Cache)
			t.Tokens.Input = *a.Input - t.Tokens.Cached
			t.Tokens.Output = deref(a.Output)
		}
		if a := res.AgentResult; a != nil && a.Cost != nil {
			t.Cost = a.Cost
		}
		if e := res.ExceptionInfo; e != nil {
			t.Error = e.Type
			if e.Message != "" {
				t.Error += ": " + e.Message
			}
		}
		t.Reward = reward(res)
		var perr string
		if res.ExceptionInfo != nil {
			perr = providerError(fsys)
		} else {
			perr = outputLimit(fsys)
		}
		switch {
		case t.Reward != nil && *t.Reward >= 1:
			t.Status = Pass
		case t.Reward != nil && perr != "":
			// The model's provider, or its output limit, ended the run, not
			// the agent: the reward grades whatever it had done by then.
			t.Status = Error
			t.Error = perr
		case t.Reward != nil:
			t.Status = Fail
		default:
			t.Status = Error
			if t.Error == "" {
				t.Error = "no reward"
			}
		}
	}

	var k Kavach
	if readJSONFS(fsys, "verifier/kavach.json", &k) == nil {
		t.Kavach = &k
	}
	var live liveFile
	if readJSONFS(fsys, "agent/opencode-live.json", &live) == nil {
		t.OpenCode = &OpenCode{URL: live.URL, Session: live.Session, Started: parseTime(live.Started), Finished: parseTime(live.Finished)}
		if live.URL != "" && live.Session != "" && live.Dir != "" {
			// The web UI names a project by its folder, base64url-encoded.
			t.OpenCode.Web = strings.TrimSuffix(live.URL, "/") + "/" + base64.RawURLEncoding.EncodeToString([]byte(live.Dir)) + "/session/" + live.Session
		}
	}
	if _, err := fs.Stat(fsys, "agent/opencode-session.json"); err == nil {
		if t.OpenCode == nil {
			t.OpenCode = &OpenCode{}
		}
		t.OpenCode.Export = filepath.Join(dir, "agent", "opencode-session.json")
	}
	return t
}

// lastLogLine is the last line of OpenCode's JSON log of the run.
func lastLogLine(fsys fs.FS) string {
	f, err := fsys.Open("agent/opencode.txt")
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 64 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		if s, ok := f.(io.Seeker); ok {
			s.Seek(st.Size()-tail, io.SeekStart)
		}
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// outputLimit says how the run ended when the model's last turn was cut off
// at its output limit (finish reason "length"): a turn that ends without a
// tool call ends OpenCode's run, so the agent stopped mid-thought, not
// because it was done.
func outputLimit(fsys fs.FS) string {
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Reason string `json:"reason"`
			Tokens struct {
				Output int `json:"output"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if json.Unmarshal([]byte(lastLogLine(fsys)), &ev) != nil || ev.Type != "step_finish" || ev.Part.Reason != "length" {
		return ""
	}
	return fmt.Sprintf("OutputLimit: the model's last turn stopped at its output limit (%d tokens) without a tool call, which ended the run", ev.Part.Tokens.Output)
}

// providerError is the error that ended OpenCode's run, when its log ends with
// one: the model's provider failing (a timeout, a 5xx) rather than the agent.
func providerError(fsys fs.FS) string {
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(lastLogLine(fsys)), &ev) != nil || ev.Type != "error" {
		return ""
	}
	msg := ev.Error.Data.Message
	// OpenRouter passes the provider's error on as JSON.
	var p struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Metadata struct {
			ErrorType string `json:"error_type"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(msg), &p) == nil && p.Message != "" {
		msg = p.Message
		if p.Code != 0 {
			msg = fmt.Sprintf("%d %s", p.Code, msg)
		}
		if p.Metadata.ErrorType != "" {
			msg += " (" + p.Metadata.ErrorType + ")"
		}
	}
	if msg == "" {
		msg = ev.Error.Name
	}
	return "ModelProviderError: " + msg
}

// reward is the trial's reward: the one named "reward", or the only one.
func reward(res trialResult) *float64 {
	v := res.VerifierResult
	if v == nil {
		return nil
	}
	if r, ok := v.Rewards["reward"]; ok {
		return &r
	}
	if len(v.Rewards) == 1 {
		for _, r := range v.Rewards {
			return &r
		}
	}
	return nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readJSONFS(fsys fs.FS, name string, v any) error {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseTime parses Harbor's timestamps. It writes a job's started_at in local
// time without a zone, and everything else in UTC.
func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return &t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.Local); err == nil {
		return &t
	}
	return nil
}

// after orders by time, newest first, with unknown times last.
func after(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a != nil
	}
	return a.After(*b)
}
