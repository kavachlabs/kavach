package kavach

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

const (
	closeTimeout = 10 * time.Second
	// failureWait bounds how long a step that is about to re-panic waits for
	// the recorder to make the failure durable.
	failureWait = 5 * time.Second
)

// ErrNotRecording is returned by Flush when the recorder could not be started
// or has stopped, or when a durable flush was not answered in time.
var ErrNotRecording = errors.New("kavach: not recording")

// Options configure a Recorder.
type Options struct {
	// Service names the service in journal headers and file names. Defaults to
	// the executable name.
	Service string
	// RecorderCommand is the argument vector that starts the recorder. If
	// empty, $KAVACH_RECORDER is used, then kavach-recorder on PATH.
	RecorderCommand []string
	// Start is journal.StartGenesis (the default) or journal.StartSnapshot,
	// which needs a Snapshotter: the journal then begins with its state.
	Start string
	// NoSnapshots keeps the recorder from starting new journal segments, which
	// a Snapshotter handler would otherwise answer.
	NoSnapshots bool
	// Clock returns the current time. Defaults to time.Now.
	Clock func() time.Time
	// Rand is the source of randomness. Defaults to crypto/rand.Reader.
	Rand io.Reader
	// Gateway makes the query a handler asked of the external system named
	// gateway. If nil, every query fails.
	Gateway func(gateway string, request []byte) ([]byte, error)
	// Config returns the current value of a config key and where it came
	// from. Defaults to environment variables, with source "env".
	Config func(key string) (value []byte, source string, ok bool)
	// Flags returns the feature flags the process could evaluate, keyed
	// "flag.<name>", for the environment record.
	Flags func() map[string][]byte
	// Deliver executes a step's outputs after the step succeeds. If nil,
	// outputs are only recorded.
	Deliver func(outs []Output) error
	// RecoverPanics makes Step return a *PanicError instead of re-panicking
	// after the recorder has written the fixture.
	RecoverPanics bool
	// NoRing carries the record stream over the recorder's pipe instead of a
	// shared-memory ring (SPEC.md §10.7). The ring is the default on Unix, and
	// the pipe is used where it cannot be set up.
	NoRing bool
	// RingBytes is the ring's capacity, a power of two of at least 64 KiB.
	// Zero means 8 MiB.
	RingBytes int
	// OnFixture, if set, is called with the path and failure of every fixture
	// the recorder reports. It runs on the recorder's control goroutine.
	OnFixture func(file, failure string)

	// The rest are the recorder's own settings (SPEC.md §10.2); zero means its
	// default.
	Dir            string
	Compression    string
	Level          int
	BlockBytes     int
	FlushMS        int
	SegmentBytes   int64
	SegmentSeconds int
	RetainSegments int
	SecretKeys     []string
}

// PanicError is returned by Step when the handler panicked and
// Options.RecoverPanics is set.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("kavach: handler panicked: %v", e.Value) }

// InvariantError is returned by Step when a declared invariant fails after a step.
type InvariantError struct {
	Name string
	Err  error
}

func (e *InvariantError) Error() string {
	return fmt.Sprintf("kavach: invariant %s violated: %v", e.Name, e.Err)
}

func (e *InvariantError) Unwrap() error { return e.Err }

// Recorder runs a handler step by step and writes everything it does to
// kavach-recorder (SPEC.md §10): every input, clock read, random read, gateway
// query, config read and output, and a marker when a step panics, returns an
// error or violates an invariant. The recorder turns those into fixtures.
//
// If the recorder cannot be started, dies, or reports a fatal error, the
// problem is logged, recording stops and steps carry on unrecorded.
type Recorder struct {
	mu        sync.Mutex // steps, writes and flushes
	h         Handler
	opts      Options
	snapshots bool
	enc       *recstream.Encoder
	buf       []byte // frames of the step in progress, reused
	outs      []Output
	env       recordEnv
	requested int // durable flushes sent
	closed    bool

	active  atomic.Bool
	closing atomic.Bool
	snapReq atomic.Bool

	ring      *recstream.Ring // nil on the pipe transport
	belled    bool            // the doorbell rang since the ring was last under half full
	cmd       *exec.Cmd
	stdin     *os.File
	exited    chan struct{}
	closedAck chan struct{}
	ackOnce   sync.Once

	cmu     sync.Mutex // guards what the control goroutine writes
	cond    *sync.Cond
	durable int
	file    string
}

// NewRecorder returns a Recorder that drives h and starts the recorder
// process. It panics if opts.Start is journal.StartSnapshot and h is not a
// Snapshotter.
func NewRecorder(h Handler, opts Options) *Recorder {
	if opts.Service == "" {
		opts.Service = filepath.Base(os.Args[0])
	}
	if opts.Start == "" {
		opts.Start = journal.StartGenesis
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Config == nil {
		opts.Config = func(key string) ([]byte, string, bool) {
			v, ok := os.LookupEnv(key)
			return []byte(v), "env", ok
		}
	}
	_, canSnapshot := h.(Snapshotter)
	if opts.Start == journal.StartSnapshot && !canSnapshot {
		panic("kavach: Start snapshot needs a handler that implements Snapshotter")
	}
	r := &Recorder{h: h, opts: opts, snapshots: canSnapshot && !opts.NoSnapshots,
		exited: make(chan struct{}), closedAck: make(chan struct{})}
	r.cond = sync.NewCond(&r.cmu)
	r.env.r = r
	r.enc = recstream.NewEncoder(pipe{r})
	if err := r.start(); err != nil {
		r.active.Store(true) // so that fail reports it
		r.fail("could not start the recorder: %v", err)
		if r.stdin != nil {
			r.stdin.Close() // the recorder finishes and the control goroutine ends
		} else {
			close(r.exited)
		}
	}
	return r
}

// pipe is the Encoder's writer: it drops frames once recording has stopped.
type pipe struct{ r *Recorder }

func (p pipe) Write(b []byte) (int, error) {
	p.r.publish(b)
	return len(b), nil
}

// publish hands whole frames to the recorder, through the ring or the pipe.
func (r *Recorder) publish(b []byte) {
	if !r.active.Load() {
		return
	}
	if r.ring == nil {
		if _, err := r.stdin.Write(b); err != nil {
			r.fail("could not write to the recorder: %v", err)
		}
		return
	}
	for len(b) > 0 {
		n, used := r.ring.TryPublish(b)
		if n == 0 {
			// Full: the recorder drains the ring on the doorbell. If it has
			// exited, the control goroutine stops recording and ends the wait.
			// TODO: a writer that outruns the recorder (e.g. while it starts,
			// or is slow to be scheduled) blocks the service here; bounding it
			// is deferred.
			r.bell()
			for n == 0 && r.active.Load() {
				time.Sleep(20 * time.Microsecond)
				n, used = r.ring.TryPublish(b)
			}
			if n == 0 {
				return
			}
		}
		b = b[n:]
		if half := r.ring.Capacity() / 2; used > half && !r.belled {
			r.belled = true
			r.bell()
		} else if used <= half {
			r.belled = false
		}
	}
}

// bell wakes a recorder that reads the ring (SPEC.md §10.7).
func (r *Recorder) bell() {
	if r.ring == nil || !r.active.Load() {
		return
	}
	if _, err := r.stdin.Write([]byte{1}); err != nil {
		r.fail("could not write to the recorder: %v", err)
	}
}

func recorderCommand(argv []string) ([]string, error) {
	if len(argv) > 0 {
		return argv, nil
	}
	if p := os.Getenv("KAVACH_RECORDER"); p != "" {
		return []string{p}, nil
	}
	p, err := exec.LookPath("kavach-recorder")
	if err != nil {
		return nil, errors.New("kavach-recorder not found (set Options.RecorderCommand, $KAVACH_RECORDER or put it on PATH)")
	}
	return []string{p}, nil
}

func (r *Recorder) start() error {
	argv, err := recorderCommand(r.opts.RecorderCommand)
	if err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	// The environment is left unchanged: the recorder reads the env. facts from it (§10.1).
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stderr = pr, os.Stderr
	ring, ringFile := r.newRing()
	if ring != nil {
		cmd.ExtraFiles = []*os.File{ringFile}
	}
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	pr.Close()
	if ringFile != nil {
		ringFile.Close()
	}
	if err != nil {
		pw.Close()
		if ring != nil {
			ring.Close()
		}
		return err
	}
	setPipeSize(pw)
	r.cmd, r.stdin = cmd, pw
	r.active.Store(true)
	go r.control(out)

	o := r.opts
	r.enc.Open(recstream.Open{
		Service: o.Service, Start: o.Start, Producer: "kavach-go/" + Version, Handler: buildRevision(),
		Snapshots: r.snapshots, Dir: o.Dir, Compression: o.Compression, Level: o.Level,
		BlockBytes: o.BlockBytes, FlushMS: o.FlushMS, SegmentBytes: o.SegmentBytes,
		SegmentSeconds: o.SegmentSeconds, RetainSegments: o.RetainSegments, SecretKeys: o.SecretKeys,
		Ring: ringCapacity(ring),
	})
	r.ring = ring
	facts := []journal.Fact{{Key: "host.runtime", Form: journal.FactValue, Value: []byte(runtime.Version())}}
	if o.Flags != nil {
		for k, v := range o.Flags() {
			facts = append(facts, journal.Fact{Key: k, Form: journal.FactValue, Value: v})
		}
	}
	journal.SortFacts(facts)
	r.enc.Facts(facts)
	if o.Start == journal.StartSnapshot {
		data, err := r.h.(Snapshotter).Snapshot()
		if err != nil {
			return fmt.Errorf("snapshot of the starting state: %w", err)
		}
		r.enc.Snapshot(data)
	}
	return nil
}

type control struct {
	T       string `json:"t"`
	File    string `json:"file"`
	Failure string `json:"failure"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// newRing sets up the ring unless the application asked for the pipe. When it
// cannot be set up, recording goes on over the pipe.
func (r *Recorder) newRing() (*recstream.Ring, *os.File) {
	if r.opts.NoRing {
		return nil, nil
	}
	capacity := r.opts.RingBytes
	if capacity == 0 {
		capacity = recstream.DefaultRing
	}
	dir := os.TempDir()
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		dir = "/dev/shm"
	}
	ring, f, err := recstream.CreateRing(dir, capacity)
	if err != nil {
		log.Printf("kavach: no shared-memory ring, recording over the pipe: %v", err)
		return nil, nil
	}
	return ring, f
}

func ringCapacity(g *recstream.Ring) int {
	if g == nil {
		return 0
	}
	return int(g.Capacity())
}

func (r *Recorder) control(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m control
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.T == "" {
			log.Printf("kavach: unreadable message from the recorder: %.200s", sc.Bytes())
			continue
		}
		r.onControl(m)
	}
	r.cmd.Wait()
	if !r.closing.Load() {
		r.fail("the recorder exited unexpectedly")
	}
	r.ackOnce.Do(func() { close(r.closedAck) })
	close(r.exited)
	r.broadcast()
}

func (r *Recorder) onControl(m control) {
	switch m.T {
	case "ready":
		r.cmu.Lock()
		r.file = m.File
		r.cmu.Unlock()
	case "snapshot_request":
		r.snapReq.Store(true)
	case "durable":
		r.cmu.Lock()
		r.durable++
		r.cmu.Unlock()
		r.broadcast()
	case "fixture":
		log.Printf("kavach: wrote fixture %s (%s)", m.File, m.Failure)
		if r.opts.OnFixture != nil {
			r.opts.OnFixture(m.File, m.Failure)
		}
	case "error":
		log.Printf("kavach: recorder error: %s", m.Message)
		if m.Fatal {
			r.fail("the recorder reported a fatal error: %s", m.Message)
		}
	case "closed":
		r.ackOnce.Do(func() { close(r.closedAck) })
	}
}

// fail stops recording, loudly, once.
func (r *Recorder) fail(format string, args ...any) {
	if r.active.Swap(false) && !r.closing.Load() {
		log.Printf("kavach: "+format+"; recording has stopped and steps run unrecorded", args...)
	}
	r.broadcast()
}

func (r *Recorder) broadcast() {
	r.cmu.Lock()
	r.cond.Broadcast()
	r.cmu.Unlock()
}

// File returns the path of the journal's first segment, once the recorder has
// reported it.
func (r *Recorder) File() string {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	return r.file
}

func (r *Recorder) add(rec journal.Record) {
	r.buf, _ = recstream.AppendRecordFrame(r.buf, rec)
}

// Step runs the handler on one input.
//
// If the handler panics, the recorder writes the failure and a fixture, and
// Step re-panics with the same value (or returns a *PanicError if
// Options.RecoverPanics is set). If it returns an error, Step returns it. If an
// invariant fails, Step returns an *InvariantError. Outputs are delivered only
// when the step succeeds. Before re-panicking, Step waits briefly for the
// recorder to make the failure durable, so that the fixture is logged.
func (r *Recorder) Step(in Input) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.answerSnapshotRequest()
	r.outs = r.outs[:0]
	// The input goes in before the handler runs, so that a step that kills the
	// process still leaves it on record (§10.2).
	r.buf = r.buf[:0]
	r.add(journal.Record{Type: journal.TypeInput, Source: in.Source, Position: in.Position, Data: in.Data})
	r.publish(r.buf)
	r.buf = r.buf[:0]

	pv, stack, herr := r.run(in)
	var ierr *InvariantError
	switch {
	case stack != nil:
		r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerPanic, Message: fmt.Sprint(pv), Data: stack})
	case herr != nil:
		r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerError, Message: herr.Error()})
	default:
		if name, err := CheckInvariants(r.h); err != nil {
			ierr = &InvariantError{Name: name, Err: err}
			r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerInvariant, Message: name, Data: []byte(err.Error())})
		}
	}
	r.buf = recstream.AppendFrame(r.buf, recstream.KindStepEnd, nil)
	r.publish(r.buf)

	switch {
	case stack != nil:
		if r.opts.RecoverPanics {
			return &PanicError{Value: pv, Stack: stack}
		}
		if r.sendFlush(true) {
			r.waitDurable(r.requested, failureWait)
		}
		// The re-panic's trace starts here, so print where the handler failed.
		fmt.Fprintf(os.Stderr, "kavach: handler panicked: %v\n%s\n", pv, stack)
		panic(pv)
	case herr != nil:
		return herr
	case ierr != nil:
		return ierr
	}
	if r.opts.Deliver != nil && len(r.outs) > 0 {
		return r.opts.Deliver(append([]Output(nil), r.outs...))
	}
	return nil
}

func (r *Recorder) run(in Input) (pv any, stack []byte, err error) {
	defer func() {
		if v := recover(); v != nil {
			pv, stack = v, debug.Stack()
		}
	}()
	return nil, nil, r.h.Handle(&r.env, in)
}

// answerSnapshotRequest answers the recorder's request for a new segment with
// the state at this step boundary (§10.4).
func (r *Recorder) answerSnapshotRequest() {
	if !r.snapReq.CompareAndSwap(true, false) || !r.snapshots || !r.active.Load() {
		return
	}
	data, err := r.h.(Snapshotter).Snapshot()
	if err != nil {
		log.Printf("kavach: snapshot failed, staying in the current segment: %v", err)
		return
	}
	r.enc.Snapshot(data)
}

// sendFlush writes a flush frame; the caller holds mu.
func (r *Recorder) sendFlush(durable bool) bool {
	if !r.active.Load() {
		return false
	}
	if durable {
		r.requested++
	}
	r.enc.Flush(durable)
	r.bell()
	return r.active.Load()
}

func (r *Recorder) waitDurable(target int, timeout time.Duration) bool {
	t := time.AfterFunc(timeout, r.broadcast)
	defer t.Stop()
	deadline := time.Now().Add(timeout)
	r.cmu.Lock()
	defer r.cmu.Unlock()
	for r.durable < target && r.active.Load() && time.Now().Before(deadline) {
		r.cond.Wait()
	}
	return r.durable >= target
}

// Flush asks the recorder to close its open block now. With durable it also
// waits until everything recorded so far is on disk. It returns
// ErrNotRecording if recording has stopped or the wait timed out. Call it
// between steps, never from a handler.
func (r *Recorder) Flush(durable bool) error {
	r.mu.Lock()
	ok := r.sendFlush(durable)
	target := r.requested
	r.mu.Unlock()
	if !ok || durable && !r.waitDurable(target, closeTimeout) {
		return ErrNotRecording
	}
	return nil
}

// Close shuts the recorder down in order: it finishes the journal and waits
// for the recorder to exit. Call it when the service shuts down; without it
// the journal ends with an exit marker (SPEC.md §10.5).
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.active.Load() {
		r.closing.Store(true)
		r.enc.Close()
		r.bell()
		select {
		case <-r.closedAck:
		case <-time.After(closeTimeout):
			log.Printf("kavach: the recorder did not answer close within %s", closeTimeout)
		}
	}
	r.closing.Store(true)
	r.active.Store(false)
	if r.stdin != nil {
		r.stdin.Close()
	}
	select {
	case <-r.exited:
		if r.ring != nil {
			r.ring.Close()
			r.ring = nil
		}
	case <-time.After(2 * time.Second):
		log.Printf("kavach: the recorder is still running after close")
	}
}

// recordEnv is the Env handed to the handler while recording.
type recordEnv struct {
	r *Recorder
}

func (e *recordEnv) Now() time.Time {
	n := e.r.opts.Clock().UnixNano()
	e.r.add(journal.Record{Type: journal.TypeClock, UnixNanos: n})
	return time.Unix(0, n).UTC()
}

func (e *recordEnv) Read(p []byte) (int, error) {
	if _, err := io.ReadFull(e.r.opts.Rand, p); err != nil {
		return 0, err
	}
	e.r.add(journal.Record{Type: journal.TypeRand, Data: p})
	return len(p), nil
}

func (e *recordEnv) Query(gateway string, request []byte) ([]byte, error) {
	var resp []byte
	err := errors.New("kavach: no gateway configured")
	if e.r.opts.Gateway != nil {
		resp, err = e.r.opts.Gateway(gateway, request)
	}
	rec := journal.Record{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: gateway, Request: request, Scope: journal.ScopeRemote}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.Response = resp
	}
	e.r.add(rec)
	return resp, err
}

func (e *recordEnv) Config(key string) ([]byte, bool) {
	v, source, ok := e.r.opts.Config(key)
	if !ok {
		v = nil
	}
	e.r.add(journal.Record{Type: journal.TypeConfig, Flags: journal.FlagCritical, Key: key, Present: ok, Value: v, Source: source})
	return v, ok
}

func (e *recordEnv) Emit(sink string, data []byte) {
	e.r.add(journal.Record{Type: journal.TypeOutput, Sink: sink, Data: data, Scope: journal.ScopeRemote})
	if e.r.opts.Deliver != nil {
		e.r.outs = append(e.r.outs, Output{Sink: sink, Data: clone(data)})
	}
}

func clone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return bi.Main.Path + "@" + bi.Main.Version
	}
	if dirty {
		rev += "+dirty"
	}
	return bi.Main.Path + "@" + rev
}
