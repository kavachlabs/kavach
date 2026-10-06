package kavach_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/kavachtest"
)

// wallet is a test handler. Inputs are "account:amount"; an amount of "null"
// crashes the buggy version.
type wallet struct {
	bal        map[string]int64
	fixed      bool // reject "null" instead of crashing
	newFormat  bool // change the output format of every step
	extraClock bool // read the clock twice per step
	failOn     string
}

func newWallet() kavach.Handler { return &wallet{bal: map[string]int64{}} }
func fixedWallet() kavach.Handler {
	return &wallet{bal: map[string]int64{}, fixed: true}
}

func (w *wallet) Handle(env kavach.Env, in kavach.Input) error {
	at := env.Now()
	if w.extraClock {
		env.Now()
	}
	acct, amt, _ := strings.Cut(string(in.Data), ":")
	if w.failOn != "" && acct == w.failOn {
		return fmt.Errorf("account %s is frozen", acct)
	}
	var amount *int64
	if amt != "null" {
		v, err := strconv.ParseInt(amt, 10, 64)
		if err != nil {
			return err
		}
		amount = &v
	}
	if amount == nil && w.fixed {
		var id [8]byte
		env.Read(id[:])
		env.Emit("rejections", []byte(acct+" null amount "+hex.EncodeToString(id[:])))
		return nil
	}
	w.bal[acct] += *amount // nil dereference on a null amount
	var id [8]byte
	env.Read(id[:])
	format := "%s=%d at %d id %x"
	if w.newFormat {
		format = "%s balance %d at %d id %x"
	}
	env.Emit("entries", []byte(fmt.Sprintf(format, acct, w.bal[acct], at.UnixNano(), id)))
	return nil
}

func (w *wallet) Snapshot() ([]byte, error) { return json.Marshal(w.bal) }
func (w *wallet) Restore(b []byte) error    { return json.Unmarshal(b, &w.bal) }

func (w *wallet) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "balance_non_negative", Check: func() error {
		for a, b := range w.bal {
			if b < 0 {
				return fmt.Errorf("%s has balance %d", a, b)
			}
		}
		return nil
	}}}
}

// fakeClock and seeded randomness make recordings reproducible in tests.
func testOptions(t *testing.T) kavach.Options {
	n := int64(1_700_000_000_000_000_000)
	return kavach.Options{
		Service: "wallet",
		Dir:     t.TempDir(),
		Clock: func() time.Time {
			n += 1_000_000
			return time.Unix(0, n)
		},
		Rand:          rand.New(rand.NewSource(1)),
		RecoverPanics: true,
	}
}

func feed(t *testing.T, r *kavach.Recorder, inputs ...string) error {
	t.Helper()
	for i, in := range inputs {
		if err := r.Step(kavach.Input{Source: "test", Position: strconv.Itoa(i), Data: []byte(in)}); err != nil {
			return err
		}
	}
	return nil
}

func recordCrash(t *testing.T) string {
	t.Helper()
	r := kavach.NewRecorder(newWallet(), testOptions(t))
	err := feed(t, r, "alice:10", "bob:5", "alice:7", "bob:null")
	var pe *kavach.PanicError
	if !errors.As(err, &pe) || pe.Fixture == "" {
		t.Fatalf("expected a panic with a fixture, got %v", err)
	}
	return pe.Fixture
}

func TestCrashIsReproducedThenFixed(t *testing.T) {
	path := recordCrash(t)

	j, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Meta.Start != journal.StartGenesis || j.Header.Meta.Service != "wallet" {
		t.Fatalf("meta = %+v", j.Header.Meta)
	}
	last := j.Records[len(j.Records)-1]
	if last.Type != journal.TypeMarker || last.Kind != journal.MarkerPanic || !strings.Contains(last.Message, "nil pointer") {
		t.Fatalf("last record = %+v", last)
	}

	res, err := kavach.ReplayFile(path, newWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusStillFailing || res.Recorded == nil || *res.Seq != res.Recorded.Seq {
		t.Fatalf("buggy build: %s (%s)", res, res.Detail)
	}

	res, err = kavach.ReplayFile(path, fixedWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusFixed || res.String() != "fixed" || !res.Passed() {
		t.Fatalf("fixed build: %s (%s)", res, res.Detail)
	}
	lastStep := res.Steps[len(res.Steps)-1]
	if lastStep.Synthesized != 1 || len(lastStep.Outputs) != 1 || lastStep.Outputs[0].Sink != "rejections" {
		t.Fatalf("failing step under fix = %+v", lastStep)
	}
}

func TestDivergedBeforeFailure(t *testing.T) {
	path := recordCrash(t)
	res, err := kavach.ReplayFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, fixed: true, newFormat: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "diverged@0" || !strings.Contains(res.Detail, "output 0 differs") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}
}

func TestNondeterminism(t *testing.T) {
	path := recordCrash(t)
	res, err := kavach.ReplayFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, extraClock: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusNondeterministic || !strings.Contains(res.Detail, "handler read clock, journal has rand") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}

	// A handler that reads less than recorded is also caught.
	res, err = kavach.ReplayFile(path, func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error { return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusNondeterministic || !strings.Contains(res.Detail, "without reading") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}

	// Reading the wrong number of random bytes is caught.
	res, _ = kavach.ReplayFile(path, func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			env.Now()
			env.Read(make([]byte, 3))
			return nil
		})
	})
	if res.Status != kavach.StatusNondeterministic || !strings.Contains(res.Detail, "3 random bytes") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}
}

func TestInvariantViolation(t *testing.T) {
	r := kavach.NewRecorder(newWallet(), testOptions(t))
	err := feed(t, r, "alice:10", "alice:-30")
	var ie *kavach.InvariantError
	if !errors.As(err, &ie) || ie.Name != "balance_non_negative" || ie.Fixture == "" {
		t.Fatalf("got %v", err)
	}
	res, err := kavach.ReplayFile(ie.Fixture, newWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "invariant_violated(balance_non_negative)@4" || res.Recorded.Kind != journal.MarkerInvariant {
		t.Fatalf("got %s (%+v)", res, res.Recorded)
	}
}

func TestHandlerErrorWritesFixture(t *testing.T) {
	var flushed []string
	opts := testOptions(t)
	opts.OnFlush = func(p string, err error) { flushed = append(flushed, p) }
	h := &wallet{bal: map[string]int64{}, failOn: "carol"}
	r := kavach.NewRecorder(h, opts)
	if err := feed(t, r, "alice:1", "carol:1"); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("got %v", err)
	}
	if len(flushed) != 1 || !strings.HasSuffix(flushed[0], "-error.kavach") {
		t.Fatalf("flushed = %v", flushed)
	}
	res, _ := kavach.ReplayFile(flushed[0], func() kavach.Handler { return &wallet{bal: map[string]int64{}, failOn: "carol"} })
	if res.Status != kavach.StatusStillFailing || !strings.Contains(res.Detail, "error: account carol is frozen") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}

	opts.NoFlushOnError = true
	flushed = nil
	r = kavach.NewRecorder(&wallet{bal: map[string]int64{}, failOn: "carol"}, opts)
	feed(t, r, "carol:1")
	if len(flushed) != 0 {
		t.Fatalf("flushed with NoFlushOnError: %v", flushed)
	}
}

func TestPanicPropagatesByDefault(t *testing.T) {
	opts := testOptions(t)
	opts.RecoverPanics = false
	var fixture string
	opts.OnFlush = func(p string, err error) { fixture = p }
	r := kavach.NewRecorder(newWallet(), opts)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected re-panic")
			}
		}()
		feed(t, r, "a:null")
	}()
	if fixture == "" {
		t.Fatal("no fixture written before re-panic")
	}
}

func TestSnapshotWindow(t *testing.T) {
	opts := testOptions(t)
	opts.SnapshotEvery = 3
	r := kavach.NewRecorder(newWallet(), opts)
	inputs := []string{"a:1", "a:2", "b:3", "a:4", "b:5", "a:6", "b:7", "a:null"}
	var pe *kavach.PanicError
	if err := feed(t, r, inputs...); !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Meta.Start != journal.StartSnapshot || j.Records[0].Type != journal.TypeSnapshot {
		t.Fatalf("fixture does not start from a snapshot: %+v", j.Header.Meta)
	}
	res, err := kavach.Replay(j, fixedWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != kavach.StatusFixed || len(res.Steps) != 2 {
		t.Fatalf("got %s with %d steps (%s)", res, len(res.Steps), res.Detail)
	}

	// A handler that cannot restore cannot replay a snapshot fixture.
	if _, err := kavach.Replay(j, func() kavach.Handler { return kavach.HandlerFunc(nil) }); err == nil {
		t.Fatal("expected error for non-Snapshotter")
	}
}

func TestWindowRestartsAfterFailure(t *testing.T) {
	r := kavach.NewRecorder(newWallet(), testOptions(t))
	if err := feed(t, r, "a:5", "a:null"); err == nil {
		t.Fatal("expected panic error")
	}
	feed(t, r, "a:1", "b:2")
	path, err := r.Flush("later")
	if err != nil {
		t.Fatal(err)
	}
	res, err := kavach.ReplayFile(path, newWallet)
	if err != nil || res.Status != kavach.StatusOK || len(res.Steps) != 2 {
		t.Fatalf("got %v (%v), err %v", res, res.Detail, err)
	}
}

func TestHistoryLost(t *testing.T) {
	opts := testOptions(t)
	opts.MaxRecords = 5
	h := kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		env.Emit("out", in.Data)
		return nil
	})
	r := kavach.NewRecorder(h, opts)
	feed(t, r, "1", "2", "3")
	if _, err := r.Flush("manual"); !errors.Is(err, kavach.ErrHistoryLost) {
		t.Fatalf("err = %v", err)
	}
}

func TestManualFlushReplaysOK(t *testing.T) {
	var delivered []kavach.Output
	opts := testOptions(t)
	opts.Deliver = func(outs []kavach.Output) error { delivered = append(delivered, outs...); return nil }
	r := kavach.NewRecorder(newWallet(), opts)
	feed(t, r, "a:1", "b:2")
	path, err := r.Flush("operator request")
	if err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 {
		t.Fatalf("delivered %d outputs", len(delivered))
	}
	res, err := kavach.ReplayFile(path, newWallet)
	if err != nil || res.Status != kavach.StatusOK || res.Recorded != nil {
		t.Fatalf("got %v, %v", res, err)
	}
}

func TestDeliverOnlyOnSuccess(t *testing.T) {
	var delivered int
	opts := testOptions(t)
	opts.Deliver = func(outs []kavach.Output) error { delivered += len(outs); return nil }
	r := kavach.NewRecorder(kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		env.Emit("out", in.Data)
		if string(in.Data) == "bad" {
			return errors.New("bad input")
		}
		return nil
	}), opts)
	feed(t, r, "ok")
	feed(t, r, "bad")
	if delivered != 1 {
		t.Fatalf("delivered %d outputs, want 1", delivered)
	}

	opts.Deliver = func([]kavach.Output) error { return errors.New("sink down") }
	r = kavach.NewRecorder(kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		env.Emit("out", in.Data)
		return nil
	}), opts)
	if err := feed(t, r, "x"); err == nil || err.Error() != "sink down" {
		t.Fatalf("err = %v", err)
	}
}

// TestReplayIsDeterministic replays the same fixture 1,000 times and requires
// byte-identical results (BENCHMARKS.md: determinism).
func TestReplayIsDeterministic(t *testing.T) {
	path := recordCrash(t)
	j, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 1000; i++ {
		res, err := kavach.Replay(j, fixedWallet)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res)
		if first == nil {
			first = b
		} else if !bytes.Equal(b, first) {
			t.Fatalf("replay %d differs:\n%s\n%s", i, first, b)
		}
	}
}

func TestCompare(t *testing.T) {
	path := recordCrash(t)
	old, _ := kavach.ReplayFile(path, newWallet)
	fix, _ := kavach.ReplayFile(path, fixedWallet)
	if d := kavach.Compare(old, old); d != nil {
		t.Fatalf("self-compare diverged: %+v", d)
	}
	d := kavach.Compare(old, fix)
	if d == nil || d.Seq != old.Recorded.Seq || d.Old != nil || d.New == nil || d.New.Sink != "rejections" {
		t.Fatalf("divergence = %+v", d)
	}
	reformatted, _ := kavach.ReplayFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, fixed: true, newFormat: true}
	})
	if d := kavach.Compare(fix, reformatted); d == nil || d.Seq != 0 || d.Output != 0 {
		t.Fatalf("divergence = %+v", d)
	}
}

func TestCompareFailureOnly(t *testing.T) {
	a := &kavach.Result{Steps: []kavach.StepResult{{Seq: 0, Panic: "x"}}}
	b := &kavach.Result{Steps: []kavach.StepResult{{Seq: 0, Error: "y"}, {Seq: 3}}}
	if d := kavach.Compare(a, b); d == nil || d.Output != -1 || !strings.Contains(d.Detail, "old: panic: x; new: error: y") {
		t.Fatalf("divergence = %+v", d)
	}
	if d := kavach.Compare(&kavach.Result{Steps: a.Steps[:0]}, b); d == nil || !strings.Contains(d.Detail, "old build stopped") {
		t.Fatalf("divergence = %+v", d)
	}
}

func TestLenientRandSynthesisIsDeterministic(t *testing.T) {
	path := recordCrash(t)
	read := func() []byte {
		var got []byte
		kavach.ReplayFile(path, func() kavach.Handler {
			w := &wallet{bal: map[string]int64{}}
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				if strings.HasSuffix(string(in.Data), "null") {
					env.Now()
					b := make([]byte, 40)
					env.Read(b)
					env.Now() // beyond what was recorded
					got = b
					return nil
				}
				return w.Handle(env, in)
			})
		})
		return got
	}
	a, b := read(), read()
	if len(a) != 40 || !bytes.Equal(a, b) || binary.LittleEndian.Uint64(a) == 0 {
		t.Fatalf("synthesized bytes not deterministic: %x vs %x", a, b)
	}
}

func TestKavachtest(t *testing.T) {
	path := recordCrash(t)
	kavachtest.Run(t, filepath.Join(filepath.Dir(path), "*.kavach"), fixedWallet)
}

func BenchmarkRecorderStep(b *testing.B) {
	opts := kavach.Options{Service: "bench", Dir: b.TempDir(), Rand: rand.New(rand.NewSource(1))}
	r := kavach.NewRecorder(benchHandler{}, opts)
	in := kavach.Input{Source: "bench", Position: "0", Data: []byte(`{"account":"alice","amount":10}`)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Step(in); err != nil {
			b.Fatal(err)
		}
	}
	// Each step journals 4 events: input, clock, rand, output.
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*4), "ns/event")
}

// benchHandler snapshots so the recorder's window stays bounded, as in production.
type benchHandler struct{}

func (benchHandler) Handle(env kavach.Env, in kavach.Input) error {
	env.Now()
	var id [8]byte
	env.Read(id[:])
	env.Emit("entries", in.Data)
	return nil
}
func (benchHandler) Snapshot() ([]byte, error) { return []byte("{}"), nil }
func (benchHandler) Restore([]byte) error      { return nil }
