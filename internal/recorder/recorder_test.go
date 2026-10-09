package recorder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

type session struct {
	t    *testing.T
	enc  *recstream.Encoder
	in   *io.PipeWriter
	out  *bytes.Buffer
	mu   sync.Mutex
	done chan int
	dir  string
}

type lockedBuffer struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func collector(env []string, host func() string) *envfacts.Collector {
	return &envfacts.Collector{
		Root: "/nonexistent", OS: "other",
		Environ:  func() []string { return env },
		Hostname: func() (string, error) { return host(), nil },
	}
}

func start(t *testing.T, cfg Config, open recstream.Open) *session {
	t.Helper()
	pr, pw := io.Pipe()
	s := &session{t: t, in: pw, out: &bytes.Buffer{}, done: make(chan int, 1), dir: t.TempDir()}
	cfg.In, cfg.Out = pr, lockedBuffer{&s.mu, s.out}
	if cfg.Collector == nil {
		cfg.Collector = collector(nil, func() string { return "h" })
	}
	go func() { s.done <- Run(cfg) }()
	s.enc = recstream.NewEncoder(pw)
	open.Service, open.Start, open.Dir = "svc", journal.StartGenesis, s.dir
	if err := s.enc.Open(open); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *session) step(data string, extra ...journal.Record) {
	s.t.Helper()
	recs := append([]journal.Record{{Type: journal.TypeInput, Source: "s", Data: []byte(data)}}, extra...)
	if err := s.enc.Records(recs, true); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) finish() int {
	s.t.Helper()
	s.in.Close()
	select {
	case c := <-s.done:
		return c
	case <-time.After(10 * time.Second):
		s.t.Fatal("recorder did not exit")
		return -1
	}
}

func (s *session) control() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(s.out.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (s *session) segment() string { return filepath.Join(s.dir, "svc-"+s.run()+"-000000.kavach") }

func (s *session) run() string {
	for i := 0; i < 100; i++ {
		for _, m := range s.control() {
			if m["t"] == "ready" {
				return m["run"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatal("no ready message")
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func factMap(r journal.Record) map[string]journal.Fact {
	m := map[string]journal.Fact{}
	for _, f := range r.Facts {
		m[f.Key] = f
	}
	return m
}

func TestGenesisEnvironmentFromCollector(t *testing.T) {
	cfg := Config{Collector: collector([]string{"MODE=prod", "CUSTOM=hide", "DB_DSN=x"}, func() string { return "web-1" })}
	s := start(t, cfg, recstream.Open{SecretKeys: []string{"CUSTOM"}})
	s.step("a")
	if err := s.enc.Close(); err != nil {
		t.Fatal(err)
	}
	if c := s.finish(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	j, err := journal.ReadFile(s.segment())
	if err != nil {
		t.Fatal(err)
	}
	env := factMap(j.Records[0])
	if j.Records[0].Type != journal.TypeEnvironment || !j.Records[0].Critical() {
		t.Fatalf("first record = %+v", j.Records[0])
	}
	if f := env["env.MODE"]; f.Form != journal.FactValue || string(f.Value) != "prod" {
		t.Errorf("MODE = %+v", f)
	}
	for _, k := range []string{"env.CUSTOM", "env.DB_DSN"} {
		if env[k].Form != journal.FactSHA256 {
			t.Errorf("%s is not hashed: %+v", k, env[k])
		}
	}
	if string(env["host.hostname"].Value) != "web-1" {
		t.Errorf("hostname = %+v", env["host.hostname"])
	}
	if _, ok := env["host.runtime"]; ok {
		t.Error("the recorder collected host.runtime")
	}
}

func TestFailureIsOnDiskBeforeClose(t *testing.T) {
	s := start(t, Config{}, recstream.Open{})
	s.step("a", journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerPanic, Message: "boom"})
	waitFor(t, "fixture message", func() bool {
		for _, m := range s.control() {
			if m["t"] == "fixture" {
				return true
			}
		}
		return false
	})
	// No flush, no close: the failure and its fixture are already readable.
	j, err := journal.ReadFile(s.segment())
	if err != nil || j.Records[len(j.Records)-1].Kind != journal.MarkerPanic {
		t.Fatalf("segment: %v %+v", err, j)
	}
	for _, m := range s.control() {
		if m["t"] == "fixture" {
			fj, err := journal.ReadFile(m["file"].(string))
			if err != nil || len(fj.Records) != len(j.Records) || fj.Header.Meta.CutFrom == nil {
				t.Fatalf("fixture: %v %+v", err, fj)
			}
		}
	}
	s.finish()
}

func TestFlushInterval(t *testing.T) {
	s := start(t, Config{}, recstream.Open{FlushMS: 30})
	s.step("a")
	waitFor(t, "the flush interval to write the block", func() bool {
		j, err := journal.ReadFile(s.segment())
		return err == nil && len(j.Records) == 2
	})
	s.finish()
}

func TestHostChangeRecord(t *testing.T) {
	var mu sync.Mutex
	host := "a"
	get := func() string { mu.Lock(); defer mu.Unlock(); return host }
	s := start(t, Config{Collector: collector(nil, get), WatchInterval: 5 * time.Millisecond}, recstream.Open{})
	s.step("one")
	time.Sleep(100 * time.Millisecond) // the watcher polls well within this
	mu.Lock()
	host = "b"
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	s.step("two")
	s.enc.Close()
	s.finish()
	j, err := journal.ReadFile(s.segment())
	if err != nil {
		t.Fatal(err)
	}
	var changes []journal.Record
	for _, r := range j.Records[1:] {
		if r.Type == journal.TypeEnvironment {
			changes = append(changes, r)
		}
	}
	if len(changes) != 1 || len(changes[0].Facts) != 1 || changes[0].Facts[0].Key != "host.hostname" || string(changes[0].Facts[0].Value) != "b" {
		t.Fatalf("changes = %+v", changes)
	}
	// It comes right after the second step_end, not inside a step.
	if last := j.Records[len(j.Records)-1]; last.Type != journal.TypeEnvironment {
		t.Fatalf("last record = %+v", last)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestControlStreamFailureDoesNotStopRecording(t *testing.T) {
	pr, pw := io.Pipe()
	dir := t.TempDir()
	done := make(chan int, 1)
	go func() {
		done <- Run(Config{In: pr, Out: failingWriter{}, Collector: collector(nil, func() string { return "h" })})
	}()
	enc := recstream.NewEncoder(pw)
	enc.Open(recstream.Open{Service: "svc", Start: journal.StartGenesis, Dir: dir})
	enc.Records([]journal.Record{{Type: journal.TypeInput, Source: "s"}}, true)
	enc.Close()
	pw.Close()
	if c := <-done; c != 0 {
		t.Fatalf("exit %d", c)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.kavach"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if j, err := journal.ReadFile(files[0]); err != nil || len(j.Records) != 2 {
		t.Fatalf("journal: %v %+v", err, j)
	}
}

func TestRotationSealsInTheBackground(t *testing.T) {
	s := start(t, Config{}, recstream.Open{Snapshots: true, SegmentBytes: 1})
	requests := func() int {
		n := 0
		for _, m := range s.control() {
			if m["t"] == "snapshot_request" {
				n++
			}
		}
		return n
	}
	for i := 1; i <= 3; i++ {
		s.step("a")
		waitFor(t, "a snapshot request", func() bool { return requests() == i })
		if err := s.enc.Snapshot([]byte("state")); err != nil {
			t.Fatal(err)
		}
		// A durable answer covers the segment that is being sealed.
		if err := s.enc.Flush(true); err != nil {
			t.Fatal(err)
		}
	}
	s.step("b")
	if c := s.finish(); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	for i := 0; i < 4; i++ {
		j, err := journal.ReadFile(filepath.Join(s.dir, fmt.Sprintf("svc-%s-%06d.kavach", s.run(), i)))
		if err != nil || len(j.Records) == 0 {
			t.Fatalf("segment %d: %v %+v", i, err, j)
		}
	}
	left, _ := filepath.Glob(filepath.Join(s.dir, "*"+standbySuffix))
	if len(left) != 0 {
		t.Fatalf("standby segments left behind: %v", left)
	}
}

func TestFlagsAreSetByTheRecorder(t *testing.T) {
	s := start(t, Config{}, recstream.Open{})
	// An SDK that forgets the critical flag does not produce an invalid journal.
	s.step("a",
		journal.Record{Type: journal.TypeGateway, Gateway: "g", Request: []byte("q"), Response: []byte("r")},
		journal.Record{Type: journal.TypeConfig, Key: "k"})
	s.enc.Close()
	s.finish()
	j, err := journal.ReadFile(s.segment())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range j.Records {
		if (r.Type == journal.TypeGateway || r.Type == journal.TypeConfig) && !r.Critical() {
			t.Errorf("%s record is not critical", r.Type)
		}
	}
}
