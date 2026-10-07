package kavach

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// Defaults for Options.
const (
	DefaultSnapshotEvery = 1000
	DefaultMaxRecords    = 100_000
	DefaultDir           = "fixtures"
)

// ErrHistoryLost is returned by Flush when the recorder can no longer write a
// replayable fixture: the handler does not implement Snapshotter and the
// history since genesis outgrew Options.MaxRecords.
var ErrHistoryLost = errors.New("kavach: history since genesis exceeds MaxRecords and the handler is not a Snapshotter")

// Options configure a Recorder.
type Options struct {
	// Service names the service in fixture headers and file names. Defaults to
	// the executable name.
	Service string
	// Dir is where fixtures are written. Defaults to "fixtures".
	Dir string
	// Clock returns the current time. Defaults to time.Now.
	Clock func() time.Time
	// Rand is the source of randomness. Defaults to crypto/rand.Reader.
	Rand io.Reader
	// Deliver executes a step's outputs after the step succeeds. If nil,
	// outputs are only recorded.
	Deliver func(outs []Output) error
	// SnapshotEvery is the number of steps between snapshots when the handler
	// implements Snapshotter. Fixtures then hold at most this many steps
	// before the failing one. Defaults to DefaultSnapshotEvery.
	SnapshotEvery int
	// MaxRecords bounds the history kept for handlers that do not implement
	// Snapshotter. Past it, fixtures can no longer be written. Defaults to
	// DefaultMaxRecords.
	MaxRecords int
	// NoFlushOnError disables writing a fixture when the handler returns an error.
	NoFlushOnError bool
	// RecoverPanics makes Step return a *PanicError instead of re-panicking
	// after the fixture is written.
	RecoverPanics bool
	// NoScrub disables PII scrubbing. By default every fixture is scrubbed
	// before it is written: emails, phone numbers, card numbers, IP addresses
	// and tokens in inputs, outputs and snapshots become stable placeholders,
	// and secret-looking environment variables are masked.
	NoScrub bool
	// NoEnv disables capturing the process environment and host facts into
	// fixture headers.
	NoEnv bool
	// OnFlush, if set, is called after every fixture write attempt.
	OnFlush func(path string, err error)
}

// PanicError is returned by Step when the handler panicked and
// Options.RecoverPanics is set.
type PanicError struct {
	Value   any
	Stack   []byte
	Fixture string
}

func (e *PanicError) Error() string { return fmt.Sprintf("kavach: handler panicked: %v", e.Value) }

// InvariantError is returned by Step when a declared invariant fails after a step.
type InvariantError struct {
	Name    string
	Err     error
	Fixture string
}

func (e *InvariantError) Error() string {
	return fmt.Sprintf("kavach: invariant %s violated: %v", e.Name, e.Err)
}

func (e *InvariantError) Unwrap() error { return e.Err }

// Recorder is an in-process flight recorder. It runs a handler step by step,
// journals every input, clock read, random read and output in memory, and writes
// a fixture when a step panics, returns an error, or violates an invariant.
type Recorder struct {
	mu      sync.Mutex
	h       Handler
	opts    Options
	nextSeq uint64
	start   string
	window  []journal.Record
	steps   int
	lost    bool
	env     recordEnv
	// envRecord is captured when the recorder is created, so it describes the
	// process at startup, not at the moment of failure.
	envRecord *journal.Env
}

// NewRecorder returns a Recorder that drives h.
func NewRecorder(h Handler, opts Options) *Recorder {
	if opts.Service == "" {
		opts.Service = filepath.Base(os.Args[0])
	}
	if opts.Dir == "" {
		opts.Dir = DefaultDir
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.SnapshotEvery <= 0 {
		opts.SnapshotEvery = DefaultSnapshotEvery
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = DefaultMaxRecords
	}
	r := &Recorder{h: h, opts: opts, start: journal.StartGenesis}
	r.env.r = r
	if !opts.NoEnv {
		r.envRecord = CaptureEnv()
	}
	return r
}

func (r *Recorder) add(rec journal.Record) {
	rec.Seq = r.nextSeq
	r.nextSeq++
	if !r.lost {
		r.window = append(r.window, rec)
	}
}

// Step runs the handler on one input.
//
// If the handler panics, the recorder writes a fixture and re-panics with the
// same value (or returns a *PanicError if Options.RecoverPanics is set). If it
// returns an error, the recorder writes a fixture (unless NoFlushOnError) and
// returns the error. If an invariant fails, it writes a fixture and returns an
// *InvariantError. Outputs are delivered only when the step succeeds.
func (r *Recorder) Step(in Input) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.add(journal.Record{Type: journal.TypeInput, Source: in.Source, Position: in.Position, Data: clone(in.Data)})
	r.env.outs = r.env.outs[:0]

	if pv, stack, herr := r.run(in); stack != nil {
		r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerPanic, Message: fmt.Sprint(pv), Data: stack})
		path, _ := r.flushLocked(journal.MarkerPanic)
		r.checkpoint()
		if r.opts.RecoverPanics {
			return &PanicError{Value: pv, Stack: stack, Fixture: path}
		}
		// The re-panic's trace starts here, so print where the handler failed.
		fmt.Fprintf(os.Stderr, "kavach: handler panicked: %v\n%s\n", pv, stack)
		panic(pv)
	} else if herr != nil {
		r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerError, Message: herr.Error()})
		if !r.opts.NoFlushOnError {
			r.flushLocked(journal.MarkerError)
		}
		r.checkpoint()
		return herr
	}

	if name, ierr := checkInvariants(r.h); ierr != nil {
		r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerInvariant, Message: name, Data: []byte(ierr.Error())})
		path, _ := r.flushLocked(journal.MarkerInvariant)
		r.checkpoint()
		return &InvariantError{Name: name, Err: ierr, Fixture: path}
	}

	if r.opts.Deliver != nil && len(r.env.outs) > 0 {
		outs := make([]Output, len(r.env.outs))
		copy(outs, r.env.outs)
		if err := r.opts.Deliver(outs); err != nil {
			return err
		}
	}
	r.afterStep()
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

// afterStep bounds the window: by snapshotting, or by giving up on history.
func (r *Recorder) afterStep() {
	r.steps++
	if _, ok := r.h.(Snapshotter); ok {
		if r.steps >= r.opts.SnapshotEvery {
			r.checkpoint()
		}
		return
	}
	if !r.lost && len(r.window) > r.opts.MaxRecords {
		r.lost = true
		r.window = nil
	}
}

// checkpoint starts a new window from a snapshot of the handler's state, if the
// handler supports it. It runs every SnapshotEvery steps, and after a failure so
// that later fixtures do not begin by replaying an incident already captured.
func (r *Recorder) checkpoint() {
	s, ok := r.h.(Snapshotter)
	if !ok {
		return
	}
	data, err := s.Snapshot()
	if err != nil {
		return // keep the longer window; try again later
	}
	r.window = r.window[:0]
	r.start = journal.StartSnapshot
	r.steps = 0
	r.add(journal.Record{Type: journal.TypeSnapshot, Flags: journal.FlagCritical, Data: data})
}

// Flush writes the current window to a fixture marked with a trigger marker
// carrying reason, and returns its path.
func (r *Recorder) Flush(reason string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerTrigger, Message: reason})
	return r.flushLocked(journal.MarkerTrigger)
}

func (r *Recorder) flushLocked(kind string) (path string, err error) {
	defer func() {
		if r.opts.OnFlush != nil {
			r.opts.OnFlush(path, err)
		}
	}()
	if r.lost {
		return "", ErrHistoryLost
	}
	if err := os.MkdirAll(r.opts.Dir, 0o755); err != nil {
		return "", err
	}
	last := r.window[len(r.window)-1].Seq
	path = filepath.Join(r.opts.Dir, fmt.Sprintf("%s-%06d-%s.kavach", r.opts.Service, last, kind))
	meta := journal.Meta{
		Service:    r.opts.Service,
		Start:      r.start,
		Handler:    buildRevision(),
		Producer:   "kavach-go/" + Version,
		RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Env:        r.envRecord,
	}
	recs := r.window
	if !r.opts.NoScrub {
		meta, recs = NewScrubber().Journal(meta, recs)
	}
	if err := journal.WriteFile(path, meta, recs); err != nil {
		return "", err
	}
	return path, nil
}

// recordEnv is the Env handed to the handler while recording.
type recordEnv struct {
	r    *Recorder
	outs []Output
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
	e.r.add(journal.Record{Type: journal.TypeRand, Data: clone(p)})
	return len(p), nil
}

func (e *recordEnv) Emit(sink string, data []byte) {
	data = clone(data)
	e.r.add(journal.Record{Type: journal.TypeOutput, Sink: sink, Data: data})
	e.outs = append(e.outs, Output{Sink: sink, Data: data})
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
