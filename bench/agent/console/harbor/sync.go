package harbor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultBucket is the R2 bucket runs are backed up to unless R2_BUCKET says
// otherwise.
const DefaultBucket = "kavach-bench-runs"

// R2Config is where runs are backed up: a Cloudflare R2 bucket, read from the
// env file or the process environment like the provider keys.
type R2Config struct {
	AccountID, AccessKeyID, SecretAccessKey, Bucket string
}

// R2 reads the config from the env file, then the environment.
func (r *Runner) R2() R2Config {
	c := R2Config{
		AccountID:       r.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     r.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: r.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          r.Getenv("R2_BUCKET"),
	}
	if c.Bucket == "" {
		c.Bucket = DefaultBucket
	}
	return c
}

// Missing lists the credentials that are not set; sync is off if any is.
func (c R2Config) Missing() []string {
	var m []string
	for _, k := range []struct{ name, val string }{
		{"R2_ACCOUNT_ID", c.AccountID}, {"R2_ACCESS_KEY_ID", c.AccessKeyID}, {"R2_SECRET_ACCESS_KEY", c.SecretAccessKey},
	} {
		if k.val == "" {
			m = append(m, k.name)
		}
	}
	return m
}

// Sync states of a job.
const (
	SyncOff     = "off"
	SyncPending = "pending"
	SyncSyncing = "syncing"
	SyncSynced  = "synced"
	SyncFailed  = "failed"
)

// SyncState is how a job's folder stands against the bucket.
type SyncState struct {
	State string `json:"state"`
	// At is when the last sync finished.
	At *time.Time `json:"at,omitempty"`
	// Files is how many of the job's files are in the bucket.
	Files int `json:"files"`
	// Skipped counts files the secret guard held back; SkippedFiles names
	// the first few and why. Never a secret, not even part of one.
	Skipped      int        `json:"skipped"`
	SkippedFiles []SyncSkip `json:"skipped_files,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// SyncSkip is a file that was not uploaded.
type SyncSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// SyncResult is what one sync did.
type SyncResult struct {
	Uploaded, Unchanged, Skipped int
	Bytes                        int64
	dirty                        bool // the manifest changed
}

// ErrSyncBusy is returned for a job whose Harbor is running or paused: a
// paused job is resumed, and written to again.
var ErrSyncBusy = errors.New("job is running or paused")

// ErrSyncOff is returned when the R2 credentials are not set.
var ErrSyncOff = errors.New("R2 sync is off")

// Syncer copies finished jobs' folders to the R2 bucket, one job at a time
// in the background. It only adds and overwrites: a file deleted locally
// stays in the bucket.
//
// What was uploaded is recorded per job in <jobs>/.bench/sync/<id>.json,
// outside the job's folder, which Harbor and the readers never see.
type Syncer struct {
	Runner *Runner
	Reader *Reader
	// Endpoint, Client and Backoff replace R2's, for tests.
	Endpoint string
	Client   *http.Client
	Backoff  time.Duration

	// afterScan, for tests, runs between a file's scan and its upload.
	afterScan func(path string)

	mu      sync.Mutex
	states  map[string]*SyncState
	mtimes  map[string]time.Time // of the manifest each state was read from or saved to
	cancels map[string]context.CancelFunc
	queued  map[string]bool
	queue   chan string
	once    sync.Once
	cfg     R2Config
	cfgTime time.Time
}

// sweepEvery is how often every job is checked for changes, which catches
// runs a different console started.
const sweepEvery = 10 * time.Minute

// config is the R2 config, re-read at most every two seconds: State is asked
// for every job once a second.
func (s *Syncer) config() R2Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.cfgTime) > 2*time.Second {
		s.cfg, s.cfgTime = s.Runner.R2(), time.Now()
	}
	return s.cfg
}

// Off says why sync is off, or "" if it is on.
func (s *Syncer) Off() string {
	if m := s.Runner.R2().Missing(); len(m) > 0 {
		return strings.Join(m, ", ") + " not set"
	}
	return ""
}

func (s *Syncer) manifestPath(id string) string {
	return filepath.Join(s.Runner.file("sync", ""), id+".json")
}

type manifest struct {
	Files map[string]fileEntry `json:"files"`
	State SyncState            `json:"state"`
}

type fileEntry struct {
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"` // unix nanoseconds
	SHA256 string `json:"sha256"`
	// Skipped is why the secret guard held the file back; it is not in the
	// bucket.
	Skipped string `json:"skipped,omitempty"`
}

func (s *Syncer) loadManifest(id string) manifest {
	m := manifest{Files: map[string]fileEntry{}}
	readJSON(s.manifestPath(id), &m)
	if m.Files == nil {
		m.Files = map[string]fileEntry{}
	}
	return m
}

func (s *Syncer) saveManifest(id string, m manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := s.manifestPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), id+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (s *Syncer) manifestMTime(id string) time.Time {
	if st, err := os.Stat(s.manifestPath(id)); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// State is the job's sync state; nil when s is nil.
func (s *Syncer) State(id string) *SyncState {
	if s == nil {
		return nil
	}
	if len(s.config().Missing()) > 0 {
		return &SyncState{State: SyncOff}
	}
	mt := s.manifestMTime(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	// A manifest saved by another process (bench sync) replaces what is
	// cached, unless this one is syncing the job.
	if st == nil || st.State != SyncSyncing && !mt.Equal(s.mtimes[id]) {
		next := SyncState{State: SyncPending}
		if m := s.loadManifest(id); m.State.State != "" {
			next = m.State
		}
		st = &next
		s.storeState(id, st, mt)
	}
	out := *st
	return &out
}

func (s *Syncer) storeState(id string, st *SyncState, mt time.Time) {
	if s.states == nil {
		s.states, s.mtimes = map[string]*SyncState{}, map[string]time.Time{}
	}
	s.states[id], s.mtimes[id] = st, mt
}

func (s *Syncer) setState(id string, st SyncState) {
	mt := s.manifestMTime(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeState(id, &st, mt)
}

// Cancel stops the sync of id that is running, if any. A new Harbor for the
// job calls it: the folder is about to change.
func (s *Syncer) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
	}
}

func busy(status string) bool {
	return status == JobRunning || status == JobPausing || status == JobPaused
}

// Check says whether id may be synced now.
func (s *Syncer) Check(id string) error {
	j, err := s.Reader.Job(id)
	if err != nil {
		return err
	}
	if busy(j.Status) {
		return ErrSyncBusy
	}
	return nil
}

// Queue syncs id in the background, after any job already syncing.
func (s *Syncer) Queue(id string) {
	if s == nil || !validName(id) {
		return
	}
	s.once.Do(func() {
		s.queue = make(chan string, 1024)
		go s.work()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued == nil {
		s.queued = map[string]bool{}
	}
	if s.queued[id] {
		return
	}
	select {
	case s.queue <- id:
		s.queued[id] = true
	default: // full: the next sweep finds it
		return
	}
}

func (s *Syncer) work() {
	for id := range s.queue {
		s.mu.Lock()
		delete(s.queued, id)
		s.mu.Unlock()
		s.RunOnce(context.Background(), id)
	}
}

// Sweep queues every job that is not running or paused.
func (s *Syncer) Sweep() {
	if s == nil || s.Off() != "" {
		return
	}
	jobs, _ := s.Reader.Jobs()
	for _, j := range jobs {
		if !busy(j.Status) {
			s.Queue(j.ID)
		}
	}
}

// StartSweeps sweeps now, and every ten minutes after.
func (s *Syncer) StartSweeps() {
	go func() {
		for {
			s.Sweep()
			time.Sleep(sweepEvery)
		}
	}()
}

// RunOnce syncs one job and waits for it. A failure is also left in the
// job's state. A sync that a new Harbor for the job cut short leaves it
// pending, to be synced again when that Harbor exits.
func (s *Syncer) RunOnce(ctx context.Context, id string) (res SyncResult, err error) {
	cfg := s.Runner.R2()
	if len(cfg.Missing()) > 0 {
		return res, ErrSyncOff
	}
	if err := s.Check(id); err != nil {
		return res, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]context.CancelFunc{}
	}
	s.cancels[id] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, id)
		s.mu.Unlock()
		cancel()
	}()

	m := s.loadManifest(id)
	was := m.State
	syncing := was
	syncing.State = SyncSyncing
	s.setState(id, syncing)
	defer func() {
		switch {
		case ctx.Err() != nil || errors.Is(err, ErrSyncBusy):
			m.State.State, m.State.Error = SyncPending, ""
			err = ErrSyncBusy
		case err != nil:
			m.State.State, m.State.Error = SyncFailed, err.Error()
		}
		if err != nil || res.dirty || res.Uploaded > 0 || was.State != SyncSynced {
			s.saveManifest(id, m)
		}
		s.setState(id, m.State)
	}()
	err = s.sync(ctx, id, cfg, &m, &res)
	return res, err
}

func (s *Syncer) sync(ctx context.Context, id string, cfg R2Config, m *manifest, res *SyncResult) error {
	c := &S3{Endpoint: s.Endpoint, Region: "auto", Bucket: cfg.Bucket, AccessKey: cfg.AccessKeyID, SecretKey: cfg.SecretAccessKey, Client: s.Client, Backoff: s.Backoff}
	if c.Endpoint == "" {
		c.Endpoint = "https://" + cfg.AccountID + ".r2.cloudflarestorage.com"
	}
	secrets := s.secrets()
	changed := ""
	root := filepath.Join(s.Runner.JobsDir, id)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // deleted while walking
			}
			return err
		}
		if !d.Type().IsRegular() { // not symlinks, which could lead anywhere
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		old, known := m.Files[rel]
		if known && old.Size == info.Size() && old.MTime == info.ModTime().UnixNano() {
			if old.Skipped != "" {
				res.Skipped++
			} else {
				res.Unchanged++
			}
			return nil
		}
		reason, sum, err := scanFile(path, secrets)
		if err != nil {
			return nil // vanished or unreadable: not this sync's failure
		}
		if s.afterScan != nil {
			s.afterScan(path)
		}
		e := fileEntry{Size: info.Size(), MTime: info.ModTime().UnixNano(), SHA256: sum, Skipped: reason}
		switch {
		case reason != "":
			res.Skipped++
		case known && old.Skipped == "" && old.SHA256 == sum:
			res.Unchanged++
		default:
			if s.Runner.alive(id) {
				return ErrSyncBusy
			}
			err := c.Put(ctx, "harbor/"+id+"/"+rel, path, info.Size(), contentType(rel), sum)
			if errors.Is(err, errChanged) {
				if changed == "" {
					changed = rel
				}
				return nil
			}
			if err != nil {
				return err
			}
			res.Uploaded++
			res.Bytes += info.Size()
			if res.Uploaded%25 == 0 { // keep progress if the console dies mid-sync
				saved := *m
				saved.State.State = SyncPending
				s.saveManifest(id, saved)
			}
		}
		m.Files[rel] = e
		res.dirty = true
		return nil
	})
	if err != nil {
		return err
	}
	if changed != "" {
		return fmt.Errorf("%s changed while uploading; it is uploaded by the next sync", changed)
	}
	// At is when anything last went up, or the job became fully synced: a
	// sweep that finds nothing to do leaves it.
	st := SyncState{State: SyncSynced, At: m.State.At}
	if res.Uploaded > 0 || m.State.State != SyncSynced || st.At == nil {
		now := time.Now()
		st.At = &now
	}
	m.State = st
	for rel, e := range m.Files {
		if e.Skipped == "" {
			m.State.Files++
			continue
		}
		m.State.Skipped++
		if len(m.State.SkippedFiles) < 20 {
			m.State.SkippedFiles = append(m.State.SkippedFiles, SyncSkip{rel, e.Skipped})
		}
	}
	return nil
}

var secretName = regexp.MustCompile(`(?i)KEY|TOKEN|SECRET|PASSWORD`)

type secret struct {
	name string
	val  []byte
}

// secrets are the values of every credential the console knows: those in the
// env file and in the environment. Short values are left out; they would
// match ordinary text.
func (s *Syncer) secrets() []secret {
	vals, _ := ReadEnv(s.Runner.EnvFile)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if _, inFile := vals[k]; !inFile {
				vals[k] = v
			}
		}
	}
	var out []secret
	for k, v := range vals {
		if secretName.MatchString(k) && len(v) >= 8 {
			out = append(out, secret{k, []byte(v)})
		}
	}
	return out
}

var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"sk-or-v1-", regexp.MustCompile(`sk-or-v1-[A-Za-z0-9]{20,}`)},
	{"sk-ant-", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
}

const scanChunk = 256 << 10

// scanFile reads path once, returning its SHA-256 and, if it holds a secret,
// why it must not be uploaded. The reason names the key or pattern only.
func scanFile(path string, secrets []secret) (reason, sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	// A match can straddle two reads: each window starts with the end of the
	// last. Patterns are found in 29 bytes; a value needs its own length.
	keep := 64
	for _, sc := range secrets {
		keep = max(keep, len(sc.val))
	}
	h := sha256.New()
	buf := make([]byte, scanChunk)
	var window, tail []byte
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			window = append(append(window[:0], tail...), buf[:n]...)
			if reason = findSecret(window, secrets); reason != "" {
				return reason, "", nil
			}
			tail = append(tail[:0], window[max(0, len(window)-keep+1):]...)
		}
		if rerr == io.EOF {
			return "", hex.EncodeToString(h.Sum(nil)), nil
		}
		if rerr != nil {
			return "", "", rerr
		}
	}
}

func findSecret(b []byte, secrets []secret) string {
	for _, sc := range secrets {
		if bytes.Contains(b, sc.val) {
			return "contains the value of " + sc.name
		}
	}
	for _, p := range secretPatterns {
		if p.re.Match(b) {
			return "matches the pattern " + p.name + "…"
		}
	}
	return ""
}

var contentTypes = map[string]string{
	".json":  "application/json",
	".jsonl": "application/x-ndjson",
	".log":   "text/plain; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".patch": "text/plain; charset=utf-8",
	".diff":  "text/plain; charset=utf-8",
	".toml":  "text/plain; charset=utf-8",
	".yaml":  "text/plain; charset=utf-8",
	".yml":   "text/plain; charset=utf-8",
	".py":    "text/plain; charset=utf-8",
	".sh":    "text/plain; charset=utf-8",
}

func contentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := contentTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// String is a one-line summary for the CLI.
func (r SyncResult) String() string {
	return fmt.Sprintf("%d uploaded (%.1f MB), %d unchanged, %d held back by the secret guard", r.Uploaded, float64(r.Bytes)/1e6, r.Unchanged, r.Skipped)
}
