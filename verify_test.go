package kavach_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
)

func crashJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.ReadFile(recordCrash(t))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// wrapped lets a test put a candidate fix in front of the buggy wallet.
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

func verify(t *testing.T, j *journal.Journal, newHandler func() kavach.Handler, opts kavach.VerifyOptions) *kavach.Verification {
	t.Helper()
	v, err := kavach.Verify(j, newWallet, newHandler, opts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVariantsOfPlainInputs(t *testing.T) {
	j := crashJournal(t)
	vs, err := kavach.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	// The inputs are not JSON, so only structural mutations apply: 3 moves,
	// 3 drops plus dropping all history, 3 redeliveries and 4 clock shifts.
	if len(vs) != 14 {
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
		"failing input (seq 12) moved 1 input earlier, before seq 8",
		"failing input (seq 12) moved 3 inputs earlier, before seq 0",
		"input seq 4 dropped",
		"all 3 inputs before the failing one dropped",
		"input seq 0 delivered twice",
		"clock shifted by -5h30m",
	} {
		if !contains(muts, want) {
			t.Errorf("no variant %q in %q", want, muts)
		}
	}

	again, _ := kavach.Variants(j)
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
	r := kavach.NewRecorder(kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
		var ev struct{ Amount *float64 }
		if err := json.Unmarshal(in.Data, &ev); err != nil {
			return err
		}
		env.Emit("out", []byte(strings.Repeat("x", int(*ev.Amount))))
		return nil
	}), testOptions(t))
	err := feed(t, r, `{"id":"a","n":1,"ok":true,"amount":2.5}`, `{"id":"b","n":2,"amount":1,"tag":"t"}`, `{"id":"c", "n":3, "ok":false, "amount":null}`)
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := kavach.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vs {
		info := v.Header.Meta.Variant
		got[info.Mutation] = string(recordAt(t, v, info.Incident).Data)
	}
	for mut, data := range map[string]string{
		`failing input (seq 4): "id" "c" → "a"`:      `{"id":"a","n":3,"ok":false,"amount":null}`,
		`failing input (seq 4): "id" "c" → "c-1"`:    `{"id":"c-1","n":3,"ok":false,"amount":null}`,
		`failing input (seq 4): "n" 3 → 4`:           `{"id":"c","n":4,"ok":false,"amount":null}`,
		`failing input (seq 4): "n" 3 → -3`:          `{"id":"c","n":-3,"ok":false,"amount":null}`,
		`failing input (seq 4): "ok" false → true`:   `{"id":"c","n":3,"ok":true,"amount":null}`,
		`failing input (seq 4): "amount" null → 2.5`: `{"id":"c","n":3,"ok":false,"amount":2.5}`,
		`failing input (seq 4): "amount" removed`:    `{"id":"c","n":3,"ok":false}`,
		`failing input (seq 4): "tag": "t" added`:    `{"id":"c","n":3,"ok":false,"amount":null,"tag":"t"}`,
	} {
		if got[mut] != data {
			t.Errorf("variant %q: got %q, want %q", mut, got[mut], data)
		}
	}
}

func TestVerifyFixed(t *testing.T) {
	v := verify(t, crashJournal(t), fixedWallet, kavach.VerifyOptions{})
	if v.String() != "fixed" || !v.Passed() || v.Candidates != 14 || v.Reproducing != 14 || v.Passing != 14 {
		t.Fatalf("got %s: %d candidates, %d reproduce, %d pass; %s", v, v.Candidates, v.Reproducing, v.Passing, v.Detail)
	}
	if v.Old.String() != "still_failing@12" || v.New.String() != "fixed" {
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
	v := verify(t, crashJournal(t), overfit, kavach.VerifyOptions{})
	if v.New.String() != "fixed" {
		t.Fatalf("new build on the recording: %s", v.New)
	}
	if v.Status != kavach.StatusVariantFailed || v.Passed() {
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
	v := verify(t, crashJournal(t), timeBomb, kavach.VerifyOptions{})
	if v.Status != kavach.StatusVariantFailed {
		t.Fatalf("got %s (%s)", v, v.Detail)
	}
	c := v.Variants[v.Variant-1]
	if c.Mutation != "clock shifted by +366 days" || !strings.Contains(c.Detail, "outputs differ from the old build's") {
		t.Fatalf("failing variant: %+v", c)
	}
}

func TestVerifyUnverifiedAndDisabled(t *testing.T) {
	j := crashJournal(t)
	v := verify(t, j, fixedWallet, kavach.VerifyOptions{MinVariants: 20})
	if v.Status != kavach.StatusUnverified || v.Passed() || !strings.Contains(v.Detail, "only 14 of 14 variants") {
		t.Fatalf("got %s: %s", v, v.Detail)
	}
	v = verify(t, j, fixedWallet, kavach.VerifyOptions{MinVariants: -1})
	if v.String() != "fixed" || v.Candidates != 0 {
		t.Fatalf("got %s with %d candidates", v, v.Candidates)
	}
	v = verify(t, j, newWallet, kavach.VerifyOptions{})
	if v.String() != "still_failing@12" || v.Candidates != 0 {
		t.Fatalf("got %s with %d candidates", v, v.Candidates)
	}
}

func TestVerifyParallelMatchesSerial(t *testing.T) {
	j := crashJournal(t)
	a, _ := json.Marshal(verify(t, j, fixedWallet, kavach.VerifyOptions{}))
	b, _ := json.Marshal(verify(t, j, fixedWallet, kavach.VerifyOptions{Parallel: 8}))
	if !bytes.Equal(a, b) {
		t.Fatal("parallel verification differs from serial")
	}
}

func TestVariantFileReplaysLeniently(t *testing.T) {
	vs, err := kavach.Variants(crashJournal(t))
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
	if *back.Header.Meta.Variant != *v.Header.Meta.Variant {
		t.Fatalf("variant meta did not round-trip: %+v", back.Header.Meta.Variant)
	}
	res, err := kavach.ReplayFile(path, fixedWallet)
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
	r := kavach.NewRecorder(frozen(), opts)
	if err := feed(t, r, "alice:10", "carol:2", "bob:5"); err == nil {
		t.Fatal("expected an error")
	}
	paths, _ := filepath.Glob(filepath.Join(opts.Dir, "*-error.kavach"))
	if len(paths) != 1 {
		t.Fatalf("fixtures: %v", paths)
	}
	j, err := journal.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	v, err := kavach.Verify(j, frozen, newWallet, kavach.VerifyOptions{MinVariants: 5})
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "fixed" || v.Reproducing != v.Candidates {
		t.Fatalf("got %s: %d of %d reproduce; %s", v, v.Reproducing, v.Candidates, v.Detail)
	}
}
