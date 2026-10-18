package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// TrialConfig turns the server into a public trial: visitors run Kavach's
// benchmark tasks with their own keys, or with the server's under an invite
// code. See "Trial mode" in API.md.
type TrialConfig struct {
	Origin  string // the public origin (https://try.kavachlabs.dev) writes may come from
	TrustCF bool   // the client address is the CF-Connecting-IP header
	// InviteCodes are the codes that run on the server's keys; each may start
	// InviteRuns runs.
	InviteCodes []string
	InviteRuns  int
	Models      []string // allowed models
	Tasks       []string // allowed task folders or base names; empty: every task
	MaxAttempts int
	// ConfigPath is the trial config file (see TrialFile). The server reloads
	// it when it changes; the fields above apply where it says nothing, or
	// is absent.
	ConfigPath string
	MaxRunning int      // live runs, all visitors together
	Showcase   []string // job ids anyone may list and read besides trial runs
	// RatePerMin is how many writes one address may make a minute.
	RatePerMin int

	TurnstileSecret  string // when set, starting a run needs a verified turnstile_token
	TurnstileSiteKey string
	TurnstileURL     string       // default Cloudflare's siteverify
	Client           *http.Client // for the Turnstile call
}

const turnstileVerify = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// TrialFromEnv reads the TRIAL_*, TURNSTILE_* settings from getenv.
func TrialFromEnv(getenv func(string) string) *TrialConfig {
	list := func(k string) []string {
		var out []string
		for _, s := range strings.Split(getenv(k), ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(strings.TrimSpace(getenv(k))); err == nil && n > 0 {
			return n
		}
		return def
	}
	c := &TrialConfig{
		InviteCodes:      list("TRIAL_INVITE_CODES"),
		InviteRuns:       num("TRIAL_INVITE_RUNS", 5),
		Models:           list("TRIAL_MODELS"),
		Tasks:            list("TRIAL_TASKS"),
		MaxAttempts:      num("TRIAL_MAX_ATTEMPTS", 2),
		MaxRunning:       num("TRIAL_MAX_RUNNING", 3),
		Showcase:         list("TRIAL_SHOWCASE"),
		ConfigPath:       cmp.Or(getenv("TRIAL_CONFIG"), DefaultTrialConfig),
		TurnstileSecret:  getenv("TURNSTILE_SECRET"),
		TurnstileSiteKey: getenv("TURNSTILE_SITE_KEY"),
	}
	if len(c.Models) == 0 {
		c.Models = []string{"openrouter/z-ai/glm-5.3"}
	}
	return c
}

const (
	sessionCookie = "kv_session"
	// secureCookie is the name over https: the __Host- prefix makes browsers
	// accept it only with Secure, Path=/ and no Domain.
	secureCookie  = "__Host-kv_session"
	sessionIdle   = 24 * time.Hour
	maxSessions   = 10000
	sessionsPerIP = 10
	maxKeyLen     = 512
	minKeyLen     = 20
	maxConcurrent = 2
)

// modelKeyVars is the variable the agent reads for a model's provider, the
// part of the model name before its first slash.
var modelKeyVars = map[string]string{
	"openrouter": "OPENROUTER_API_KEY",
	"anthropic":  "ANTHROPIC_API_KEY",
	"openai":     "OPENAI_API_KEY",
	"nvidia":     "NVIDIA_API_KEY",
	"zai":        "ZAI_API_KEY",
}

// session is one visitor. It lives in memory only.
type session struct {
	ip     string
	last   time.Time
	keys   *visitorKeys
	invite string // the invite code redeemed; "" if none
	runs   []string
}

type visitorKeys struct {
	model, provider, daytona, modalID, modalSecret string
}

// trial is the state of trial mode.
type trial struct {
	cfg    TrialConfig
	runner *harbor.Runner
	jobs   string
	tasks  string

	usageErr error // invite usage could not be read: invite runs are refused

	confMu    sync.Mutex
	conf      *trialSettings
	confStamp fileStamp

	mu       sync.Mutex
	sessions map[string]*session
	swept    time.Time
	owners   map[string]string     // job id -> SHA-256 of its session's id
	runIP    map[string]string     // job id -> the address that started it; lost on restart
	used     map[string]*InviteUse // SHA-256 of an invite code -> its runs
	paths    []pathRule            // the server's own directories, hidden from responses
	// secrets of the server, and of each trial run while this process lives,
	// replaced in what is served in case Harbor or an agent wrote one into
	// a file.
	serverSecrets []string
	jobSecrets    map[string][]string

	rateMu sync.Mutex
	rate   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newTrial(cfg TrialConfig, runner *harbor.Runner, jobs, tasks string) *trial {
	if cfg.TurnstileURL == "" {
		cfg.TurnstileURL = turnstileVerify
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 2
	}
	if cfg.MaxRunning < 1 {
		cfg.MaxRunning = 3
	}
	if cfg.InviteRuns < 1 {
		cfg.InviteRuns = 5
	}
	if cfg.RatePerMin < 1 {
		cfg.RatePerMin = 20
	}
	t := &trial{
		cfg: cfg, runner: runner, jobs: jobs, tasks: tasks,
		sessions: map[string]*session{}, owners: map[string]string{}, runIP: map[string]string{}, jobSecrets: map[string][]string{}, used: map[string]*InviteUse{},
		rate: map[string]*window{},
	}
	if t.used, t.usageErr = ReadInviteUse(jobs); t.usageErr != nil {
		log.Printf("invite usage unreadable, invite runs are refused: %v", t.usageErr)
	}
	t.conf = t.build(nil)
	t.settings()
	paths, _ := filepath.Glob(filepath.Join(jobs, ".bench", "*.owner"))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			t.owners[strings.TrimSuffix(filepath.Base(p), ".owner")] = strings.TrimSpace(string(b))
		}
	}
	t.paths = pathRules(jobs, tasks, runner.Root)
	names := []string{"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY"}
	for _, v := range modelKeyVars {
		names = append(names, v)
	}
	for _, p := range harbor.Providers {
		names = append(names, p.Keys()...)
	}
	// Both the env file's value and the environment's: Harbor sees only one,
	// but either may have been written somewhere.
	fileVals, _ := harbor.ReadEnv(runner.EnvFile)
	for _, n := range names {
		t.serverSecrets = append(t.serverSecrets, fileVals[n], os.Getenv(n))
	}
	runner.SweepHomes()
	return t
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (t *trial) allowedTasks() []string {
	all, _ := harbor.FindTasks(t.tasks)
	want := t.settings().tasks
	var out []string
	for _, task := range all {
		for _, a := range task.Arms {
			if len(want) == 0 || slices.Contains(want, a.Task) || slices.Contains(want, task.Name) {
				out = append(out, a.Task)
			}
		}
	}
	return out
}

// lookup returns the visitor's session and its id, or nil. A session idle for
// a day is gone.
func (t *trial) lookup(r *http.Request) (string, *session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range []string{secureCookie, sessionCookie} {
		if c, err := r.Cookie(name); err == nil {
			if id, s := t.lookupLocked(c.Value); s != nil {
				return id, s
			}
		}
	}
	return "", nil
}

func (t *trial) lookupLocked(id string) (string, *session) {
	now := time.Now()
	if now.Sub(t.swept) > time.Minute {
		for k, s := range t.sessions {
			if now.Sub(s.last) > sessionIdle {
				delete(t.sessions, k)
			}
		}
		t.swept = now
	}
	s := t.sessions[id]
	if s == nil || now.Sub(s.last) > sessionIdle {
		delete(t.sessions, id)
		return "", nil
	}
	s.last = now
	return id, s
}

func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func sessionCookieFor(r *http.Request, id string) *http.Cookie {
	c := &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: secure(r), MaxAge: int(sessionIdle / time.Second),
	}
	if c.Secure {
		c.Name = secureCookie
	}
	return c
}

// mint starts a new session for the visitor and sets its cookie. It never
// reuses an id, so a cookie planted before the keys were entered is worth
// nothing, and it drops the session the request came with. A client address
// holds at most sessionsPerIP sessions, and a full table drops the one idle
// longest, so no visitor can fill it for everyone else.
func (t *trial) mint(w http.ResponseWriter, r *http.Request) *session {
	ip := t.clientIP(r)
	t.mu.Lock()
	defer t.mu.Unlock()
	// The visitor keeps ownership of the runs they started, and nothing else.
	var carried []string
	for _, name := range []string{secureCookie, sessionCookie} {
		if c, err := r.Cookie(name); err == nil {
			if old := t.sessions[c.Value]; old != nil && carried == nil {
				carried = old.runs
			}
			delete(t.sessions, c.Value)
		}
	}
	evict := func(match func(*session) bool) {
		oldest := ""
		for id, s := range t.sessions {
			if match(s) && (oldest == "" || s.last.Before(t.sessions[oldest].last)) {
				oldest = id
			}
		}
		delete(t.sessions, oldest)
	}
	count := 0
	for _, s := range t.sessions {
		if s.ip == ip {
			count++
		}
	}
	if count >= sessionsPerIP {
		evict(func(s *session) bool { return s.ip == ip })
	}
	if len(t.sessions) >= maxSessions {
		evict(func(*session) bool { return true })
	}
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s := &session{ip: ip, last: time.Now()}
	t.sessions[id] = s
	owner := digest(id)
	for _, run := range carried {
		// A run whose record cannot be rewritten stays with the old id, which
		// nobody holds any more.
		if os.WriteFile(filepath.Join(t.jobs, ".bench", run+".owner"), []byte(owner), 0o644) == nil {
			t.owners[run] = owner
			s.runs = append(s.runs, run)
		}
	}
	w.Header().Del("Set-Cookie")
	http.SetCookie(w, sessionCookieFor(r, id))
	return s
}

// sessionView is GET /api/trial/session; it holds no secret.
type sessionView struct {
	HasKeys  bool     `json:"has_keys"`
	Provider string   `json:"provider"`
	Invite   bool     `json:"invite"`
	Runs     []string `json:"runs"`
	Limits   limits   `json:"limits"`
	// InviteRunsLeft is what the redeemed code may still start.
	InviteRunsLeft   *int   `json:"invite_runs_left,omitempty"`
	TurnstileSiteKey string `json:"turnstile_site_key"`
}

type limits struct {
	Models      []string `json:"models"`
	Tasks       []string `json:"tasks"`
	MaxAttempts int      `json:"max_attempts"`
}

func (t *trial) view(s *session) sessionView {
	v := sessionView{Runs: []string{}, TurnstileSiteKey: t.cfg.TurnstileSiteKey}
	conf := t.settings()
	v.Limits = limits{Models: conf.models, Tasks: t.allowedTasks(), MaxAttempts: conf.maxAttempts}
	if v.Limits.Tasks == nil {
		v.Limits.Tasks = []string{}
	}
	if s == nil {
		return v
	}
	t.mu.Lock()
	v.Runs = append(v.Runs, s.runs...)
	keys, invite := s.keys, s.invite
	left := 0
	if inv, ok := conf.invite(invite); ok && !inv.Revoked {
		left = max(inv.Runs-t.used[digest(invite)].runs(), 0)
	}
	t.mu.Unlock()
	if keys != nil {
		v.HasKeys, v.Provider = true, keys.provider
	}
	if invite != "" {
		v.Invite, v.Provider, v.InviteRunsLeft = true, t.inviteProvider(), &left
	}
	return v
}

func (s *Server) getTrialSession(w http.ResponseWriter, r *http.Request) {
	_, sess := s.trial.lookup(r)
	writeJSON(w, http.StatusOK, s.trial.view(sess))
}

// printable reports whether v is 8-512 visible ASCII characters.
func printable(v string) bool {
	if len(v) < minKeyLen || len(v) > maxKeyLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *Server) postTrialKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelKey         string `json:"model_key"`
		Provider         string `json:"provider"`
		DaytonaAPIKey    string `json:"daytona_api_key"`
		ModalTokenID     string `json:"modal_token_id"`
		ModalTokenSecret string `json:"modal_token_secret"`
	}
	if !readJSONBody(w, r, &req) {
		return
	}
	k := &visitorKeys{model: req.ModelKey, provider: req.Provider}
	need := []string{req.ModelKey}
	switch req.Provider {
	case "daytona":
		k.daytona = req.DaytonaAPIKey
		need = append(need, k.daytona)
	case "modal":
		k.modalID, k.modalSecret = req.ModalTokenID, req.ModalTokenSecret
		need = append(need, k.modalID, k.modalSecret)
	default:
		writeError(w, http.StatusBadRequest, "provider must be daytona or modal")
		return
	}
	for _, v := range need {
		if !printable(v) {
			writeError(w, http.StatusBadRequest, "every key is required: %d-%d visible ASCII characters, no spaces or newlines", minKeyLen, maxKeyLen)
			return
		}
	}
	sess := s.trial.mint(w, r)
	s.trial.mu.Lock()
	sess.keys = k
	s.trial.mu.Unlock()
	writeJSON(w, http.StatusOK, s.trial.view(sess))
}

func (s *Server) deleteTrialKeys(w http.ResponseWriter, r *http.Request) {
	if _, sess := s.trial.lookup(r); sess != nil {
		s.trial.mu.Lock()
		sess.keys, sess.invite = nil, ""
		s.trial.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) postTrialInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !readJSONBody(w, r, &req) {
		return
	}
	inv, ok := s.trial.settings().invite(req.Code)
	if !ok {
		writeError(w, http.StatusForbidden, "invalid invite code")
		return
	}
	if inv.Revoked {
		writeError(w, http.StatusForbidden, "this invite code has been revoked")
		return
	}
	match := inv.Code
	s.trial.mu.Lock()
	spent := s.trial.used[digest(match)].runs() >= inv.Runs
	s.trial.mu.Unlock()
	if spent {
		writeError(w, http.StatusForbidden, "this invite code has no runs left")
		return
	}
	sess := s.trial.mint(w, r)
	s.trial.mu.Lock()
	sess.invite = match
	s.trial.mu.Unlock()
	writeJSON(w, http.StatusOK, s.trial.view(sess))
}

func readJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "body: %v", err)
		return false
	}
	return true
}

// inviteProvider is the remote provider invite runs use: the first one the
// server has its keys for as variables. A credentials file such as
// ~/.modal.toml does not count: the run's HOME is not the operator's.
// t.mu need not be held.
func (t *trial) inviteProvider() string {
	for _, p := range harbor.Providers {
		if p.Name != "daytona" && p.Name != "modal" {
			continue
		}
		if missing, via := t.runner.Credentials(p); len(missing) == 0 && via == "" && t.runner.Problem(p) == "" {
			return p.Name
		}
	}
	return ""
}

// runEnv is the environment Harbor gets for a run: the visitor's keys, or,
// for an invite, the server's keys the run needs and nothing else.
func (t *trial) runEnv(k *visitorKeys, model string) (env map[string]string, provider string, err error) {
	keyVar, ok := modelKeyVars[strings.SplitN(model, "/", 2)[0]]
	if !ok {
		return nil, "", errors.New("no key variable is known for model " + model)
	}
	if k != nil {
		env = map[string]string{keyVar: k.model}
		if k.provider == "daytona" {
			env["DAYTONA_API_KEY"] = k.daytona
		} else {
			env["MODAL_TOKEN_ID"], env["MODAL_TOKEN_SECRET"] = k.modalID, k.modalSecret
		}
		return env, k.provider, nil
	}
	provider = t.inviteProvider()
	if provider == "" || t.runner.Getenv(keyVar) == "" {
		return nil, "", errors.New("the server has no keys for invite runs")
	}
	env = map[string]string{keyVar: t.runner.Getenv(keyVar)}
	for _, p := range harbor.Providers {
		if p.Name == provider {
			for _, k := range p.Keys() {
				if v := t.runner.Getenv(k); v != "" {
					env[k] = v
				}
			}
		}
	}
	return env, provider, nil
}

type trialRunError struct {
	code int
	msg  string
}

func (s *Server) postTrialJob(w http.ResponseWriter, r *http.Request) {
	t := s.trial
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	id, sess := t.lookup(r)
	var keys *visitorKeys
	var invite string
	if sess != nil {
		t.mu.Lock()
		keys, invite = sess.keys, sess.invite
		t.mu.Unlock()
	}
	if keys == nil && invite == "" {
		writeError(w, http.StatusUnauthorized, "add your keys or redeem an invite code first")
		return
	}
	var req struct {
		harbor.RunRequest
		TurnstileToken string `json:"turnstile_token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	rr := req.RunRequest
	conf := t.settings()
	if len(rr.Models) == 0 {
		rr.Models = conf.models[:1]
	}
	for _, m := range rr.Models {
		if !slices.Contains(conf.models, m) {
			writeError(w, http.StatusBadRequest, "model %q is not available in the trial", m)
			return
		}
	}
	allowed := t.allowedTasks()
	for _, task := range rr.Tasks {
		if !slices.Contains(allowed, task) {
			writeError(w, http.StatusBadRequest, "task %q is not available in the trial", task)
			return
		}
	}
	if rr.Attempts == 0 {
		rr.Attempts = 1
	}
	if rr.Attempts < 1 || rr.Attempts > conf.maxAttempts {
		writeError(w, http.StatusBadRequest, "attempts must be 1-%d in the trial", conf.maxAttempts)
		return
	}
	if t.cfg.TurnstileSecret != "" {
		if re := t.verifyTurnstile(r.Context(), req.TurnstileToken, t.clientIP(r)); re != nil {
			writeError(w, re.code, "%s", re.msg)
			return
		}
	}
	rr.Agent, rr.Concurrent, rr.Retries, rr.TimeoutSec, rr.Name = harbor.LiveAgent, maxConcurrent, 0, 0, ""

	// A models list of mixed providers would need several keys.
	env, provider, err := t.runEnv(keys, rr.Models[0])
	for _, m := range rr.Models[1:] {
		if modelKeyVars[strings.SplitN(m, "/", 2)[0]] != modelKeyVars[strings.SplitN(rr.Models[0], "/", 2)[0]] {
			err = errors.New("models of different providers cannot share a key")
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	rr.Env, rr.Environment = env, provider

	name, re := t.startRun(sess, id, t.clientIP(r), invite, rr)
	if re != nil {
		writeError(w, re.code, "%s", re.msg)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, name)
}

// startRun applies the run limits and starts Harbor, all under t.mu so that
// two requests cannot both pass a limit.
func (t *trial) startRun(sess *session, id, ip, invite string, rr harbor.RunRequest) (string, *trialRunError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, run := range sess.runs {
		if t.runner.Alive(run) {
			return "", &trialRunError{http.StatusTooManyRequests, "you already have a run in progress; stop it or wait for it to finish"}
		}
	}
	// A finished run's files were scrubbed by its helper; its secrets are no
	// longer needed here.
	for run := range t.jobSecrets {
		if !t.runner.Alive(run) {
			delete(t.jobSecrets, run)
		}
	}
	for run, rip := range t.runIP {
		switch {
		case !t.runner.Alive(run):
			delete(t.runIP, run)
		case rip == ip:
			return "", &trialRunError{http.StatusTooManyRequests, "another run from your network address is in progress; wait for it to finish"}
		}
	}
	running := 0
	for run := range t.owners {
		if t.runner.Alive(run) {
			running++
		}
	}
	if running >= t.cfg.MaxRunning {
		return "", &trialRunError{http.StatusTooManyRequests, "the trial is busy (" + strconv.Itoa(running) + " runs in progress); try again in a few minutes"}
	}
	var code string
	if invite != "" {
		// A code revoked after the session redeemed it stops here.
		inv, ok := t.settings().invite(invite)
		if !ok || inv.Revoked {
			return "", &trialRunError{http.StatusForbidden, "this invite code has been revoked"}
		}
		if t.usageErr != nil {
			return "", &trialRunError{http.StatusServiceUnavailable, "invite runs are paused until the operator fixes the usage record"}
		}
		code = digest(invite)
		if t.used[code].runs() >= inv.Runs {
			return "", &trialRunError{http.StatusForbidden, "this invite code has no runs left"}
		}
	}
	name, err := t.runner.Start(rr)
	switch {
	case errors.Is(err, harbor.ErrInvalid):
		return "", &trialRunError{http.StatusBadRequest, err.Error()}
	case err != nil:
		return "", &trialRunError{http.StatusInternalServerError, "starting harbor: " + err.Error()}
	}
	owner := digest(id)
	t.owners[name], t.runIP[name] = owner, ip
	for _, v := range rr.Env {
		t.jobSecrets[name] = append(t.jobSecrets[name], v)
	}
	sess.runs = append(sess.runs, name)
	var saveErr error
	if err := os.WriteFile(filepath.Join(t.jobs, ".bench", name+".owner"), []byte(owner), 0o644); err != nil {
		saveErr = err
	}
	if code != "" {
		u := t.used[code]
		if u == nil {
			u = &InviteUse{}
			t.used[code] = u
		}
		u.Count++
		u.Last = time.Now().UTC()
		u.Jobs = append(u.Jobs, name)
		if err := saveInviteUse(t.jobs, t.used); err != nil {
			saveErr = err
		}
	}
	if saveErr != nil {
		t.runner.Stop(name)
		return "", &trialRunError{http.StatusInternalServerError, "recording the run: " + saveErr.Error()}
	}
	return name, nil
}

// verifyTurnstile checks token with the Turnstile service. A nil error means
// it passed.
func (t *trial) verifyTurnstile(ctx context.Context, token, ip string) *trialRunError {
	if token == "" {
		return &trialRunError{http.StatusBadRequest, "turnstile_token is required"}
	}
	form := url.Values{"secret": {t.cfg.TurnstileSecret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.TurnstileURL, strings.NewReader(form.Encode()))
	if err != nil {
		return &trialRunError{http.StatusBadGateway, "turnstile: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.cfg.Client.Do(req)
	if err != nil {
		return &trialRunError{http.StatusBadGateway, "the turnstile check could not be made"}
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil || !out.Success {
		return &trialRunError{http.StatusForbidden, "the turnstile check failed; reload the page and try again"}
	}
	return nil
}

// clientIP is the connection's address. Behind a tunnel on the same machine
// (-trust-cf), the connection is loopback and the real address is the
// CF-Connecting-IP header; from anywhere else the header is the client's
// own claim and is ignored.
func (t *trial) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); t.cfg.TrustCF && ip != nil && ip.IsLoopback() {
		if h := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(h) != nil {
			return h
		}
	}
	return host
}

func (t *trial) allow(ip string) bool {
	t.rateMu.Lock()
	defer t.rateMu.Unlock()
	now := time.Now()
	if len(t.rate) > 10000 {
		for k, w := range t.rate {
			if now.Sub(w.start) > time.Minute {
				delete(t.rate, k)
			}
		}
	}
	w := t.rate[ip]
	if w == nil || now.Sub(w.start) > time.Minute {
		w = &window{start: now}
		t.rate[ip] = w
	}
	w.n++
	return w.n <= t.cfg.RatePerMin
}

// visible reports whether anyone may read the job: a trial run or a showcase run.
func (t *trial) visible(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.owners[id]
	return ok || t.settings().showcase[id]
}

// viewer is the owner digest of the request's session, or "".
func (t *trial) viewer(r *http.Request) string {
	sid, sess := t.lookup(r)
	if sess == nil {
		return ""
	}
	return digest(sid)
}

// owns reports whether the visitor with this session cookie started the job.
func (t *trial) owns(r *http.Request, id string) bool { return t.ownedBy(t.viewer(r), id) }

func (t *trial) ownedBy(viewer, id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return viewer != "" && t.owners[id] == viewer
}

// shapeEvent applies shape to a job event for one viewer.
func (t *trial) shapeEvent(viewer string, e event) event {
	if e.name == "trial" {
		return t.shapeTrialEvent(e)
	}
	if e.name != "job" {
		return e
	}
	var j harbor.Job
	if json.Unmarshal(e.data, &j) != nil {
		return e
	}
	t.shape(&j, t.ownedBy(viewer, j.ID))
	e.data, _ = json.Marshal(j)
	e.data = t.scrubSecrets(j.ID, e.data)
	return e
}

// shapeTrialEvent applies shapeTrial to a trial event.
func (t *trial) shapeTrialEvent(e event) event {
	var tr harbor.Trial
	if json.Unmarshal(e.data, &tr) != nil {
		return e
	}
	t.shapeTrial(&tr)
	e.data, _ = json.Marshal(tr)
	e.data = t.scrubSecrets(tr.Job, e.data)
	return e
}

// shape makes a job as a visitor may see it: it can be stopped only by its
// owner and never paused, resumed, retried or synced.
func (t *trial) shape(j *harbor.Job, owner bool) {
	j.Stoppable = j.Stoppable && owner
	j.Resumable, j.Retryable, j.Sync = false, false, nil
	j.Error = classifyJobError(j.Error)
}

// shapeTrial removes from a trial what only the operator can use or may see:
// the addresses and files of its OpenCode (they name the server's machine),
// and the text of its error, which Harbor wrote and may quote a key.
func (t *trial) shapeTrial(tr *harbor.Trial) {
	if tr.OpenCode != nil {
		oc := *tr.OpenCode
		oc.URL, oc.Web, oc.Export = "", "", ""
		tr.OpenCode = &oc
	}
	tr.Error = classifyTrialError(tr.Error)
}

var (
	exitStatus = regexp.MustCompile(`harbor: exit status (\d+)`)
	// authFailed matches a provider refusing credentials. A bare 401 or 403
	// is not enough: Docker progress prints "403.3kB/40.21MB".
	authFailed = regexp.MustCompile(`(?i)(?:http/[\d.]+\s+|status[ _]?code\W{0,3}|error code:?\s*)(?:401|403)\b|\b(?:401|403)\s+(?:unauthori[sz]ed|forbidden)\b|(?:error|exception)[^\n]{0,40}?[-:]\s*(?:401|403)\b|unauthori[sz]ed|invalid[ _]api[ _]key|incorrect api key|no auth credentials|user not found|invalid[ _]token|token id is invalid|\bauth(?:entication)?error\b`)
	// sandboxStart matches the sandbox failing to come up: Harbor's
	// "docker compose up" failing, which includes the image pull, or timing out.
	sandboxStart   = regexp.MustCompile(`docker compose up failed|EnvironmentStartTimeoutError`)
	sandboxTimeout = regexp.MustCompile(`(?i)timeout|timed out`)
	sandboxLog     = regexp.MustCompile(`(?i)daytona|modal`)
)

const (
	modelKeyRejected   = "the model provider rejected the key"
	sandboxKeyRejected = "the sandbox provider rejected the key"
	sandboxStartFailed = "the sandbox failed to start"
	sandboxStartSlow   = "the sandbox timed out starting"
)

// classifyJobError turns the tail of Harbor's output, which the reader puts
// in a stopped job's error, into a short message that cannot quote a key
// or name a path. The log is already redacted of the run's keys; this keeps
// anything else out of public view.
func classifyJobError(s string) string {
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "Harbor stopped writing"):
		return s
	case sandboxStart.MatchString(s):
		return sandboxStartMessage(s)
	case authFailed.MatchString(s) && sandboxLog.MatchString(s):
		return sandboxKeyRejected
	case authFailed.MatchString(s):
		return modelKeyRejected
	}
	if m := exitStatus.FindStringSubmatch(s); m != nil {
		return "Harbor exited with an error (code " + m[1] + ")"
	}
	return "Harbor exited with an error"
}

func sandboxStartMessage(s string) string {
	if sandboxTimeout.MatchString(s) {
		return sandboxStartSlow
	}
	return sandboxStartFailed
}

// ClassifyError is the message a visitor is shown for the error text of a
// "job" or a "trial". The classifyhost command serves it to the kavach CLI.
func ClassifyError(kind, text string) string {
	if kind == "trial" {
		return classifyTrialError(text)
	}
	return classifyJobError(text)
}

// classifyTrialError keeps the kind of a trial's error (CancelledError,
// AgentTimeoutError) and drops its message.
func classifyTrialError(s string) string {
	switch {
	case s == "":
		return ""
	case sandboxStart.MatchString(s):
		return sandboxStartMessage(s)
	case authFailed.MatchString(s) && sandboxLog.MatchString(s):
		return sandboxKeyRejected
	case authFailed.MatchString(s):
		return modelKeyRejected
	}
	kind, _, _ := strings.Cut(strings.SplitN(s, "\n", 2)[0], ":")
	if f := strings.Fields(kind); len(f) == 1 && len(f[0]) <= 64 {
		return f[0]
	}
	return "the attempt failed"
}

// jobID is the job a request path is about, or "".
func jobID(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/jobs/")
	if !ok {
		rest, ok = strings.CutPrefix(path, "/api/trials/")
	}
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// serve checks a request before the mux sees it: the address's rate, the
// origin of a write, and the job it names, and hides keys in what the mux
// answers.
func (t *trial) serve(s *Server, w http.ResponseWriter, r *http.Request) {
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if write {
		if !t.allow(t.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many requests; wait a minute")
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || (u.Host != r.Host && o != t.cfg.Origin) {
				writeError(w, http.StatusForbidden, "cross-origin request from %s", o)
				return
			}
		}
	}
	if id := jobID(r.URL.Path); id != "" && !t.visible(id) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		// A visitor in use stays signed in; only idle sessions expire.
		if id, sess := t.lookup(r); sess != nil {
			http.SetCookie(w, sessionCookieFor(r, id))
		}
		job := jobID(r.URL.Path)
		w = scrubber{w, func(b []byte) []byte { return scrubPaths(t.paths, t.scrubSecrets(job, b)) }}
	}
	s.mux.ServeHTTP(w, r)
}

// scrubSecrets replaces the server's secrets and the job's in b.
func (t *trial) scrubSecrets(job string, b []byte) []byte {
	t.mu.Lock()
	values := append(slices.Clone(t.serverSecrets), t.jobSecrets[job]...)
	t.mu.Unlock()
	var pairs []string
	for _, v := range values {
		if len(v) < minKeyLen {
			continue
		}
		pairs = append(pairs, v, "[redacted]")
		if q, _ := json.Marshal(v); string(q) != `"`+v+`"` {
			pairs = append(pairs, string(q[1:len(q)-1]), "[redacted]")
		}
	}
	if len(pairs) == 0 {
		return b
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(b)))
}

// scrubber hides, in what is written, the secrets of the server and of the
// job the request names, and the server's directories. A value is found
// within one Write; the API writes each JSON document or event in one.
type scrubber struct {
	http.ResponseWriter
	fn func([]byte) []byte
}

func (w scrubber) Write(b []byte) (int, error) {
	_, err := w.ResponseWriter.Write(w.fn(b))
	return len(b), err
}

func (w scrubber) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w scrubber) Unwrap() http.ResponseWriter { return w.ResponseWriter }
