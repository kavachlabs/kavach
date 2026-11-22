package replay_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func crashJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.ReadFile(recordCrash(t))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

type wrapped struct {
	*wallet
	pre func(w *wallet, env kavach.Env, in kavach.Input) (handled bool, err error)
}

func (h wrapped) Handle(env kavach.Env, in kavach.Input) error {
	if ok, err := h.pre(h.wallet, env, in); ok {
		return err
	}
	return h.wallet.Handle(env, in)
}

func wrap(pre func(w *wallet, env kavach.Env, in kavach.Input) (bool, error)) func() kavach.Handler {
	return func() kavach.Handler { return wrapped{newWallet().(*wallet), pre} }
}

func verify(t *testing.T, j *journal.Journal, newHandler func() kavach.Handler, opts replay.VerifyOptions) *replay.Verification {
	t.Helper()
	v, err := replay.Verify(j, newWallet, newHandler, opts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVariantsOfPlainInputs(t *testing.T) {
	j := crashJournal(t)
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	// The inputs are not JSON, so only structural mutations apply: 3 moves,
	// 3 drops plus dropping all history, 6 redeliveries (3 immediate, 2
	// delayed, 1 failing-step), and 8 clock shifts (4 uniform, 4 relative).
	if len(vs) != 21 {
		t.Fatalf("got %d variants", len(vs))
	}
	var muts []string
	for i, v := range vs {
		info := v.Header.Meta.Variant
		if info == nil || info.ID != i+1 || info.Failure != "panic: runtime error: invalid memory address or nil pointer dereference" {
			t.Fatalf("variant %d meta = %+v", i, info)
		}
		if in := recordAt(t, v, info.Incident); in.Type != journal.TypeInput || string(in.Data) != "bob:null" {
			t.Fatalf("variant %d: incident seq %d is %+v", info.ID, info.Incident, in)
		}
		for _, r := range v.Records {
			if r.Type == journal.TypeOutput || r.Type == journal.TypeMarker {
				t.Fatalf("variant %d carries a %s record", info.ID, r.Type)
			}
		}
		muts = append(muts, info.Mutation)
	}
	for _, want := range []string{
		"failing input (seq 13) moved 1 input earlier, before seq 9",
		"failing input (seq 13) moved 3 inputs earlier, before seq 1",
		"input seq 5 dropped",
		"all 3 inputs before the failing one dropped",
		"input seq 1 delivered twice",
		"clock shifted by -5h30m",
	} {
		if !contains(muts, want) {
			t.Errorf("no variant %q in %q", want, muts)
		}
	}

	again, _ := replay.Variants(j)
	a, _ := json.Marshal(vs)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatal("variant generation is not deterministic")
	}
}

func recordAt(t *testing.T, j *journal.Journal, seq uint64) journal.Record {
	t.Helper()
	for _, r := range j.Records {
		if r.Seq == seq {
			return r
		}
	}
	t.Fatalf("no record with seq %d", seq)
	return journal.Record{}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestVariantsOfJSONInput(t *testing.T) {
	opts := testOptions(t)
	r := newRecorder(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		var ev struct{ Amount *float64 }
		if err := json.Unmarshal(in.Data, &ev); err != nil {
			return err
		}
		env.Emit("out", []byte(strings.Repeat("x", int(*ev.Amount))))
		return nil
	}), opts)
	err := feed(t, r, `{"id":"a","n":1,"ok":true,"amount":2.5}`, `{"id":"b","n":2,"amount":1,"tag":"t"}`, `{"id":"c", "n":3, "ok":false, "amount":null}`)
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	kind := map[string]string{}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		got[info.Mutation] = string(recordAt(t, v, info.Incident).Data)
		kind[info.Mutation] = info.Kind
	}
	for mut, k := range map[string]string{
		`failing input (seq 5): "amount" removed`:                       replay.KindField,
		`failing input (seq 5): "ok" false → true, "amount" null → 2.5`: replay.KindPair,
	} {
		if kind[mut] != k {
			t.Errorf("variant %q: kind %q, want %q", mut, kind[mut], k)
		}
	}
	for mut, k := range kind {
		switch {
		case strings.Contains(mut, "earlier, before"):
			if k != replay.KindMove {
				t.Errorf("variant %q: kind %q, want %q", mut, k, replay.KindMove)
			}
		case strings.Contains(mut, "delivered twice"):
			if k != replay.KindDuplicate {
				t.Errorf("variant %q: kind %q, want %q", mut, k, replay.KindDuplicate)
			}
		case strings.Contains(mut, "dropped"):
			if k != replay.KindDrop {
				t.Errorf("variant %q: kind %q, want %q", mut, k, replay.KindDrop)
			}
		case k == "":
			t.Errorf("variant %q has no kind", mut)
		}
	}
	for mut, data := range map[string]string{
		`failing input (seq 5): "id" "c" → "a"`:                         `{"id":"a","n":3,"ok":false,"amount":null}`,
		`failing input (seq 5): "id" "c" → "c-1"`:                       `{"id":"c-1","n":3,"ok":false,"amount":null}`,
		`failing input (seq 5): "n" 3 → 4`:                              `{"id":"c","n":4,"ok":false,"amount":null}`,
		`failing input (seq 5): "n" 3 → -3`:                             `{"id":"c","n":-3,"ok":false,"amount":null}`,
		`failing input (seq 5): "ok" false → true`:                      `{"id":"c","n":3,"ok":true,"amount":null}`,
		`failing input (seq 5): "amount" null → 2.5`:                    `{"id":"c","n":3,"ok":false,"amount":2.5}`,
		`failing input (seq 5): "amount" removed`:                       `{"id":"c","n":3,"ok":false}`,
		`failing input (seq 5): "tag": "t" added`:                       `{"id":"c","n":3,"ok":false,"amount":null,"tag":"t"}`,
		`failing input (seq 5): "ok" false → true, "amount" null → 2.5`: `{"id":"c","n":3,"ok":true,"amount":2.5}`,
	} {
		if got[mut] != data {
			t.Errorf("variant %q: got %q, want %q", mut, got[mut], data)
		}
	}
}

func TestVerifyFixed(t *testing.T) {
	v := verify(t, crashJournal(t), fixedWallet, replay.VerifyOptions{})
	if v.String() != "fixed" || !v.Passed() || v.Candidates != 21 || v.Reproducing != 20 || v.Passing != 20 {
		t.Fatalf("got %s: %d candidates, %d reproduce, %d pass; %s", v, v.Candidates, v.Reproducing, v.Passing, v.Detail)
	}
	if v.Old.String() != "still_failing@13" || v.New.String() != "fixed" {
		t.Fatalf("old %s, new %s", v.Old, v.New)
	}
}

// A fix that only works with the history seen in production.
func TestVerifyRejectsOverfitFix(t *testing.T) {
	overfit := wrap(func(w *wallet, env kavach.Env, in kavach.Input) (bool, error) {
		acct, amt, _ := strings.Cut(string(in.Data), ":")
		if amt == "null" && w.bal[acct] > 0 {
			env.Now()
			env.Emit("rejections", []byte(acct+" null amount"))
			return true, nil
		}
		return false, nil
	})
	v := verify(t, crashJournal(t), overfit, replay.VerifyOptions{})
	if v.New.String() != "fixed" {
		t.Fatalf("new build on the recording: %s", v.New)
	}
	if v.Status != replay.StatusVariantFailed || v.Passed() {
		t.Fatalf("got %s, want variant_failed", v)
	}
	c := v.Variants[v.Variant-1]
	if !c.Reproduces || c.Passed || !strings.HasPrefix(c.Detail, "still_failing@") {
		t.Fatalf("failing variant: %+v", c)
	}
	if want := "variant_failed(" + strconv.Itoa(c.ID) + ")@"; !strings.HasPrefix(v.String(), want) {
		t.Fatalf("verdict %s, want prefix %s", v, want)
	}
}

// A fix that changes behavior before the incident on some variants.
func TestVerifyRejectsVariantDivergence(t *testing.T) {
	timeBomb := func() kavach.Handler { return &wallet{bal: map[string]int64{}, fixed: true, lateFormat: true} }
	v := verify(t, crashJournal(t), timeBomb, replay.VerifyOptions{})
	if v.Status != replay.StatusVariantFailed {
		t.Fatalf("got %s (%s)", v, v.Detail)
	}
	c := v.Variants[v.Variant-1]
	if c.Mutation != "clock shifted by +366 days" || !strings.Contains(c.Detail, "outputs differ from the old build's") {
		t.Fatalf("failing variant: %+v", c)
	}
}

func TestVerifyUnverifiedAndDisabled(t *testing.T) {
	j := crashJournal(t)
	v := verify(t, j, fixedWallet, replay.VerifyOptions{MinVariants: 25})
	if v.Status != replay.StatusUnverified || v.Passed() || !strings.Contains(v.Detail, "only 20 of 21 variants") {
		t.Fatalf("got %s: %s", v, v.Detail)
	}
	v = verify(t, j, fixedWallet, replay.VerifyOptions{MinVariants: -1})
	if v.String() != "fixed" || v.Candidates != 0 {
		t.Fatalf("got %s with %d candidates", v, v.Candidates)
	}
	v = verify(t, j, newWallet, replay.VerifyOptions{})
	if v.String() != "still_failing@13" || v.Candidates != 0 {
		t.Fatalf("got %s with %d candidates", v, v.Candidates)
	}
}

func TestVerifyParallelMatchesSerial(t *testing.T) {
	j := crashJournal(t)
	a, _ := json.Marshal(verify(t, j, fixedWallet, replay.VerifyOptions{}))
	b, _ := json.Marshal(verify(t, j, fixedWallet, replay.VerifyOptions{Parallel: 8}))
	if !bytes.Equal(a, b) {
		t.Fatal("parallel verification differs from serial")
	}
}

func TestVariantFileReplaysLeniently(t *testing.T) {
	vs, err := replay.Variants(crashJournal(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "v.kavach")
	v := vs[0]
	if err := journal.WriteFile(path, v.Header.Meta, v.Records); err != nil {
		t.Fatal(err)
	}
	back, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := *v.Header.Meta.Variant
	want.Kind = "" // not stored in the journal
	if *back.Header.Meta.Variant != want {
		t.Fatalf("variant meta did not round-trip: %+v", back.Header.Meta.Variant)
	}
	res, err := replay.RunFile(path, fixedWallet)
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "ok" || res.Variant != v.Header.Meta.Variant.Mutation {
		t.Fatalf("fixed wallet on a variant: %s %q (%s)", res, res.Variant, res.Detail)
	}
}

func TestVerifyErrorIncident(t *testing.T) {
	frozen := func() kavach.Handler { return &wallet{bal: map[string]int64{}, failOn: "bob"} }
	opts := testOptions(t)
	r := newRecorder(t, frozen(), opts)
	if err := feed(t, r, "alice:10", "carol:2", "bob:5"); err == nil {
		t.Fatal("expected an error")
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	v, err := replay.Verify(j, frozen, newWallet, replay.VerifyOptions{MinVariants: 5})
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "fixed" || v.Reproducing != v.Candidates-1 {
		t.Fatalf("got %s: %d of %d reproduce; %s", v, v.Reproducing, v.Candidates, v.Detail)
	}
}

func TestVariantsOfArrayElements(t *testing.T) {
	opts := testOptions(t)
	r := newRecorder(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		var cmd struct{ Argv []string }
		if err := json.Unmarshal(in.Data, &cmd); err != nil {
			return err
		}
		if cmd.Argv[1] == "boom" {
			return errors.New("boom")
		}
		env.Emit("out", []byte("OK"))
		return nil
	}), opts)
	err := feed(t, r, `{"argv":["SET","a","1"]}`, `{"argv":["GET","b"]}`, `{"argv":["SET","boom","2"]}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		got[info.Mutation] = string(recordAt(t, v, info.Incident).Data)
	}

	for mut, data := range map[string]string{
		`"argv"[0] "SET" → "GET"`:                         `{"argv":["GET","boom","2"]}`,
		`"argv"[1] "boom" → "a"`:                          `{"argv":["SET","a","2"]}`,
		`"argv"[1] "boom" → "b"`:                          `{"argv":["SET","b","2"]}`,
		`"argv"[2] "2" → "1"`:                             `{"argv":["SET","boom","1"]}`,
		`"argv"[1] "boom" → "a", "argv"[2] "2" → "1"`:     `{"argv":["SET","a","1"]}`,
		`"argv"[0] "SET" → "GET", "argv"[1] "boom" → "a"`: `{"argv":["GET","a","2"]}`,
	} {
		found := false
		for m, d := range got {
			if strings.HasSuffix(m, ": "+mut) {
				found = true
				if d != data {
					t.Errorf("variant %q: got %q, want %q", m, d, data)
				}
			}
		}
		if !found {
			t.Errorf("no variant %q among %v", mut, keys(got))
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReplayWithFeatureSteps(t *testing.T) {
	opts := testOptions(t)
	r := newRecorder(t, newWallet(), opts)
	if err := feed(t, r, "alice:10", "bob:5", "alice:7"); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(true); err != nil {
		t.Fatal(err)
	}
	var path string
	filepath.WalkDir(opts.Dir, func(p string, _ os.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".kavach") {
			path = p
		}
		return nil
	})
	j, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []uint64
	for _, rec := range j.Records {
		if rec.Type == journal.TypeInput {
			inputs = append(inputs, rec.Seq)
		}
	}
	// bob's step changes its format: a feature step reports it and goes on;
	// alice's second step is a feature step whose outputs do not change.
	newFormat := func() kavach.Handler { return &bobFormat{wallet: newWallet().(*wallet)} }
	res, err := replay.RunWith(j, newFormat, replay.Options{Features: map[uint64]bool{inputs[1]: true, inputs[2]: true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusOK || len(res.Changed) != 1 || res.Changed[0].Seq != inputs[1] || len(res.Unreached) != 1 || res.Unreached[0] != inputs[2] {
		t.Errorf("got %s, changed %+v, unreached %v", res, res.Changed, res.Unreached)
	}
	if len(res.Invariants) != 1 || res.Invariants[0] != "balance_non_negative" {
		t.Errorf("declared invariants: %v", res.Invariants)
	}
	res, err = replay.Run(j, newFormat)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusDiverged || *res.Seq != inputs[1] {
		t.Errorf("without feature steps: got %s", res)
	}
}

type bobFormat struct{ *wallet }

func (h *bobFormat) Handle(env kavach.Env, in kavach.Input) error {
	h.wallet.newFormat = strings.HasPrefix(string(in.Data), "bob:")
	return h.wallet.Handle(env, in)
}

func TestVerifyRejectsNarrowFixesViaCrossIndexAndPairMutations(t *testing.T) {
	// 1. Cross-index array element mutation (redis-10764 pattern):
	// BZMPOP blocks on non-key arguments when waiting; ZADD to "1" wakes it.
	// A narrow fix skips leading arguments ("0", "1") but still blocks on
	// trailing options ("MIN", "COUNT"). Cross-index mutation replaces ZADD's
	// key ("1", which appeared at argv[2] of BZMPOP) with "MIN" (argv[4] of
	// BZMPOP) and catches the narrow fix.
	t.Run("cross-index", func(t *testing.T) {
		makePop := func(mode string) func() kavach.Handler {
			return func() kavach.Handler {
				var blocked []string
				var popped string
				return &invHandler{
					handle: func(env kavach.Env, in kavach.Input) error {
						popped = ""
						var cmd struct{ Argv []string }
						if err := json.Unmarshal(in.Data, &cmd); err != nil {
							return err
						}
						a := cmd.Argv
						switch a[0] {
						case "BZMPOP":
							switch mode {
							case "old":
								blocked = a[1 : len(a)-1] // blocks on "0","1","jobs","MIN","COUNT"
							case "narrow":
								blocked = a[3 : len(a)-1] // skips "0","1", still blocks on "MIN","COUNT"
							case "fixed":
								blocked = a[3:4] // blocks only on "jobs"
							}
						case "ZADD":
							for _, k := range blocked {
								if k == a[1] {
									popped = k
									env.Emit("reply:waiter", []byte(k))
									break
								}
							}
							env.Emit("reply", []byte(":1"))
						default:
							env.Emit("reply", []byte("OK"))
						}
						return nil
					},
					inv: kavach.Invariant{Name: "pop-key-contract", Check: func() error {
						if popped != "" && popped != "jobs" {
							return errors.New("popped foreign key " + popped)
						}
						return nil
					}},
				}
			}
		}
		opts := testOptions(t)
		r := newRecorder(t, makePop("old")(), opts)
		err := feed(t, r,
			`{"argv":["PING"]}`,
			`{"argv":["ZADD","2","10","p1"]}`,
			`{"argv":["ZADD","3","20","p2"]}`,
			`{"argv":["BZMPOP","0","1","jobs","MIN","COUNT","2"]}`,
			`{"argv":["ZCARD","2"]}`,
			`{"argv":["ZADD","1","10","p3"]}`,
		)
		if err == nil {
			t.Fatal("expected invariant failure")
		}
		j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
		if err != nil {
			t.Fatal(err)
		}
		vNarrow, err := replay.Verify(j, makePop("old"), makePop("narrow"), replay.VerifyOptions{MinVariants: 3})
		if err != nil {
			t.Fatal(err)
		}
		if vNarrow.Status != replay.StatusVariantFailed || !strings.Contains(vNarrow.Detail, `"argv"[1] "1" → "MIN"`) {
			t.Fatalf("narrow fix: got %s (%s), want variant_failed on MIN", vNarrow, vNarrow.Detail)
		}
		vFixed, err := replay.Verify(j, makePop("old"), makePop("fixed"), replay.VerifyOptions{MinVariants: 3})
		if err != nil {
			t.Fatal(err)
		}
		if vFixed.Status != replay.StatusFixed {
			t.Fatalf("fixed build: got %s (%s)", vFixed, vFixed.Detail)
		}
	})

	// 2. Pairwise array element mutation (redis-11734 pattern):
	// BITPOS on a missing key returns -1 before validating start/end/unit.
	// A narrow fix ("unit-only") validates only argv[5] (the unit), so a single
	// element mutation either keeps argv[5]="BITS" (which unit-only rejects) or
	// changes argv[5] to "BIT" while argv[3..4] are valid integers (which does
	// not fail on the old build). Pairwise mutation changes argv[4] ("-1" →
	// "BIT") and argv[5] ("BITS" → "BIT") together and catches unit-only.
	t.Run("pairwise", func(t *testing.T) {
		makeBitpos := func(mode string) func() kavach.Handler {
			return func() kavach.Handler {
				keys := map[string]bool{}
				var lastArgv []string
				var lastReply string
				validUnit := func(u string) bool { return u == "BIT" || u == "BYTE" }
				validInt := func(s string) bool { _, err := strconv.ParseInt(s, 10, 64); return err == nil }
				return &invHandler{
					handle: func(env kavach.Env, in kavach.Input) error {
						var cmd struct{ Argv []string }
						if err := json.Unmarshal(in.Data, &cmd); err != nil {
							return err
						}
						a := cmd.Argv
						lastArgv = a
						reply := func(s string) { lastReply = s; env.Emit("reply", []byte(s)) }
						switch a[0] {
						case "SETBIT":
							keys[a[1]] = true
							reply(":0")
						case "BITCOUNT":
							if len(a) == 5 && (!validInt(a[2]) || !validInt(a[3]) || !validUnit(a[4])) {
								reply("-ERR syntax")
								return nil
							}
							reply(":1")
						case "BITPOS":
							if a[2] != "0" && a[2] != "1" {
								reply("-ERR bit")
								return nil
							}
							if mode == "unit-only" && len(a) == 6 && !validUnit(a[5]) {
								reply("-ERR syntax")
								return nil
							}
							if mode == "fixed" && len(a) == 6 && (!validInt(a[3]) || !validInt(a[4]) || !validUnit(a[5])) {
								reply("-ERR syntax")
								return nil
							}
							if !keys[a[1]] {
								reply(":-1")
								return nil
							}
							if len(a) == 6 && (!validInt(a[3]) || !validInt(a[4]) || !validUnit(a[5])) {
								reply("-ERR syntax")
								return nil
							}
							reply(":3")
						}
						return nil
					},
					inv: kavach.Invariant{Name: "bit-range-syntax", Check: func() error {
						if len(lastArgv) == 6 && lastArgv[0] == "BITPOS" {
							if (lastArgv[2] != "0" && lastArgv[2] != "1") || !validInt(lastArgv[3]) || !validInt(lastArgv[4]) || !validUnit(lastArgv[5]) {
								if !strings.HasPrefix(lastReply, "-ERR") {
									return errors.New("accepted invalid syntax")
								}
							}
						}
						return nil
					}},
				}
			}
		}
		opts := testOptions(t)
		r := newRecorder(t, makeBitpos("old")(), opts)
		err := feed(t, r,
			`{"argv":["SETBIT","k1","3","1"]}`,
			`{"argv":["BITCOUNT","k1","0","-1","BIT"]}`,
			`{"argv":["BITPOS","k1","1","0","-1","BIT"]}`,
			`{"argv":["BITPOS","k1","1","1","-1","BYTE"]}`,
			`{"argv":["BITPOS","k1","1","0","-1","BITS"]}`,
			`{"argv":["BITPOS","missing","1","0","-1","BITS"]}`,
		)
		if err == nil {
			t.Fatal("expected invariant failure")
		}
		j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
		if err != nil {
			t.Fatal(err)
		}
		vNarrow, err := replay.Verify(j, makeBitpos("old"), makeBitpos("unit-only"), replay.VerifyOptions{MinVariants: 5})
		if err != nil {
			t.Fatal(err)
		}
		if vNarrow.Status != replay.StatusVariantFailed || !strings.Contains(vNarrow.Detail, `"argv"[4] "-1" → "BIT", "argv"[5] "BITS" → "BIT"`) {
			t.Fatalf("unit-only fix: got %s (%s), want variant_failed on pair mutation", vNarrow, vNarrow.Detail)
		}
		vFixed, err := replay.Verify(j, makeBitpos("old"), makeBitpos("fixed"), replay.VerifyOptions{MinVariants: 5})
		if err != nil {
			t.Fatal(err)
		}
		if vFixed.Status != replay.StatusFixed {
			t.Fatalf("fixed build: got %s (%s)", vFixed, vFixed.Detail)
		}
	})
}

type invHandler struct {
	handle func(kavach.Env, kavach.Input) error
	inv    kavach.Invariant
}

func (h *invHandler) Handle(env kavach.Env, in kavach.Input) error { return h.handle(env, in) }
func (h *invHandler) Invariants() []kavach.Invariant               { return []kavach.Invariant{h.inv} }

func recordFailingJSON(t *testing.T, h kavach.Handler, inputs ...string) *journal.Journal {
	t.Helper()
	opts := testOptions(t)
	r := newRecorder(t, h, opts)
	if err := feed(t, r, inputs...); err == nil {
		t.Fatal("expected recorded failure")
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func okReplayResult(j *journal.Journal) *replay.Result {
	var steps []replay.StepResult
	for _, r := range j.Records {
		if r.Type == journal.TypeInput {
			steps = append(steps, replay.StepResult{Seq: r.Seq})
		}
	}
	return &replay.Result{Status: replay.StatusOK, Steps: steps}
}

func fixedReplayResult(j *journal.Journal) *replay.Result {
	res := okReplayResult(j)
	res.Status = replay.StatusFixed
	return res
}

func failingReplayAt(j *journal.Journal, seq uint64, step replay.StepResult, inv string) *replay.Result {
	var steps []replay.StepResult
	for _, r := range j.Records {
		if r.Type != journal.TypeInput {
			continue
		}
		if r.Seq == seq {
			step.Seq = seq
			steps = append(steps, step)
			break
		}
		steps = append(steps, replay.StepResult{Seq: r.Seq})
	}
	s := seq
	res := &replay.Result{Status: replay.StatusStillFailing, Seq: &s, Steps: steps}
	if inv != "" {
		res.Status = replay.StatusInvariantViolated
		res.Invariant = inv
	}
	return res
}

func TestVerifyNormalizedFailureMatching(t *testing.T) {
	t.Run("error-numbers-uuids-timestamps-hex", func(t *testing.T) {
		type payload struct {
			Amount int     `json:"amount"`
			Delta  float64 `json:"delta"`
			ID     string  `json:"id"`
			TS     string  `json:"ts"`
			Hash   string  `json:"hash"`
		}
		oldFn := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				var p payload
				if err := json.Unmarshal(in.Data, &p); err != nil {
					return err
				}
				if p.Amount >= 100 {
					return fmt.Errorf("limit exceeded: %d > 50, delta=%.2f, win=%s, id=%s, digest=0x%s, sha=%s",
						p.Amount, p.Delta, p.TS, p.ID, p.Hash[:8], p.Hash)
				}
				env.Emit("ok", []byte("1"))
				return nil
			})
		}
		newFn := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("ok", []byte("1"))
				return nil
			})
		}
		j := recordFailingJSON(t, oldFn(),
			`{"amount":10,"delta":1.5,"id":"11111111-2222-3333-4444-555555555555","ts":"2026-01-15T09:00:00Z","hash":"0123456789abcdef0123456789abcdef"}`,
			`{"amount":20,"delta":-2.25,"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","ts":"2026-01-15T09:30:00+05:30","hash":"abcdef0123456789abcdef0123456789"}`,
			`{"amount":100,"delta":-3.5,"id":"550e8400-e29b-41d4-a716-446655440000","ts":"2026-01-15T10:00:00.123456Z","hash":"deadbeefcafebabe0123456789abcdef"}`,
		)
		v, err := replay.Verify(j, oldFn, newFn, replay.VerifyOptions{MinVariants: 5})
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != replay.StatusFixed || v.Reproducing < 5 {
			t.Fatalf("got %s (reproducing=%d/%d): %s", v, v.Reproducing, v.Candidates, v.Detail)
		}
	})

	t.Run("panic-floats-and-signed-nums", func(t *testing.T) {
		j := recordFailingJSON(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			if strings.Contains(string(in.Data), `"boom"`) {
				panic("drift 1.0000000000000002 at -5")
			}
			env.Emit("ok", []byte("1"))
			return nil
		}), `{"op":"ok"}`, `{"op":"ok2"}`, `{"op":"boom"}`)

		v, err := replay.VerifyWith(j,
			func(vj *journal.Journal) (*replay.Result, error) {
				if vj.Header.Meta.Variant == nil {
					return failingReplayAt(vj, j.Records[len(j.Records)-2].Seq, replay.StepResult{Panic: "drift 1.0000000000000002 at -5"}, ""), nil
				}
				return failingReplayAt(vj, vj.Header.Meta.Variant.Incident, replay.StepResult{Panic: "drift 2.5 at +17"}, ""), nil
			},
			func(vj *journal.Journal) (*replay.Result, error) {
				if vj.Header.Meta.Variant == nil {
					return fixedReplayResult(vj), nil
				}
				return okReplayResult(vj), nil
			},
			replay.VerifyOptions{MinVariants: 3},
		)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != replay.StatusFixed || v.Reproducing != v.Candidates {
			t.Fatalf("got %s (%d/%d): %s", v, v.Reproducing, v.Candidates, v.Detail)
		}
	})

	t.Run("distinct-errors-and-strict-modes-never-match", func(t *testing.T) {
		j := recordFailingJSON(t, kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			if strings.Contains(string(in.Data), `"boom"`) {
				return errors.New("account_not_found")
			}
			env.Emit("ok", []byte("1"))
			return nil
		}), `{"op":"ok"}`, `{"op":"boom"}`)

		jInv := recordFailingJSON(t, &invHandler{
			handle: func(env kavach.Env, in kavach.Input) error { return nil },
			inv: kavach.Invariant{Name: "inv_1", Check: func() error {
				return errors.New("violated")
			}},
		}, `{"op":"boom"}`)

		cases := []struct {
			name string
			j    *journal.Journal
			step replay.StepResult
			inv  string
		}{
			{name: "different-error-text", j: j, step: replay.StepResult{Error: "insufficient funds"}},
			{name: "different-error-code-template", j: j, step: replay.StepResult{Error: "code 500: internal error"}},
			{name: "error-vs-panic", j: j, step: replay.StepResult{Panic: "account_not_found"}},
			{name: "error-vs-crash", j: j, step: replay.StepResult{Crash: "SIGSEGV"}},
			{name: "error-vs-timeout", j: j, step: replay.StepResult{Timeout: "1s"}},
			{name: "different-invariant-name", j: jInv, inv: "inv_2"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v, err := replay.VerifyWith(tc.j,
					func(vj *journal.Journal) (*replay.Result, error) {
						if vj.Header.Meta.Variant == nil {
							var recSeq uint64
							for _, r := range vj.Records {
								if r.Type == journal.TypeInput {
									recSeq = r.Seq
								}
							}
							if tc.inv != "" {
								return failingReplayAt(vj, recSeq, replay.StepResult{}, "inv_1"), nil
							}
							return failingReplayAt(vj, recSeq, replay.StepResult{Error: "account_not_found"}, ""), nil
						}
						return failingReplayAt(vj, vj.Header.Meta.Variant.Incident, tc.step, tc.inv), nil
					},
					func(vj *journal.Journal) (*replay.Result, error) {
						if vj.Header.Meta.Variant == nil {
							return fixedReplayResult(vj), nil
						}
						return okReplayResult(vj), nil
					},
					replay.VerifyOptions{MinVariants: 1},
				)
				if err != nil {
					t.Fatal(err)
				}
				if v.Status != replay.StatusUnverified || v.Reproducing != 0 {
					t.Fatalf("got %s (reproducing=%d), want unverified with 0 reproducing", v, v.Reproducing)
				}
			})
		}
	})
}

func TestVerifyKindDiversityAndReproducingByKind(t *testing.T) {
	oldFn := func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			var ev struct {
				Amount int `json:"amount"`
			}
			if err := json.Unmarshal(in.Data, &ev); err != nil {
				return err
			}
			if ev.Amount >= 100 {
				return fmt.Errorf("too high: %d", ev.Amount)
			}
			env.Emit("ok", []byte("1"))
			return nil
		})
	}
	newFn := func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			env.Emit("ok", []byte("1"))
			return nil
		})
	}
	j := recordFailingJSON(t, oldFn(),
		`{"amount":10}`,
		`{"amount":20}`,
		`{"amount":100}`,
	)

	v, err := replay.Verify(j, oldFn, newFn, replay.VerifyOptions{MinVariants: 5})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != replay.StatusFixed {
		t.Fatalf("got %s: %s", v, v.Detail)
	}
	if len(v.ReproducingByKind) < 2 {
		t.Fatalf("expected >= 2 reproducing kinds, got %v", v.ReproducingByKind)
	}
	sum := 0
	for k, n := range v.ReproducingByKind {
		if k == "" {
			t.Fatalf("empty kind key in ReproducingByKind: %v", v.ReproducingByKind)
		}
		sum += n
	}
	if sum != v.Reproducing {
		t.Fatalf("ReproducingByKind sum %d != Reproducing %d (%v)", sum, v.Reproducing, v.ReproducingByKind)
	}
	for _, c := range v.Variants {
		if c.Kind == "" {
			t.Fatalf("variant %d missing Kind: %+v", c.ID, c)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reproducing_by_kind"`) || !strings.Contains(string(raw), `"kind"`) {
		t.Fatalf("missing kind fields in JSON: %s", raw)
	}

	// Simulate an incident where only a single mutation kind reproduces.
	singleKindOld := func(vj *journal.Journal) (*replay.Result, error) {
		if vj.Header.Meta.Variant == nil {
			var recSeq uint64
			for _, r := range vj.Records {
				if r.Type == journal.TypeInput {
					recSeq = r.Seq
				}
			}
			return failingReplayAt(vj, recSeq, replay.StepResult{Error: "too high: 100"}, ""), nil
		}
		if vj.Header.Meta.Variant.Kind == replay.KindDrop {
			return failingReplayAt(vj, vj.Header.Meta.Variant.Incident, replay.StepResult{Error: "too high: 100"}, ""), nil
		}
		return okReplayResult(vj), nil
	}
	passNew := func(vj *journal.Journal) (*replay.Result, error) {
		if vj.Header.Meta.Variant == nil {
			return fixedReplayResult(vj), nil
		}
		return okReplayResult(vj), nil
	}

	// Default MinKinds=0 passes when MinVariants is met.
	v0, err := replay.VerifyWith(j, singleKindOld, passNew, replay.VerifyOptions{MinVariants: 2})
	if err != nil {
		t.Fatal(err)
	}
	if v0.Status != replay.StatusFixed {
		t.Fatalf("MinKinds=0: got %s (%s)", v0, v0.Detail)
	}

	// MinKinds=2 rejects single-kind reproduction even though Reproducing >= MinVariants.
	vLow, err := replay.VerifyWith(j, singleKindOld, passNew, replay.VerifyOptions{MinVariants: 2, MinKinds: 2})
	if err != nil {
		t.Fatal(err)
	}
	if vLow.Status != replay.StatusUnverified || vLow.Passed() || !strings.Contains(vLow.Detail, "1 distinct mutation kinds") {
		t.Fatalf("MinKinds=2: got %s (%s)", vLow, vLow.Detail)
	}

	// MinKinds=1 passes.
	v1, err := replay.VerifyWith(j, singleKindOld, passNew, replay.VerifyOptions{MinVariants: 2, MinKinds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v1.Status != replay.StatusFixed {
		t.Fatalf("MinKinds=1: got %s (%s)", v1, v1.Detail)
	}
}

func TestVerifyRejectsPureMetadataPadding(t *testing.T) {
	// Journal whose payload only contains non-semantic metadata keys, so field
	// variants are guaranteed to exist for those metadata keys.
	metaHandler := func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			if strings.Contains(string(in.Data), `"r3"`) {
				return errors.New("boom")
			}
			env.Emit("ok", []byte("1"))
			return nil
		})
	}
	jMeta := recordFailingJSON(t, metaHandler(),
		`{"customer_email":"a@example.com","trace_id":"t1","request_id":"r1"}`,
		`{"customer_email":"b@example.com","trace_id":"t2","request_id":"r2"}`,
		`{"customer_email":"c@example.com","trace_id":"t3","request_id":"r3"}`,
	)

	passNew := func(vj *journal.Journal) (*replay.Result, error) {
		if vj.Header.Meta.Variant == nil {
			return fixedReplayResult(vj), nil
		}
		return okReplayResult(vj), nil
	}
	metaOnlyOld := func(vj *journal.Journal) (*replay.Result, error) {
		if vj.Header.Meta.Variant == nil {
			var recSeq uint64
			for _, r := range vj.Records {
				if r.Type == journal.TypeInput {
					recSeq = r.Seq
				}
			}
			return failingReplayAt(vj, recSeq, replay.StepResult{Error: "boom"}, ""), nil
		}
		k := vj.Header.Meta.Variant.Kind
		if k == replay.KindField || k == replay.KindPair {
			return failingReplayAt(vj, vj.Header.Meta.Variant.Incident, replay.StepResult{Error: "boom"}, ""), nil
		}
		return okReplayResult(vj), nil
	}

	vRejected, err := replay.VerifyWith(jMeta, metaOnlyOld, passNew, replay.VerifyOptions{MinVariants: 2})
	if err != nil {
		t.Fatal(err)
	}
	if vRejected.Reproducing < 2 {
		t.Fatalf("expected >= 2 reproducing metadata variants, got %d", vRejected.Reproducing)
	}
	if vRejected.Status != replay.StatusUnverified || vRejected.Passed() || vRejected.Detail != "all reproducing variants only mutated non-semantic metadata fields" {
		t.Fatalf("got %s (%s), want unverified anti-metadata-padding", vRejected, vRejected.Detail)
	}

	// If even one structural variant (e.g. drop) reproduces alongside metadata ones, it passes.
	metaPlusDropOld := func(vj *journal.Journal) (*replay.Result, error) {
		if vj.Header.Meta.Variant != nil && vj.Header.Meta.Variant.Kind == replay.KindDrop {
			return failingReplayAt(vj, vj.Header.Meta.Variant.Incident, replay.StepResult{Error: "boom"}, ""), nil
		}
		return metaOnlyOld(vj)
	}
	vMixed, err := replay.VerifyWith(jMeta, metaPlusDropOld, passNew, replay.VerifyOptions{MinVariants: 2})
	if err != nil {
		t.Fatal(err)
	}
	if vMixed.Status != replay.StatusFixed {
		t.Fatalf("mixed metadata+drop: got %s (%s)", vMixed, vMixed.Detail)
	}

	// Normal semantic keys (including values containing metadata-looking strings or commas) must pass.
	semHandler := func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			if strings.Contains(string(in.Data), `"boom"`) {
				return errors.New("boom")
			}
			env.Emit("ok", []byte("1"))
			return nil
		})
	}
	jSem := recordFailingJSON(t, semHandler(),
		`{"amount":10,"note":"email, trace_id","tag":"ok"}`,
		`{"amount":20,"note":"request_id, pan","tag":"ok2"}`,
		`{"amount":30,"note":"email, trace_id","tag":"boom"}`,
	)
	vSem, err := replay.VerifyWith(jSem, metaOnlyOld, passNew, replay.VerifyOptions{MinVariants: 2})
	if err != nil {
		t.Fatal(err)
	}
	if vSem.Status != replay.StatusFixed {
		t.Fatalf("semantic keys: got %s (%s)", vSem, vSem.Detail)
	}
}

func TestVerifyPreFailureOutputPresence(t *testing.T) {
	// Simulate a multi-item handler where the recorded failing step emitted an
	// output for item 0 before failing on item 1. A mutation that flips an
	// admission precondition (allow_overdraft: true -> false) aborts item 0
	// immediately with 0 outputs on the failing step; both old and new reject it
	// with the same error, so it must NOT count as reproducing the incident.
	makeBulk := func(fixed bool) func() kavach.Handler {
		return func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				var req struct {
					AllowOverdraft bool   `json:"allow_overdraft"`
					Item           string `json:"item"`
				}
				if err := json.Unmarshal(in.Data, &req); err != nil {
					return err
				}
				if !req.AllowOverdraft && req.Item == "bad" {
					return errors.New("insufficient funds")
				}
				env.Emit("tx0", []byte(req.Item))
				if !fixed && req.Item == "bad" {
					return errors.New("insufficient funds")
				}
				env.Emit("tx1", []byte(req.Item))
				return nil
			})
		}
	}
	j := recordFailingJSON(t, makeBulk(false)(),
		`{"allow_overdraft":false,"item":"ok1"}`,
		`{"allow_overdraft":true,"item":"ok2"}`,
		`{"allow_overdraft":true,"item":"bad"}`,
	)

	v, err := replay.Verify(j, makeBulk(false), makeBulk(true), replay.VerifyOptions{MinVariants: 3})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != replay.StatusFixed || !v.Passed() {
		t.Fatalf("got %s (%s), want fixed", v, v.Detail)
	}
	checkedPrecond := false
	for _, c := range v.Variants {
		if strings.Contains(c.Mutation, `"allow_overdraft" true → false`) {
			checkedPrecond = true
			if c.Reproduces {
				t.Fatalf("early precondition rejection %q should not count as reproducing", c.Mutation)
			}
		}
	}
	if !checkedPrecond {
		t.Fatal("expected an allow_overdraft true -> false variant")
	}
}
