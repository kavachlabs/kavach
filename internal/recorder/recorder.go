// Package recorder implements kavach-recorder (SPEC.md §3.6, §10): the process
// that reads an SDK's record stream, numbers the records, batches them into
// blocks and writes journal segments and fixtures.
package recorder

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

// Version and Name identify the recorder in the control stream and in headers.
const (
	Version = "0.2.0"
	Name    = "kavach-recorder/" + Version
)

// DefaultWatchInterval is how often host facts are polled.
const DefaultWatchInterval = 5 * time.Second

const (
	exitOK    = 0
	exitFatal = 1
)

// Config is how a recorder is run.
type Config struct {
	// In is the record stream; Out receives the control stream.
	In  io.Reader
	Out io.Writer
	// Log receives the recorder's own logs. Defaults to discarding them.
	Log io.Writer
	// Collector gathers env. and host. facts. Defaults to envfacts.New().
	Collector *envfacts.Collector
	// Test, if set, replaces the collected facts, the run and the time of the
	// header, and turns host watching off (SPEC.md §10.6).
	Test *TestFacts
	// WatchInterval is the host polling interval; DefaultWatchInterval if zero.
	WatchInterval time.Duration
	// Ring is the file the SDK passed as descriptor 3, which an open frame
	// with "ring" says holds the record stream (SPEC.md §10.7).
	Ring *os.File
}

// TestFacts is the content of a --test-facts file.
type TestFacts struct {
	Run        string                       `json:"run"`
	RecordedAt string                       `json:"recorded_at"`
	Facts      map[string]envfacts.JSONFact `json:"facts"`
}

// fatalError is a protocol violation or a failure to write: the recorder
// reports it, finishes the journal and exits.
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

func fatalf(format string, args ...any) error { return &fatalError{fmt.Sprintf(format, args...)} }

type recorder struct {
	cfg  Config
	log  *log.Logger
	test *TestFacts

	ctl   sync.Mutex // guards writes to Out
	ctlOK bool       // false once Out has failed; the recorder keeps recording

	open   recstream.Open
	opened bool
	run    string

	seg     *segment
	standby *segment        // the next segment, prepared; nil while work runs or snapshots are off
	work    chan sealResult // the background seal of the previous segment, nil if none
	started bool
	nextSeq uint64
	lastSeq uint64 // seq of the last record written; meaningful once started

	env     map[string]journal.Fact // the environment as the journal holds it
	pending map[string]journal.Fact // facts from the SDK and the watcher, not yet in the journal
	watcher *envfacts.Watcher

	inStep      bool
	step        []stepRecord
	stepMarker  *journal.Record
	snapRequest bool

	stepsSinceClock int

	timer *time.Timer
	armed bool
}

// Run runs a recorder until the record stream ends, it is closed or it hits a
// fatal error, and returns the process's exit status.
func Run(cfg Config) int {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Collector == nil {
		cfg.Collector = envfacts.New()
	}
	r := &recorder{
		cfg:     cfg,
		log:     log.New(cfg.Log, "kavach-recorder: ", log.LstdFlags),
		test:    cfg.Test,
		ctlOK:   true,
		env:     map[string]journal.Fact{},
		pending: map[string]journal.Fact{},
	}
	return r.loop()
}

type item struct {
	f   recstream.Frame
	err error
}

func (r *recorder) loop() (code int) {
	// Frames cross to the loop in batches: a channel operation per frame costs
	// more than handling it.
	frames := make(chan []item, 16)
	go func() {
		rd := recstream.NewReader(r.cfg.In)
		var batch []item
		send := func(it item) {
			batch = append(batch, it)
			if it.err != nil || len(batch) == 256 || rd.Buffered() == 0 {
				frames <- batch
				batch = nil
			}
		}
		for first := true; ; first = false {
			f, err := rd.Next()
			send(item{f, err})
			if err != nil {
				return
			}
			if first && f.Kind == recstream.KindOpen {
				// Later frames come from the ring, if the open frame asks
				// for one; standard input is then only a doorbell.
				if o, err := recstream.ParseOpen(f.Payload); err == nil && o.Ring > 0 {
					ring, err := r.mapRing(o)
					if err != nil {
						send(item{err: err})
						return
					}
					defer ring.Close()
					rr := ring.NewReader()
					go ringBell(rd.Raw(), rr)
					rd = recstream.NewReader(rr)
				}
			}
		}
	}()
	defer func() {
		if r.watcher != nil {
			r.watcher.Stop()
		}
	}()
	r.timer = time.NewTimer(time.Hour)
	r.timer.Stop()

	var batch []item
	for {
		if len(batch) == 0 {
			select {
			case batch = <-frames:
			case <-r.timer.C:
				r.armed = false
				if r.seg != nil {
					if err := r.seg.w.Flush(); err != nil {
						return r.fail(fatalf("writing %s: %v", r.seg.path, err))
					}
				}
			}
			continue
		}
		it := batch[0]
		batch = batch[1:]

		if it.err != nil {
			if it.err == io.EOF || it.err == io.ErrUnexpectedEOF {
				if it.err == io.ErrUnexpectedEOF {
					r.log.Printf("the record stream ended inside a frame")
				}
				return r.eof()
			}
			return r.fail(fatalf("reading the record stream: %v", it.err))
		}
		done, err := r.handle(it.f)
		if err != nil {
			return r.fail(err)
		}
		if done {
			return exitOK
		}
		if r.seg != nil && r.seg.w.Pending() > 0 && !r.armed {
			r.timer.Reset(time.Duration(r.open.FlushMS) * time.Millisecond)
			r.armed = true
		}
	}
}

func (r *recorder) mapRing(o recstream.Open) (*recstream.Ring, error) {
	if p := o.RingPath; p != "" {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("ring: %w", err)
		}
		defer f.Close()
		os.Remove(p)
		return recstream.OpenRing(f, o.Ring)
	}
	if r.cfg.Ring == nil {
		return nil, errors.New("ring: the open frame asks for one but no file was passed")
	}
	return recstream.OpenRing(r.cfg.Ring, o.Ring)
}

// ringBell turns every byte on standard input into a wake-up, and its end into
// the end of the service.
func ringBell(in io.Reader, rr *recstream.RingReader) {
	var b [64]byte
	for {
		n, err := in.Read(b[:])
		if n > 0 {
			rr.Bell()
		}
		if err != nil {
			rr.End()
			return
		}
	}
}

// fail reports a fatal error and finishes the journal up to the last complete
// step (SPEC.md §10.2).
func (r *recorder) fail(err error) int {
	var fe *fatalError
	if !errors.As(err, &fe) {
		fe = &fatalError{err.Error()}
	}
	r.log.Printf("fatal: %s", fe.msg)
	r.send(map[string]any{"t": "error", "message": fe.msg, "fatal": true})
	r.inStep, r.step, r.stepMarker = false, nil, nil
	if err := r.settle(); err != nil {
		r.log.Printf("finishing a segment: %v", err)
	}
	if r.seg != nil {
		if err := r.seg.close(); err != nil {
			r.log.Printf("finishing %s: %v", r.seg.path, err)
		}
		r.seg = nil
	}
	return exitFatal
}

// eof handles the record stream ending without a close frame (SPEC.md §10.5).
func (r *recorder) eof() int {
	if !r.opened {
		r.log.Printf("the record stream ended before the open frame")
		return exitFatal
	}
	if r.inStep {
		// The service died during the step: that step is the recorded failure.
		if r.stepMarker == nil {
			crash := journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerCrash, Message: "process ended during step"}
			payload, _ := journal.AppendPayload(nil, crash)
			r.addStepRecord(journal.TypeMarker, 0, payload, &crash)
		}
		if err := r.endStep(true); err != nil {
			return r.fail(err)
		}
	} else if r.started {
		if err := r.write(journal.Record{Type: journal.TypeMarker, Kind: journal.MarkerExit, Message: "process ended without closing the recorder"}); err != nil {
			return r.fail(err)
		}
	}
	return r.finish()
}

// finish writes what remains and makes it durable.
func (r *recorder) finish() int {
	err := r.settle()
	if r.seg != nil {
		if cerr := r.seg.close(); cerr != nil {
			cerr = fmt.Errorf("finishing %s: %w", r.seg.path, cerr)
			err = errors.Join(err, cerr)
		}
		r.seg = nil
	}
	if err != nil {
		r.log.Print(err)
		r.send(map[string]any{"t": "error", "message": err.Error(), "fatal": true})
		return exitFatal
	}
	return exitOK
}

// send writes one control message. A failing control stream is logged once and
// does not stop the recording.
func (r *recorder) send(msg map[string]any) {
	r.ctl.Lock()
	defer r.ctl.Unlock()
	if !r.ctlOK || r.cfg.Out == nil {
		return
	}
	b, _ := json.Marshal(msg)
	if _, err := r.cfg.Out.Write(append(b, '\n')); err != nil {
		r.ctlOK = false
		r.log.Printf("the control stream failed, recording continues: %v", err)
	}
}

func (r *recorder) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.log.Print(msg)
	r.send(map[string]any{"t": "error", "message": msg, "fatal": false})
}

func seqString(n uint64) string { return strconv.FormatUint(n, 10) }

// handle processes one frame. It returns true once the recorder is done.
func (r *recorder) handle(f recstream.Frame) (done bool, err error) {
	if !r.opened {
		if f.Kind != recstream.KindOpen {
			return false, fatalf("the first frame must be open, got %s", recstream.KindName(f.Kind))
		}
		return false, r.handleOpen(f.Payload)
	}
	switch f.Kind {
	case recstream.KindOpen:
		return false, fatalf("open frame sent twice")
	case recstream.KindRecord:
		return false, r.handleRecord(f.Payload)
	case recstream.KindStepEnd:
		if !r.inStep {
			return false, fatalf("step_end outside a step")
		}
		return false, r.endStep(false)
	case recstream.KindFacts:
		if r.inStep {
			return false, fatalf("facts frame inside a step")
		}
		facts, err := recstream.ParseFacts(f.Payload)
		if err != nil {
			return false, fatalf("malformed facts frame: %v", err)
		}
		for _, fact := range facts {
			r.pending[fact.Key] = fact
		}
		return false, nil
	case recstream.KindSnapshot:
		if r.inStep {
			return false, fatalf("snapshot frame inside a step")
		}
		state, err := recstream.ParseSnapshot(f.Payload)
		if err != nil {
			return false, fatalf("malformed snapshot frame: %v", err)
		}
		return false, r.handleSnapshot(state)
	case recstream.KindFlush:
		if r.inStep {
			return false, fatalf("flush frame inside a step")
		}
		durable, err := recstream.ParseFlush(f.Payload)
		if err != nil {
			return false, fatalf("malformed flush frame: %v", err)
		}
		return false, r.flush(durable)
	case recstream.KindClose:
		if r.inStep {
			return false, fatalf("close frame inside a step")
		}
		if len(f.Payload) != 0 {
			return false, fatalf("malformed close frame")
		}
		if code := r.finish(); code != exitOK {
			return true, fatalf("could not finish the journal")
		}
		r.send(map[string]any{"t": "closed"})
		return true, nil
	}
	return false, fatalf("unknown frame kind 0x%02x", f.Kind)
}

func (r *recorder) handleOpen(payload []byte) error {
	o, err := recstream.ParseOpen(payload)
	if err != nil {
		return fatalf("%v", err)
	}
	r.open = o.Defaults()
	r.opened = true
	r.run = newRun()
	if r.test != nil && r.test.Run != "" {
		r.run = r.test.Run
	}
	if err := os.MkdirAll(r.open.Dir+"/fixtures", 0o755); err != nil {
		return fatalf("creating %s: %v", r.open.Dir, err)
	}
	r.send(map[string]any{
		"t": "ready", "protocol": recstream.Protocol, "recorder": Name, "run": r.run,
		"file": r.segmentPath(0),
	})
	return nil
}

// newRun returns an identifier that is unique per process start.
func newRun() string {
	var b [4]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func isFailureKind(kind string) bool {
	switch kind {
	case journal.MarkerPanic, journal.MarkerError, journal.MarkerInvariant, journal.MarkerCrash:
		return true
	}
	return false
}

func (r *recorder) handleRecord(payload []byte) error {
	if len(payload) < 2 {
		return fatalf("malformed record frame: record frame is shorter than its type and flags")
	}
	ty, flags, body := journal.Type(payload[0]), payload[1], payload[2:]
	// The record goes into the journal as the SDK encoded it, once it is known
	// to be well formed; only markers are decoded, for what they say.
	if err := journal.ValidatePayload(ty, flags, body); err != nil {
		return fatalf("malformed record frame: %v", err)
	}
	switch ty {
	case journal.TypeEnvironment, journal.TypeSnapshot:
		return fatalf("record frame holds a %s record; the recorder writes those", ty)
	}
	// The recorder writes these, so it sets the flags the spec requires.
	switch ty {
	case journal.TypeGateway, journal.TypeConfig:
		flags |= journal.FlagCritical
	}
	var marker *journal.Record
	if ty == journal.TypeMarker {
		m, _ := journal.ParsePayload(ty, flags, body)
		marker = &m
	}

	if r.inStep {
		switch {
		case ty == journal.TypeInput:
			return fatalf("input record inside a step; the previous step has no step_end")
		case r.stepMarker != nil:
			return fatalf("%s record after the step's marker", ty)
		}
		r.addStepRecord(ty, flags, body, marker)
		return nil
	}

	if r.open.Start == journal.StartSnapshot && !r.started {
		return fatalf("the first frame after open must be a snapshot when start is \"snapshot\"")
	}
	switch {
	case ty == journal.TypeInput:
		if err := r.ensureStarted(); err != nil {
			return err
		}
		r.inStep = true
		r.addStepRecord(ty, flags, body, nil)
		return nil
	case marker != nil && (marker.Kind == journal.MarkerTrigger || marker.Kind == journal.MarkerDropped):
		if err := r.ensureStarted(); err != nil {
			return err
		}
		return r.writeEncoded(ty, flags, body)
	}
	return fatalf("%s record between steps; only an input or a trigger or dropped marker may start here", ty)
}

// stepRecord is a record of the step in progress, as the SDK encoded it.
type stepRecord struct {
	ty      journal.Type
	flags   uint8
	payload []byte
}

func (r *recorder) addStepRecord(ty journal.Type, flags uint8, payload []byte, marker *journal.Record) {
	if marker != nil {
		r.stepMarker = marker
	}
	r.step = append(r.step, stepRecord{ty, flags, payload})
}

// ensureStarted writes the header and genesis environment of a genesis
// journal when its first record arrives.
func (r *recorder) ensureStarted() error {
	if r.started {
		return nil
	}
	return r.startJournal(nil)
}

// collect returns the environment this process starts with.
func (r *recorder) collect() ([]journal.Fact, error) {
	if r.test != nil {
		return envfacts.FromJSON(r.test.Facts)
	}
	return r.cfg.Collector.Facts(r.open.SecretKeys), nil
}

// mergePending moves the facts the SDK and the watcher reported into the
// environment and returns those that differ from what it held.
func (r *recorder) mergePending() []journal.Fact {
	if r.watcher != nil {
		for _, f := range r.watcher.Take() {
			if _, ok := r.pending[f.Key]; !ok {
				r.pending[f.Key] = f
			}
		}
	}
	var changed []journal.Fact
	for k, f := range r.pending {
		old, had := r.env[k]
		switch {
		case f.Form == journal.FactUnset:
			if had {
				delete(r.env, k)
				changed = append(changed, f)
			}
		case !had || old.Form != f.Form || !bytes.Equal(old.Value, f.Value):
			r.env[k] = f
			changed = append(changed, f)
		}
	}
	if len(r.pending) > 0 {
		r.pending = map[string]journal.Fact{}
	}
	journal.SortFacts(changed)
	return changed
}

func (r *recorder) fullEnvironment() journal.Record {
	facts := make([]journal.Fact, 0, len(r.env))
	for _, f := range r.env {
		facts = append(facts, f)
	}
	journal.SortFacts(facts)
	return journal.Record{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: facts}
}

// startJournal creates the first segment: header, the starting snapshot if
// there is one, and the genesis environment (SPEC.md §10.4).
func (r *recorder) startJournal(snapshot []byte) error {
	base, err := r.collect()
	if err != nil {
		return fatalf("test facts: %v", err)
	}
	for _, f := range base {
		r.env[f.Key] = f
	}
	r.mergePending() // facts the SDK sent before now belong to the genesis environment
	start := journal.StartGenesis
	if snapshot != nil {
		start = journal.StartSnapshot
	}
	first, err := r.newSegment(0, start, false)
	if err != nil {
		return fatalf("creating segment 0: %v", err)
	}
	if err := r.beginSegment(first, snapshot); err != nil {
		return err
	}
	r.started = true
	if r.test == nil {
		interval := r.cfg.WatchInterval
		if interval <= 0 {
			interval = DefaultWatchInterval
		}
		r.watcher = envfacts.NewWatcher(r.cfg.Collector, interval, r.fullEnvironment().Facts)
		r.watcher.Start()
	}
	if err := r.prune(r.seg.path); err != nil {
		r.warn("deleting old segments: %v", err)
	}
	if r.open.Snapshots {
		r.seal(nil, r.seg)
	}
	return nil
}

// beginSegment makes seg the one being written and writes its first records.
// snapshot is nil only for the first segment of a genesis journal.
func (r *recorder) beginSegment(seg *segment, snapshot []byte) error {
	r.seg = seg
	seg.firstSeq = r.nextSeq
	if seg.meta.Start == journal.StartSnapshot {
		if err := r.write(journal.Record{Type: journal.TypeSnapshot, Flags: journal.FlagCritical, Data: snapshot}); err != nil {
			return err
		}
	}
	return r.write(r.fullEnvironment())
}

// writeEncoded numbers a record the SDK encoded and adds it to the open block.
func (r *recorder) writeEncoded(ty journal.Type, flags uint8, payload []byte) error {
	if err := r.seg.w.WriteEncoded(r.nextSeq, ty, flags, payload); err != nil {
		return fatalf("writing %s: %v", r.seg.path, err)
	}
	r.lastSeq = r.nextSeq
	r.nextSeq++
	return nil
}

// write numbers a record and adds it to the open block.
func (r *recorder) write(rec journal.Record) error {
	rec.Seq = r.nextSeq
	if err := r.seg.w.Write(rec); err != nil {
		return fatalf("writing %s: %v", r.seg.path, err)
	}
	r.lastSeq = rec.Seq
	r.nextSeq++
	return nil
}

// endStep numbers and writes the buffered step, then does what a step boundary
// calls for: a fixture after a failure, environment changes, a segment
// request. final is set when the stream ended in the step, after which only
// the failure is written.
func (r *recorder) endStep(final bool) error {
	step, marker := r.step, r.stepMarker
	r.inStep, r.stepMarker = false, nil
	inputSeq := r.nextSeq
	for _, rec := range step {
		if err := r.writeEncoded(rec.ty, rec.flags, rec.payload); err != nil {
			return err
		}
	}
	clear(step)
	r.step = step[:0]
	if marker != nil && isFailureKind(marker.Kind) {
		// The failure goes to disk before anything else happens (SPEC.md §3.6).
		if err := r.seg.sync(); err != nil {
			return fatalf("writing %s: %v", r.seg.path, err)
		}
		failure := marker.Kind
		if failure != journal.MarkerCrash {
			failure += ": " + marker.Message
		}
		path, err := r.writeFixture(inputSeq, r.lastSeq)
		if err != nil {
			r.warn("writing the fixture of seq %d: %v", inputSeq, err)
		} else {
			r.send(map[string]any{"t": "fixture", "file": path, "seq": seqString(inputSeq), "failure": failure})
		}
	}
	if final {
		return nil
	}

	if changed := r.mergePending(); len(changed) > 0 {
		if err := r.write(journal.Record{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: changed}); err != nil {
			return err
		}
	}
	return r.maybeRequestSnapshot()
}

func (r *recorder) maybeRequestSnapshot() error {
	// A failed seal surfaces when it is noticed, not only at the next rotation.
	if err := r.reap(false); err != nil {
		return err
	}
	if !r.open.Snapshots || r.snapRequest || r.seg == nil {
		return nil
	}
	// The clock is read every 256 steps: the age limit is in seconds.
	r.stepsSinceClock++
	if r.seg.cw.n.Load() >= r.open.SegmentBytes || r.stepsSinceClock%256 == 0 && time.Since(r.seg.started) >= time.Duration(r.open.SegmentSeconds)*time.Second {
		r.snapRequest = true
		r.send(map[string]any{"t": "snapshot_request"})
	}
	return nil
}

// handleSnapshot starts the journal (when start is "snapshot") or the next
// segment (when the recorder asked for one).
func (r *recorder) handleSnapshot(state []byte) error {
	if state == nil {
		state = []byte{}
	}
	if !r.started {
		if r.open.Start != journal.StartSnapshot {
			return fatalf("snapshot frame before the first input of a genesis journal")
		}
		return r.startJournal(state)
	}
	if !r.snapRequest {
		return fatalf("snapshot frame without a snapshot_request")
	}
	r.snapRequest = false
	// TODO: when a rotation comes while the previous segment is still being
	// sealed, the loop waits here. A third buffer or a backpressure policy is
	// deferred.
	if err := r.reap(true); err != nil {
		return err
	}
	old, next := r.seg, r.standby
	if err := r.activate(next); err != nil {
		return fatalf("starting %s: %v", next.path, err)
	}
	r.standby = nil
	r.mergePending() // the new segment starts from the environment as it stands
	r.seal(old, next)
	if err := r.beginSegment(next, state); err != nil {
		return err
	}
	r.send(map[string]any{"t": "segment", "file": r.seg.path, "first_seq": seqString(r.seg.firstSeq)})
	return nil
}

// sealResult is what the background work of a rotation hands back: the
// prepared standby segment, or the error that stopped it.
type sealResult struct {
	next *segment
	err  error
}

// seal finishes old (nil at the start of the journal) and then prepares the
// segment after active, on a goroutine, so that the loop reading the stream
// does not wait on the compressor, fsync or file creation. At most one runs.
func (r *recorder) seal(old, active *segment) {
	done := make(chan sealResult, 1)
	r.work = done
	path, index := active.path, active.index
	go func() {
		if old != nil {
			if err := old.close(); err != nil {
				done <- sealResult{err: fatalf("finishing %s: %v", old.path, err)}
				return
			}
			if err := r.prune(path); err != nil {
				r.warn("deleting old segments: %v", err)
			}
		}
		next, err := r.newSegment(index+1, journal.StartSnapshot, true)
		if err != nil {
			err = fatalf("creating segment %d: %v", index+1, err)
		}
		done <- sealResult{next, err}
	}()
}

// reap takes the result of the background work: it waits for it if wait is
// set, and otherwise only if it has finished.
func (r *recorder) reap(wait bool) error {
	if r.work == nil {
		return nil
	}
	var res sealResult
	if wait {
		res = <-r.work
	} else {
		select {
		case res = <-r.work:
		default:
			return nil
		}
	}
	r.work, r.standby = nil, res.next
	return res.err
}

// settle waits for the background work and discards the standby segment, which
// is not part of the journal.
func (r *recorder) settle() error {
	err := r.reap(true)
	if s := r.standby; s != nil {
		r.standby = nil
		s.w.Close()
		s.f.Close()
		os.Remove(s.path)
	}
	return err
}

func (r *recorder) flush(durable bool) error {
	if durable {
		// A sealing segment holds records this answer covers.
		if err := r.reap(true); err != nil {
			return err
		}
	}
	if r.seg != nil {
		var err error
		if durable {
			err = r.seg.sync()
		} else {
			err = r.seg.w.Flush()
		}
		if err != nil {
			return fatalf("writing %s: %v", r.seg.path, err)
		}
	}
	if durable {
		// SPEC: before any record is written there is nothing to be durable;
		// the answer then names seq 0 so that an SDK waiting for it proceeds.
		r.send(map[string]any{"t": "durable", "seq": seqString(r.lastSeq)})
	}
	return nil
}
