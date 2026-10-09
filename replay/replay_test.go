package replay_test

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

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
	"github.com/kavachlabs/kavach/sdk/go/kavachtest"
)

// wallet is a test handler. Inputs are "account:amount"; an amount of "null"
// crashes the buggy version.
type wallet struct {
	bal        map[string]int64
	fixed      bool // reject "null" instead of crashing
	newFormat  bool // change the output format of every step
	extraClock bool // read the clock twice per step
	lateFormat bool // change the output format after 2023
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
	if w.newFormat || w.lateFormat && at.Year() > 2023 {
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

// newRecorder starts a recorder that is closed when the test ends.
func newRecorder(t *testing.T, h kavach.Handler, opts kavach.Options) *kavach.Recorder {
	t.Helper()
	r := kavach.NewRecorder(h, opts)
	t.Cleanup(r.Close)
	return r
}

// fixtureIn waits until everything recorded is durable and returns the one
// fixture the recorder wrote under dir.
func fixtureIn(t *testing.T, r *kavach.Recorder, dir string) string {
	t.Helper()
	if err := r.Flush(true); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "fixtures", "*.kavach"))
	if len(m) != 1 {
		t.Fatalf("want one fixture under %s, got %v", dir, m)
	}
	return m[0]
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
	opts := testOptions(t)
	r := newRecorder(t, newWallet(), opts)
	err := feed(t, r, "alice:10", "bob:5", "alice:7", "bob:null")
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	return fixtureIn(t, r, opts.Dir)
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

	res, err := replay.RunFile(path, newWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusStillFailing || res.Recorded == nil || *res.Seq != res.Recorded.Seq {
		t.Fatalf("buggy build: %s (%s)", res, res.Detail)
	}

	res, err = replay.RunFile(path, fixedWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusFixed || res.String() != "fixed" || !res.Passed() {
		t.Fatalf("fixed build: %s (%s)", res, res.Detail)
	}
	lastStep := res.Steps[len(res.Steps)-1]
	if lastStep.Synthesized != 1 || len(lastStep.Outputs) != 1 || lastStep.Outputs[0].Sink != "rejections" {
		t.Fatalf("failing step under fix = %+v", lastStep)
	}
}

func TestDivergedBeforeFailure(t *testing.T) {
	path := recordCrash(t)
	res, err := replay.RunFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, fixed: true, newFormat: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "diverged@1" || !strings.Contains(res.Detail, "output 0 differs") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}
}

func TestNondeterminism(t *testing.T) {
	path := recordCrash(t)
	res, err := replay.RunFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, extraClock: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "handler read clock, journal has rand") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}

	// A handler that reads less than recorded is also caught.
	res, err = replay.RunFile(path, func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error { return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "without reading") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}

	// Reading the wrong number of random bytes is caught.
	res, _ = replay.RunFile(path, func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			env.Now()
			env.Read(make([]byte, 3))
			return nil
		})
	})
	if res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "3 random bytes") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}
}

func TestInvariantViolation(t *testing.T) {
	opts := testOptions(t)
	r := newRecorder(t, newWallet(), opts)
	err := feed(t, r, "alice:10", "alice:-30")
	var ie *kavach.InvariantError
	if !errors.As(err, &ie) || ie.Name != "balance_non_negative" {
		t.Fatalf("got %v", err)
	}
	res, err := replay.RunFile(fixtureIn(t, r, opts.Dir), newWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "invariant_violated(balance_non_negative)@5" || res.Recorded.Kind != journal.MarkerInvariant {
		t.Fatalf("got %s (%+v)", res, res.Recorded)
	}
}

func TestHandlerErrorWritesFixture(t *testing.T) {
	opts := testOptions(t)
	reported := make(chan string, 1)
	opts.OnFixture = func(file, failure string) { reported <- file + " " + failure }
	r := newRecorder(t, &wallet{bal: map[string]int64{}, failOn: "carol"}, opts)
	if err := feed(t, r, "alice:1", "carol:1"); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("got %v", err)
	}
	path := fixtureIn(t, r, opts.Dir)
	if got := <-reported; got != path+" error: account carol is frozen" {
		t.Fatalf("OnFixture got %q", got)
	}
	res, _ := replay.RunFile(path, func() kavach.Handler { return &wallet{bal: map[string]int64{}, failOn: "carol"} })
	if res.Status != replay.StatusStillFailing || !strings.Contains(res.Detail, "error: account carol is frozen") {
		t.Fatalf("got %s (%s)", res, res.Detail)
	}
}

func TestPanicPropagatesByDefault(t *testing.T) {
	opts := testOptions(t)
	opts.RecoverPanics = false
	r := newRecorder(t, newWallet(), opts)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected re-panic")
			}
		}()
		feed(t, r, "a:null")
	}()
	// The step made the failure durable before it panicked.
	if m, _ := filepath.Glob(filepath.Join(opts.Dir, "fixtures", "*.kavach")); len(m) != 1 {
		t.Fatalf("fixtures: %v", m)
	}
}

// TestSegmentSnapshot starts a segment for every step, so the fixture of the
// crash begins from a snapshot the SDK took at a step boundary.
func TestSegmentSnapshot(t *testing.T) {
	opts := testOptions(t)
	opts.SegmentBytes = 1
	r := newRecorder(t, newWallet(), opts)
	for i, in := range []string{"a:1", "a:2", "b:3", "a:4"} {
		if err := feed(t, r, in); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		// The snapshot_request of this step is read before the next one.
		if err := r.Flush(true); err != nil {
			t.Fatal(err)
		}
	}
	var pe *kavach.PanicError
	if err := feed(t, r, "a:null"); !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Meta.Start != journal.StartSnapshot || j.Records[0].Type != journal.TypeSnapshot {
		t.Fatalf("fixture does not start from a snapshot: %+v", j.Header.Meta)
	}
	res, err := replay.Run(j, fixedWallet)
	if err != nil || res.Status != replay.StatusFixed {
		t.Fatalf("got %v (%v)", res, err)
	}

	// A handler that cannot restore cannot replay a snapshot fixture.
	if _, err := replay.Run(j, func() kavach.Handler { return kavach.HandlerFunc(nil) }); err == nil {
		t.Fatal("expected error for non-Snapshotter")
	}
}

func TestSegmentReplaysOK(t *testing.T) {
	var delivered []kavach.Output
	opts := testOptions(t)
	opts.Deliver = func(outs []kavach.Output) error { delivered = append(delivered, outs...); return nil }
	r := newRecorder(t, newWallet(), opts)
	feed(t, r, "a:1", "b:2")
	r.Close()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d outputs", len(delivered))
	}
	res, err := replay.RunFile(r.File(), newWallet)
	if err != nil || res.Status != replay.StatusOK || res.Recorded != nil || len(res.Steps) != 2 {
		t.Fatalf("got %v, %v", res, err)
	}
}

func TestDeliverOnlyOnSuccess(t *testing.T) {
	var delivered int
	opts := testOptions(t)
	opts.Deliver = func(outs []kavach.Output) error { delivered += len(outs); return nil }
	r := newRecorder(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
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
	r = newRecorder(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
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
		res, err := replay.Run(j, fixedWallet)
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
	old, _ := replay.RunFile(path, newWallet)
	fix, _ := replay.RunFile(path, fixedWallet)
	if d := replay.Compare(old, old); d != nil {
		t.Fatalf("self-compare diverged: %+v", d)
	}
	d := replay.Compare(old, fix)
	if d == nil || d.Seq != old.Recorded.Seq || d.Old != nil || d.New == nil || d.New.Sink != "rejections" {
		t.Fatalf("divergence = %+v", d)
	}
	reformatted, _ := replay.RunFile(path, func() kavach.Handler {
		return &wallet{bal: map[string]int64{}, fixed: true, newFormat: true}
	})
	if d := replay.Compare(fix, reformatted); d == nil || d.Seq != 1 || d.Output != 0 {
		t.Fatalf("divergence = %+v", d)
	}
}

func TestCompareFailureOnly(t *testing.T) {
	a := &replay.Result{Steps: []replay.StepResult{{Seq: 0, Panic: "x"}}}
	b := &replay.Result{Steps: []replay.StepResult{{Seq: 0, Error: "y"}, {Seq: 3}}}
	if d := replay.Compare(a, b); d == nil || d.Output != -1 || !strings.Contains(d.Detail, "old: panic: x; new: error: y") {
		t.Fatalf("divergence = %+v", d)
	}
	if d := replay.Compare(&replay.Result{Steps: a.Steps[:0]}, b); d == nil || !strings.Contains(d.Detail, "old build stopped") {
		t.Fatalf("divergence = %+v", d)
	}
}

func TestLenientRandSynthesisIsDeterministic(t *testing.T) {
	path := recordCrash(t)
	read := func() []byte {
		var got []byte
		replay.RunFile(path, func() kavach.Handler {
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
	defer r.Close()
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

// benchHandler snapshots, as a production handler would.
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
