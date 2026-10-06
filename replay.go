package kavach

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// Status is the outcome of replaying a fixture.
type Status string

const (
	// StatusOK: the journal recorded no failure, and replay matched it.
	StatusOK Status = "ok"
	// StatusFixed: the journal recorded a failure, replay ran every step
	// without failing, and every step before the recorded failure produced the
	// recorded outputs.
	StatusFixed Status = "fixed"
	// StatusStillFailing: a step panicked or returned an error during replay.
	StatusStillFailing Status = "still_failing"
	// StatusDiverged: a step before the recorded failure produced different
	// outputs from the ones recorded.
	StatusDiverged Status = "diverged"
	// StatusInvariantViolated: a declared invariant failed after a step.
	StatusInvariantViolated Status = "invariant_violated"
	// StatusNondeterministic: the handler's clock or random reads did not
	// match the journal, so the handler is not deterministic under Env.
	StatusNondeterministic Status = "nondeterministic"
)

// Failure describes a failure recorded in a journal.
type Failure struct {
	Seq     uint64 `json:"seq"` // seq of the input whose step failed
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// StepResult is what one step produced during replay.
type StepResult struct {
	Seq     uint64   `json:"seq"` // seq of the step's input
	Outputs []Output `json:"outputs"`
	Panic   string   `json:"panic,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Synthesized counts clock and random reads that the journal could not
	// serve. It is only ever non-zero for the step that failed when the journal
	// was recorded: a fixed build may read more than the failing one did.
	Synthesized int `json:"synthesized_reads,omitempty"`
}

// Result is the outcome of replaying a journal. It contains no timings or other
// run-dependent values: replaying the same journal against the same handler
// produces an identical Result.
type Result struct {
	Status    Status       `json:"status"`
	Seq       *uint64      `json:"seq,omitempty"` // where Status applies, if anywhere
	Invariant string       `json:"invariant,omitempty"`
	Detail    string       `json:"detail,omitempty"`
	Service   string       `json:"service"`
	Start     string       `json:"start"`
	Records   int          `json:"records"`
	Recorded  *Failure     `json:"recorded_failure,omitempty"`
	Steps     []StepResult `json:"steps"`
}

// String formats the status as an agent-facing verdict, e.g. "diverged@12",
// "invariant_violated(balance_non_negative)@7" or "fixed".
func (r *Result) String() string {
	s := string(r.Status)
	if r.Invariant != "" {
		s += "(" + r.Invariant + ")"
	}
	if r.Seq != nil {
		s += fmt.Sprintf("@%d", *r.Seq)
	}
	return s
}

// Passed reports whether the status is ok or fixed.
func (r *Result) Passed() bool { return r.Status == StatusOK || r.Status == StatusFixed }

// ReplayFile replays the fixture at path against a fresh handler from newHandler.
func ReplayFile(path string, newHandler func() Handler) (*Result, error) {
	j, err := journal.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Replay(j, newHandler)
}

type step struct {
	input   journal.Record
	reads   []journal.Record // clock and rand, in order
	outputs []Output
	marker  *journal.Record // failure marker, if any
}

// Replay re-runs every step of j against a fresh handler from newHandler,
// serving clock and random reads from the journal and capturing outputs. It
// returns an error only if the journal cannot be replayed at all.
func Replay(j *journal.Journal, newHandler func() Handler) (*Result, error) {
	h := newHandler()
	res := &Result{Service: j.Header.Meta.Service, Start: j.Header.Meta.Start, Records: len(j.Records), Steps: []StepResult{}}

	recs := j.Records
	if j.Header.Meta.Start == journal.StartSnapshot {
		if len(recs) == 0 {
			return nil, errors.New("kavach: snapshot journal has no snapshot record")
		}
		s, ok := h.(Snapshotter)
		if !ok {
			return nil, errors.New("kavach: fixture starts from a snapshot but the handler does not implement Snapshotter")
		}
		if err := s.Restore(recs[0].Data); err != nil {
			return nil, fmt.Errorf("kavach: restoring snapshot: %w", err)
		}
		recs = recs[1:]
	}
	if len(recs) > 0 && recs[0].Type == journal.TypeSnapshot {
		return nil, errors.New("kavach: genesis journal contains a snapshot record")
	}

	steps, err := splitSteps(recs)
	if err != nil {
		return nil, err
	}
	for _, st := range steps {
		if st.marker != nil {
			res.Recorded = &Failure{Seq: st.input.Seq, Kind: st.marker.Kind, Message: st.marker.Message}
			break
		}
	}

	env := &replayEnv{}
	for _, st := range steps {
		seq := st.input.Seq
		env.reset(st, st.marker != nil)
		sr := StepResult{Seq: seq}
		pv, panicked, herr := runReplayStep(h, env, Input{Source: st.input.Source, Position: st.input.Position, Data: st.input.Data})
		sr.Outputs = append([]Output{}, env.outs...)
		sr.Synthesized = env.synthesized

		if nd, ok := pv.(nondeterminism); ok && panicked {
			res.Steps = append(res.Steps, sr)
			return res.set(StatusNondeterministic, nd.seq, string(nd.msg)), nil
		}
		if panicked {
			sr.Panic = fmt.Sprint(pv)
			res.Steps = append(res.Steps, sr)
			return res.set(StatusStillFailing, seq, "panic: "+sr.Panic), nil
		}
		if herr != nil {
			sr.Error = herr.Error()
			res.Steps = append(res.Steps, sr)
			return res.set(StatusStillFailing, seq, "error: "+sr.Error), nil
		}
		res.Steps = append(res.Steps, sr)
		if !env.lenient && env.pos < len(env.reads) {
			r := env.reads[env.pos]
			return res.set(StatusNondeterministic, r.Seq, fmt.Sprintf("step finished without reading the recorded %s at seq %d", r.Type, r.Seq)), nil
		}
		if name, ierr := checkInvariants(h); ierr != nil {
			res.Invariant = name
			return res.set(StatusInvariantViolated, seq, ierr.Error()), nil
		}
		if st.marker == nil {
			if d := diffOutputs(st.outputs, sr.Outputs); d != "" {
				return res.set(StatusDiverged, seq, d), nil
			}
		}
	}
	if res.Recorded != nil {
		res.Status = StatusFixed
	} else {
		res.Status = StatusOK
	}
	return res, nil
}

func (r *Result) set(s Status, seq uint64, detail string) *Result {
	r.Status, r.Seq, r.Detail = s, &seq, detail
	return r
}

func splitSteps(recs []journal.Record) ([]*step, error) {
	var steps []*step
	var cur *step
	for i := range recs {
		rec := recs[i]
		switch rec.Type {
		case journal.TypeInput:
			cur = &step{input: rec}
			steps = append(steps, cur)
		case journal.TypeClock, journal.TypeRand:
			if cur == nil {
				return nil, fmt.Errorf("kavach: %s record at seq %d precedes the first input", rec.Type, rec.Seq)
			}
			cur.reads = append(cur.reads, rec)
		case journal.TypeOutput:
			if cur == nil {
				return nil, fmt.Errorf("kavach: output record at seq %d precedes the first input", rec.Seq)
			}
			cur.outputs = append(cur.outputs, Output{Sink: rec.Sink, Data: rec.Data})
		case journal.TypeMarker:
			if cur != nil && cur.marker == nil && isFailure(rec.Kind) {
				cur.marker = &recs[i]
			}
		}
	}
	return steps, nil
}

func isFailure(kind string) bool {
	return kind == journal.MarkerPanic || kind == journal.MarkerError || kind == journal.MarkerInvariant
}

func diffOutputs(want, got []Output) string {
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			return fmt.Sprintf("output %d missing: recorded %s", i, describe(want[i]))
		case i >= len(want):
			return fmt.Sprintf("output %d unexpected: replay produced %s", i, describe(got[i]))
		case want[i].Sink != got[i].Sink || !bytes.Equal(want[i].Data, got[i].Data):
			return fmt.Sprintf("output %d differs: recorded %s, replay produced %s", i, describe(want[i]), describe(got[i]))
		}
	}
	return ""
}

func describe(o Output) string {
	const max = 120
	d := o.Data
	suffix := ""
	if len(d) > max {
		d, suffix = d[:max], "…"
	}
	return fmt.Sprintf("%s %q%s", o.Sink, d, suffix)
}

func runReplayStep(h Handler, env *replayEnv, in Input) (pv any, panicked bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			pv, panicked = v, true
		}
	}()
	return nil, false, h.Handle(env, in)
}

// nondeterminism is the panic value replayEnv uses to stop a handler whose
// reads do not match the journal.
type nondeterminism struct {
	seq uint64
	msg string
}

// replayEnv serves a step's recorded reads. Normally reads must match the
// journal exactly. For the step that failed when recorded, it is lenient: a
// fixed handler may go past the point where the old one failed, so reads are
// served by type in recorded order and, once those run out, synthesized
// deterministically (the last recorded time; hash-derived random bytes).
type replayEnv struct {
	input       uint64
	reads       []journal.Record
	pos         int
	outs        []Output
	lenient     bool
	clockPos    int
	randPos     int
	lastClock   int64
	synthesized int
}

func (e *replayEnv) reset(st *step, lenient bool) {
	e.input, e.reads, e.pos, e.outs = st.input.Seq, st.reads, 0, e.outs[:0]
	e.lenient, e.clockPos, e.randPos, e.synthesized = lenient, 0, 0, 0
}

func (e *replayEnv) next(want journal.Type) journal.Record {
	if e.pos >= len(e.reads) {
		panic(nondeterminism{e.input, fmt.Sprintf("handler read %s, but the step at seq %d recorded no further reads", want, e.input)})
	}
	r := e.reads[e.pos]
	if r.Type != want {
		panic(nondeterminism{r.Seq, fmt.Sprintf("handler read %s, journal has %s at seq %d", want, r.Type, r.Seq)})
	}
	e.pos++
	return r
}

// nextOfType returns the next recorded read of type want, scanning from *pos.
func (e *replayEnv) nextOfType(want journal.Type, pos *int) (journal.Record, bool) {
	for ; *pos < len(e.reads); *pos++ {
		if e.reads[*pos].Type == want {
			r := e.reads[*pos]
			*pos++
			return r, true
		}
	}
	return journal.Record{}, false
}

func (e *replayEnv) Now() time.Time {
	var n int64
	if !e.lenient {
		n = e.next(journal.TypeClock).UnixNanos
	} else if r, ok := e.nextOfType(journal.TypeClock, &e.clockPos); ok {
		n = r.UnixNanos
	} else {
		n = e.lastClock
		e.synthesized++
	}
	e.lastClock = n
	return time.Unix(0, n).UTC()
}

func (e *replayEnv) Read(p []byte) (int, error) {
	if e.lenient {
		if r, ok := e.nextOfType(journal.TypeRand, &e.randPos); ok && len(r.Data) == len(p) {
			return copy(p, r.Data), nil
		}
		e.synthesized++
		fillDeterministic(p, e.input, e.synthesized)
		return len(p), nil
	}
	r := e.next(journal.TypeRand)
	if len(r.Data) != len(p) {
		panic(nondeterminism{r.Seq, fmt.Sprintf("handler read %d random bytes, journal has %d at seq %d", len(p), len(r.Data), r.Seq)})
	}
	return copy(p, r.Data), nil
}

func fillDeterministic(p []byte, seq uint64, n int) {
	var block [8 + 8 + 8]byte
	binary.LittleEndian.PutUint64(block[0:], seq)
	binary.LittleEndian.PutUint64(block[8:], uint64(n))
	for i := 0; i < len(p); i += sha256.Size {
		binary.LittleEndian.PutUint64(block[16:], uint64(i))
		sum := sha256.Sum256(block[:])
		copy(p[i:], sum[:])
	}
}

func (e *replayEnv) Emit(sink string, data []byte) {
	e.outs = append(e.outs, Output{Sink: sink, Data: clone(data)})
}

// Divergence is the first point where two replays of the same journal differ.
type Divergence struct {
	Seq    uint64  `json:"seq"`    // seq of the step's input
	Output int     `json:"output"` // index of the differing output in the step, or -1 for a failure difference
	Old    *Output `json:"old,omitempty"`
	New    *Output `json:"new,omitempty"`
	Detail string  `json:"detail"`
}

// Compare returns the first output where two replay results differ, or nil if
// they produced identical outputs and failures at every step.
func Compare(old, new *Result) *Divergence {
	for i := 0; i < len(old.Steps) || i < len(new.Steps); i++ {
		if i >= len(old.Steps) {
			return &Divergence{Seq: new.Steps[i].Seq, Output: -1, Detail: "old build stopped before this step"}
		}
		if i >= len(new.Steps) {
			return &Divergence{Seq: old.Steps[i].Seq, Output: -1, Detail: "new build stopped before this step"}
		}
		o, n := old.Steps[i], new.Steps[i]
		for k := 0; k < len(o.Outputs) || k < len(n.Outputs); k++ {
			d := &Divergence{Seq: o.Seq, Output: k}
			if k < len(o.Outputs) {
				d.Old = &o.Outputs[k]
			}
			if k < len(n.Outputs) {
				d.New = &n.Outputs[k]
			}
			if d.Old == nil || d.New == nil || d.Old.Sink != d.New.Sink || !bytes.Equal(d.Old.Data, d.New.Data) {
				d.Detail = fmt.Sprintf("output %d of step %d differs", k, o.Seq)
				return d
			}
		}
		if failure(o) != failure(n) {
			return &Divergence{Seq: o.Seq, Output: -1, Detail: fmt.Sprintf("old: %s; new: %s", failure(o), failure(n))}
		}
	}
	return nil
}

func failure(s StepResult) string {
	switch {
	case s.Panic != "":
		return "panic: " + s.Panic
	case s.Error != "":
		return "error: " + s.Error
	}
	return "no failure"
}
