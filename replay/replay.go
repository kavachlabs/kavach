package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
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
	Seq     uint64          `json:"seq"` // seq of the step's input
	Outputs []kavach.Output `json:"outputs"`
	Panic   string          `json:"panic,omitempty"`
	Error   string          `json:"error,omitempty"`
	// Crash is set when the host process running the step exited during it
	// (SPEC.md §9.5); it holds what happened.
	Crash string `json:"crash,omitempty"`
	// Synthesized counts reads that the journal could not serve: clock and
	// random reads past the recorded ones, gateway queries answered by a
	// record with a different request or by none, config values that no
	// record of the step held. It is only ever non-zero for a lenient step
	// (SPEC.md §6): a fixed build may read more than the failing one did.
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
	Variant   string       `json:"variant,omitempty"` // the mutation, when replaying a variant journal
	Steps     []StepResult `json:"steps"`
	// Drift and EnvChanges explain a status; they never change it (SPEC.md §6.2).
	// Drift is set only when the replay ran in a host process that reported its
	// environment.
	Drift      []Drift     `json:"drift,omitempty"`
	EnvChanges []EnvChange `json:"environment_changes,omitempty"`
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

// RunFile replays the fixture at path against a fresh handler from newHandler.
func RunFile(path string, newHandler func() kavach.Handler) (*Result, error) {
	j, err := journal.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Run(j, newHandler)
}

type step struct {
	input    journal.Record
	reads    []journal.Record // clock, rand, gateway and config, in order
	outputs  []kavach.Output
	marker   *journal.Record  // failure marker, if any
	envAfter []journal.Record // environment records written after the step
}

// Replay re-runs every step of j against a fresh handler from newHandler,
// serving reads from the journal and capturing outputs. It returns an error
// only if the journal cannot be replayed at all.
func Run(j *journal.Journal, newHandler func() kavach.Handler) (*Result, error) {
	return replayJournal(j, func(snapshot []byte) (kavach.Handler, error) {
		h := newHandler()
		if snapshot == nil {
			return h, nil
		}
		s, ok := h.(kavach.Snapshotter)
		if !ok {
			return nil, errors.New("kavach: fixture starts from a snapshot but the handler does not implement Snapshotter")
		}
		if err := s.Restore(snapshot); err != nil {
			return nil, fmt.Errorf("kavach: restoring snapshot: %w", err)
		}
		return h, nil
	})
}

// replayJournal is the replay engine. open creates the handler the journal
// starts from; snapshot is nil unless the journal starts from one.
func replayJournal(j *journal.Journal, open func(snapshot []byte) (kavach.Handler, error)) (*Result, error) {
	res := &Result{Service: j.Header.Meta.Service, Start: j.Header.Meta.Start, Records: len(j.Records), Steps: []StepResult{}}
	// A variant never happened, so it has no recorded outputs to match and its
	// recorded reads are only a source of plausible values: every step is lenient.
	variant := j.Header.Meta.Variant != nil
	if variant {
		res.Variant = j.Header.Meta.Variant.Mutation
	}

	recs := j.Records
	var snapshot []byte
	if j.Header.Meta.Start == journal.StartSnapshot {
		if len(recs) == 0 {
			return nil, errors.New("kavach: snapshot journal has no snapshot record")
		}
		snapshot = append([]byte{}, recs[0].Data...)
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
	hist := newHistory()
	if g := genesisEnv(recs); g != nil {
		hist.addFacts(*g)
	}
	for _, st := range steps {
		for _, e := range st.envAfter {
			res.EnvChanges = append(res.EnvChanges, newEnvChange(st.input.Seq, e))
		}
	}

	h, err := open(snapshot)
	if err != nil {
		return nil, err
	}
	env := &replayEnv{hist: hist}
	for _, st := range steps {
		seq := st.input.Seq
		env.reset(st, variant || st.marker != nil)
		sr := StepResult{Seq: seq}
		pv, panicked, herr := runReplayStep(h, env, kavach.Input{Source: st.input.Source, Position: st.input.Position, Data: st.input.Data})
		sr.Outputs = append([]kavach.Output{}, env.outs...)
		sr.Synthesized = env.synthesized

		if nd, ok := pv.(nondeterminism); ok && panicked {
			res.Steps = append(res.Steps, sr)
			return res.set(StatusNondeterministic, nd.seq, string(nd.msg)), nil
		}
		if hf, ok := pv.(hostFailure); ok && panicked {
			return nil, hf.err
		}
		if hc, ok := pv.(hostCrash); ok && panicked {
			sr.Crash = string(hc)
			res.Steps = append(res.Steps, sr)
			return res.set(StatusStillFailing, seq, "crash: "+sr.Crash), nil
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
		if name, ierr := kavach.CheckInvariants(h); ierr != nil {
			res.Invariant = name
			return res.set(StatusInvariantViolated, seq, ierr.Error()), nil
		}
		if st.marker == nil && !variant {
			if d := diffOutputs(st.outputs, sr.Outputs); d != "" {
				return res.set(StatusDiverged, seq, d), nil
			}
		}
		hist.addStep(st)
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
		case journal.TypeClock, journal.TypeRand, journal.TypeGateway, journal.TypeConfig:
			if cur == nil {
				return nil, fmt.Errorf("kavach: %s record at seq %d precedes the first input", rec.Type, rec.Seq)
			}
			cur.reads = append(cur.reads, rec)
		case journal.TypeOutput:
			if cur == nil {
				return nil, fmt.Errorf("kavach: output record at seq %d precedes the first input", rec.Seq)
			}
			cur.outputs = append(cur.outputs, kavach.Output{Sink: rec.Sink, Data: rec.Data})
		case journal.TypeEnvironment:
			if cur != nil {
				cur.envAfter = append(cur.envAfter, rec)
			}
		case journal.TypeMarker:
			if cur != nil && cur.marker == nil && isFailure(rec.Kind) {
				cur.marker = &recs[i]
			}
		}
	}
	return steps, nil
}

// genesisEnv returns the environment record before the first input, if any.
func genesisEnv(recs []journal.Record) *journal.Record {
	for i := range recs {
		switch recs[i].Type {
		case journal.TypeEnvironment:
			return &recs[i]
		case journal.TypeInput:
			return nil
		}
	}
	return nil
}

func isFailure(kind string) bool {
	return kind == journal.MarkerPanic || kind == journal.MarkerError || kind == journal.MarkerInvariant || kind == journal.MarkerCrash
}

func diffOutputs(want, got []kavach.Output) string {
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

func describe(o kavach.Output) string {
	const max = 120
	d := o.Data
	suffix := ""
	if len(d) > max {
		d, suffix = d[:max], "…"
	}
	return fmt.Sprintf("%s %q%s", o.Sink, d, suffix)
}

func runReplayStep(h kavach.Handler, env *replayEnv, in kavach.Input) (pv any, panicked bool, err error) {
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

// hostFailure and hostCrash are the panic values a host proxy uses to end a
// step: the first makes the replay fail to run, the second is a step failure
// of the host process itself (SPEC.md §9.5).
type hostFailure struct{ err error }

type hostCrash string

// history is what earlier parts of a journal said about config values: the
// last config record of each key, and the latest value of each environment
// fact. Lenient steps fall back on it (SPEC.md §6).
type history struct {
	config map[string]journal.Record
	facts  map[string]factAt
}

type factAt struct {
	fact journal.Fact
	seq  uint64
}

func newHistory() *history {
	return &history{config: map[string]journal.Record{}, facts: map[string]factAt{}}
}

func (h *history) addFacts(env journal.Record) {
	for _, f := range env.Facts {
		h.facts[f.Key] = factAt{f, env.Seq}
	}
}

func (h *history) addStep(st *step) {
	for _, r := range st.reads {
		if r.Type == journal.TypeConfig {
			h.config[r.Key] = r
		}
	}
	for _, e := range st.envAfter {
		h.addFacts(e)
	}
}

// lookup returns the value last recorded for key, whichever of a config record
// and an environment fact came later. A key is also looked up as a flag. or
// env. fact. SPEC: a change record holds only the facts that changed, so the
// "latest environment record" is read as the latest record that holds the fact.
func (h *history) lookup(key string) ([]byte, bool) {
	best, bestSeq, found := journal.Record{}, uint64(0), false
	if r, ok := h.config[key]; ok {
		best, bestSeq, found = r, r.Seq, true
	}
	for _, k := range []string{key, "flag." + key, "env." + key} {
		if f, ok := h.facts[k]; ok && (!found || f.seq > bestSeq) {
			best = journal.Record{Present: f.fact.Form == journal.FactValue, Value: f.fact.Value}
			bestSeq, found = f.seq, true
		}
	}
	if !found || !best.Present {
		return nil, false
	}
	return clone(best.Value), true
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
	outs        []kavach.Output
	lenient     bool
	clockPos    int
	randPos     int
	lastClock   int64
	synthesized int
	used        []bool // lenient gateway and config reads already served
	hist        *history
}

func (e *replayEnv) reset(st *step, lenient bool) {
	e.input, e.reads, e.pos, e.outs = st.input.Seq, st.reads, 0, e.outs[:0]
	e.lenient, e.clockPos, e.randPos, e.synthesized, e.used = lenient, 0, 0, 0, nil
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

// takeUnread marks and returns the first unserved read for which match is true.
func (e *replayEnv) takeUnread(match func(journal.Record) bool) (journal.Record, bool) {
	if e.used == nil {
		e.used = make([]bool, len(e.reads))
	}
	for i, r := range e.reads {
		if !e.used[i] && match(r) {
			e.used[i] = true
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

func (e *replayEnv) Query(gateway string, request []byte) ([]byte, error) {
	var rec journal.Record
	if !e.lenient {
		rec = e.next(journal.TypeGateway)
		if rec.Gateway != gateway || !bytes.Equal(rec.Request, request) {
			panic(nondeterminism{rec.Seq, fmt.Sprintf("handler queried %s with %q, journal has a query of %s with %q at seq %d", gateway, request, rec.Gateway, rec.Request, rec.Seq)})
		}
	} else {
		var ok bool
		if rec, ok = e.takeUnread(func(r journal.Record) bool {
			return r.Type == journal.TypeGateway && r.Gateway == gateway && bytes.Equal(r.Request, request)
		}); !ok {
			e.synthesized++
			if rec, ok = e.takeUnread(func(r journal.Record) bool { return r.Type == journal.TypeGateway && r.Gateway == gateway }); !ok {
				return nil, errors.New("kavach: no recorded response")
			}
		}
	}
	if rec.Error != "" {
		return nil, errors.New(rec.Error)
	}
	return clone(rec.Response), nil
}

func (e *replayEnv) Config(key string) ([]byte, bool) {
	var rec journal.Record
	if !e.lenient {
		rec = e.next(journal.TypeConfig)
		if rec.Key != key {
			panic(nondeterminism{rec.Seq, fmt.Sprintf("handler read config %q, journal has %q at seq %d", key, rec.Key, rec.Seq)})
		}
	} else {
		var ok bool
		if rec, ok = e.takeUnread(func(r journal.Record) bool { return r.Type == journal.TypeConfig && r.Key == key }); !ok {
			e.synthesized++
			return e.hist.lookup(key)
		}
	}
	if !rec.Present {
		return nil, false
	}
	return clone(rec.Value), true
}

func (e *replayEnv) Emit(sink string, data []byte) {
	e.outs = append(e.outs, kavach.Output{Sink: sink, Data: clone(data)})
}

// Divergence is the first point where two replays of the same journal differ.
type Divergence struct {
	Seq    uint64         `json:"seq"`    // seq of the step's input
	Output int            `json:"output"` // index of the differing output in the step, or -1 for a failure difference
	Old    *kavach.Output `json:"old,omitempty"`
	New    *kavach.Output `json:"new,omitempty"`
	Detail string         `json:"detail"`
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
	case s.Crash != "":
		return "crash: " + s.Crash
	case s.Panic != "":
		return "panic: " + s.Panic
	case s.Error != "":
		return "error: " + s.Error
	}
	return "no failure"
}

func clone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}
