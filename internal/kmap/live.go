package kmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

const (
	defaultLiveInterval  = 500 * time.Millisecond
	defaultLiveKeepalive = 15 * time.Second
)

// LiveConfig configures a recorder-driven LiveServer.
type LiveConfig struct {
	WatchDirs []string
	Fixtures  []string
	SpecPath  string
	Bin       string
	New       string
	Diffs     []string
	Overlays  []string
	// Sources are raw --source values (DIR or GROUP=DIR), parsed against
	// the spec on every rebuild.
	Sources   []string
	Patches   []string
	MaxData   int
	BaseDir   string
	Version   string
	Title     string
	Interval  time.Duration
	Keepalive time.Duration
	Replay    func(j *journal.Journal, bin string) (*replay.Result, error)
}

// LiveEvent is the JSON payload emitted on /api/events SSE updates.
type LiveEvent struct {
	Revision  uint64 `json:"revision"`
	UpdatedAt string `json:"updated_at"`
	Fixtures  int    `json:"fixtures"`
}

// LiveServer watches recorder directories and static inputs, rebuilds the map
// model when files change, and serves the live HTML UI, JSON model, and SSE stream.
type LiveServer struct {
	cfg     LiveConfig
	absBase string

	refreshMu sync.Mutex

	mu        sync.RWMutex
	model     *Model
	modelJSON []byte
	revision  uint64
	updatedAt time.Time
	lastFP    string
	subs      map[chan LiveEvent]struct{}

	startMu   sync.Mutex
	started   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// NewLiveServer creates a LiveServer and performs an initial model build.
func NewLiveServer(cfg LiveConfig) (*LiveServer, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultLiveInterval
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = defaultLiveKeepalive
	}
	if cfg.MaxData < 0 {
		return nil, fmt.Errorf("max-data must not be negative")
	}

	baseDir := cfg.BaseDir
	if baseDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		baseDir = wd
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}
	cfg.BaseDir = absBase

	s := &LiveServer{
		cfg:     cfg,
		absBase: absBase,
		subs:    make(map[chan LiveEvent]struct{}),
		done:    make(chan struct{}),
	}
	if _, err := s.Refresh(); err != nil {
		return nil, err
	}
	return s, nil
}

// Start begins background polling at cfg.Interval until ctx is cancelled or Close is called.
func (s *LiveServer) Start(ctx context.Context) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}

	if ctx == nil {
		ctx = context.Background()
	}
	pollCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.started = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-s.done:
				return
			case <-ticker.C:
				_, _ = s.Refresh()
			}
		}
	}()
}

// Close stops background polling and terminates active SSE streams.
func (s *LiveServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.startMu.Lock()
		cancel := s.cancel
		s.startMu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.wg.Wait()
	})
	return nil
}

// Revision returns the current model revision number.
func (s *LiveServer) Revision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

// Refresh scans watched paths, checks the fingerprint, and rebuilds the model if changed.
func (s *LiveServer) Refresh() (bool, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	scan := s.scanInputs()

	s.mu.RLock()
	unchanged := s.revision > 0 && scan.fingerprint == s.lastFP
	s.mu.RUnlock()
	if unchanged {
		return false, nil
	}

	m, err := s.buildFromScan(scan)
	if err != nil {
		return false, err
	}

	rawJSON, err := json.Marshal(m)
	if err != nil {
		return false, fmt.Errorf("marshal live model: %w", err)
	}

	now := time.Now().UTC()

	s.mu.Lock()
	s.revision++
	rev := s.revision
	s.model = m
	s.modelJSON = rawJSON
	s.updatedAt = now
	s.lastFP = scan.fingerprint
	ev := LiveEvent{
		Revision:  rev,
		UpdatedAt: now.Format(time.RFC3339Nano),
		Fixtures:  len(m.Fixtures),
	}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
	s.mu.Unlock()

	return true, nil
}

// ServeHTTP handles /, /api/model, and /api/events.
func (s *LiveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveIndex(w, r)
	case "/api/model":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveModel(w, r)
	case "/api/events":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *LiveServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	m := s.model
	s.mu.RUnlock()

	var buf bytes.Buffer
	if err := RenderLiveHTML(&buf, m); err != nil {
		http.Error(w, "failed to render map", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *LiveServer) serveModel(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	raw := s.modelJSON
	rev := s.revision
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Kavach-Revision", strconv.FormatUint(rev, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(raw)
}

func (s *LiveServer) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := make(chan LiveEvent, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	initEv := LiveEvent{
		Revision:  s.revision,
		UpdatedAt: s.updatedAt.Format(time.RFC3339Nano),
		Fixtures:  len(s.model.Fixtures),
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	if err := writeSSEEvent(w, initEv); err != nil {
		return
	}
	flusher.Flush()

	keepalive := time.NewTicker(s.cfg.Keepalive)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case ev := <-ch:
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, ev LiveEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: update\ndata: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

type scanResult struct {
	fixtures    []string
	fingerprint string
	warnings    []string
}

func (s *LiveServer) scanInputs() scanResult {
	var warnings []string
	fpEntries := make([]string, 0, 32)
	seenFixtures := make(map[string]bool)
	var fixtures []string

	recordFile := func(kind, rawPath string) (string, os.FileInfo, bool) {
		abs, err := filepath.Abs(rawPath)
		if err != nil {
			fpEntries = append(fpEntries, fmt.Sprintf("%s\x00%s\x00err", kind, rawPath))
			warnings = append(warnings, fmt.Sprintf("live: cannot resolve %s %s: %v", kind, rawPath, err))
			return "", nil, false
		}
		info, err := os.Stat(abs)
		if err != nil {
			fpEntries = append(fpEntries, fmt.Sprintf("%s\x00%s\x00missing", kind, abs))
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("live: cannot stat %s %s: %v", kind, relPath(s.absBase, abs), err))
			}
			return abs, nil, false
		}
		fpEntries = append(fpEntries, fmt.Sprintf("%s\x00%s\x00%d\x00%d", kind, abs, info.Size(), info.ModTime().UnixNano()))
		return abs, info, true
	}

	if s.cfg.SpecPath != "" {
		recordFile("spec", s.cfg.SpecPath)
	}
	for _, dp := range s.cfg.Diffs {
		recordFile("diff", dp)
	}
	for _, op := range s.cfg.Overlays {
		recordFile("overlay", op)
	}
	for _, fp := range s.cfg.Fixtures {
		abs, info, ok := recordFile("fixture", fp)
		if ok && !info.IsDir() && !seenFixtures[abs] {
			seenFixtures[abs] = true
			fixtures = append(fixtures, abs)
		}
	}

	for _, dir := range s.cfg.WatchDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			fpEntries = append(fpEntries, fmt.Sprintf("watch\x00%s\x00err", dir))
			warnings = append(warnings, fmt.Sprintf("live: cannot resolve watch dir %s: %v", dir, err))
			continue
		}
		info, err := os.Stat(absDir)
		if err != nil {
			fpEntries = append(fpEntries, fmt.Sprintf("watch\x00%s\x00missing", absDir))
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("live: cannot stat watch dir %s: %v", relPath(s.absBase, absDir), err))
			}
			continue
		}
		if !info.IsDir() {
			if strings.HasSuffix(strings.ToLower(info.Name()), ".kavach") && !seenFixtures[absDir] {
				seenFixtures[absDir] = true
				fixtures = append(fixtures, absDir)
				fpEntries = append(fpEntries, fmt.Sprintf("fixture\x00%s\x00%d\x00%d", absDir, info.Size(), info.ModTime().UnixNano()))
			}
			continue
		}

		walkErr := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != absDir && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".kavach") {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if !seenFixtures[path] {
				seenFixtures[path] = true
				fixtures = append(fixtures, path)
			}
			fpEntries = append(fpEntries, fmt.Sprintf("fixture\x00%s\x00%d\x00%d", path, fi.Size(), fi.ModTime().UnixNano()))
			return nil
		})
		if walkErr != nil {
			warnings = append(warnings, fmt.Sprintf("live: error walking %s: %v", relPath(s.absBase, absDir), walkErr))
		}
	}

	sort.Strings(fixtures)
	sort.Strings(fpEntries)

	h := sha256.New()
	for _, entry := range fpEntries {
		h.Write([]byte(entry))
		h.Write([]byte{'\n'})
	}
	return scanResult{
		fixtures:    fixtures,
		fingerprint: hex.EncodeToString(h.Sum(nil)),
		warnings:    warnings,
	}
}

func (s *LiveServer) buildFromScan(scan scanResult) (*Model, error) {
	warnings := append([]string(nil), scan.warnings...)

	var spec *Spec
	if s.cfg.SpecPath != "" {
		sp, err := LoadSpec(s.cfg.SpecPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("live: skipped unreadable spec %s: %v", relPath(s.absBase, s.cfg.SpecPath), err))
		} else {
			spec = sp
		}
	}

	validFixtures := make([]string, 0, len(scan.fixtures))
	for _, fp := range scan.fixtures {
		if _, err := journal.ReadFile(fp); err != nil {
			warnings = append(warnings, fmt.Sprintf("live: skipped unreadable fixture %s: %v", relPath(s.absBase, fp), err))
			continue
		}
		validFixtures = append(validFixtures, fp)
	}

	validDiffs := make([]string, 0, len(s.cfg.Diffs))
	for _, dp := range s.cfg.Diffs {
		if _, _, err := LoadDiff(dp); err != nil {
			warnings = append(warnings, fmt.Sprintf("live: skipped unreadable diff %s: %v", relPath(s.absBase, dp), err))
			continue
		}
		validDiffs = append(validDiffs, dp)
	}

	validOverlays := make([]string, 0, len(s.cfg.Overlays))
	for _, op := range s.cfg.Overlays {
		if _, err := LoadOverlay(op); err != nil {
			warnings = append(warnings, fmt.Sprintf("live: skipped unreadable overlay %s: %v", relPath(s.absBase, op), err))
			continue
		}
		validOverlays = append(validOverlays, op)
	}

	var roots []SourceRoot
	if len(s.cfg.Sources) > 0 {
		r, err := ParseSourceRoots(s.cfg.Sources, spec)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("live: %v", err))
		} else {
			roots = r
		}
	}

	m, err := Build(Input{
		Sources:  roots,
		Patches:  s.cfg.Patches,
		Spec:     spec,
		Fixtures: validFixtures,
		Bin:      s.cfg.Bin,
		New:      s.cfg.New,
		Diffs:    validDiffs,
		Overlays: validOverlays,
		MaxData:  s.cfg.MaxData,
		BaseDir:  s.absBase,
		Version:  s.cfg.Version,
		Title:    s.cfg.Title,
		Replay:   s.cfg.Replay,
	})
	if err != nil {
		return nil, err
	}

	if m.Groups == nil {
		m.Groups = []Group{}
	}
	if m.Nodes == nil {
		m.Nodes = []Node{}
	}
	if m.Edges == nil {
		m.Edges = []Edge{}
	}
	if len(warnings) > 0 {
		m.Warnings = append(m.Warnings, warnings...)
	}
	return m, nil
}
