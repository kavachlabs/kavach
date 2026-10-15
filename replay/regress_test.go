package replay_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// bitKV is the BITPOS pattern of redis-11734: BITPOS on a missing key fails
// in the old build; the first set bit of an existing key is 3. The buggy fix
// handles the missing key but converts a BIT-range start twice.
type bitKV struct {
	mode string // "old", "fixed", "buggy", "narrow-buggy"
	keys map[string]bool
}

func newBitKV(mode string) func() kavach.Handler {
	return func() kavach.Handler { return &bitKV{mode, map[string]bool{}} }
}

func (h *bitKV) Handle(env kavach.Env, in kavach.Input) error {
	var c struct{ Argv []string }
	if err := json.Unmarshal(in.Data, &c); err != nil {
		return err
	}
	a := c.Argv
	switch a[0] {
	case "SET":
		h.keys[a[1]] = true
		env.Emit("reply", []byte("OK"))
	case "BITPOS":
		if !h.keys[a[1]] {
			if h.mode == "old" || h.mode == "narrow-buggy" && (len(a) < 4 || a[3] != "0") {
				return fmt.Errorf("no such key %s", a[1])
			}
			env.Emit("reply", []byte(":-1"))
			return nil
		}
		start := 0
		if len(a) > 3 {
			start, _ = strconv.Atoi(a[3])
		}
		if h.mode == "buggy" || h.mode == "narrow-buggy" {
			start *= 2
		}
		env.Emit("reply", []byte(":"+strconv.Itoa(3+start)))
	default:
		env.Emit("reply", []byte("+"+a[0]))
	}
	return nil
}

func recordJournal(t *testing.T, h kavach.Handler, inputs ...string) *journal.Journal {
	t.Helper()
	opts := testOptions(t)
	r := newRecorder(t, h, opts)
	if err := feed(t, r, inputs...); err == nil {
		t.Fatal("expected the last input to fail")
	}
	j, err := journal.ReadFile(fixtureIn(t, r, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func bitposJournal(t *testing.T) *journal.Journal {
	return recordJournal(t, newBitKV("old")(),
		`{"argv":["PING"]}`,
		`{"argv":["SET","k","v"]}`,
		`{"argv":["SETBIT","k","7","1"]}`,
		`{"argv":["BITPOS","k","1","0","-1","BIT"]}`,
		`{"argv":["GET","k"]}`,
		`{"argv":["BITPOS","k","1"]}`,
		`{"argv":["BITPOS","nokey","1","0","-1","BIT"]}`,
	)
}

func TestRegressionNeighbours(t *testing.T) {
	j := bitposJournal(t)
	vs, err := replay.RegressionVariants(j, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || len(vs) > replay.MaxRegressionCandidates {
		t.Fatalf("got %d regression variants", len(vs))
	}
	// Only the two BITPOS inputs share a leaf with the failing one
	// (argv[0], "1"); PING, SET, GET and SETBIT share none.
	want := map[uint64]bool{}
	for _, r := range j.Records {
		if r.Type == journal.TypeInput && strings.Contains(string(r.Data), `"BITPOS","k"`) {
			want[r.Seq] = true
		}
	}
	if len(want) != 2 {
		t.Fatalf("test setup: %d BITPOS inputs", len(want))
	}
	for i, v := range vs {
		info := v.Header.Meta.Variant
		if info.ID != i+1 || info.Kind != replay.KindRegression || info.Failure != "" {
			t.Fatalf("variant %d meta = %+v", i, info)
		}
		for _, r := range v.Records {
			if r.Type == journal.TypeOutput || r.Type == journal.TypeMarker {
				t.Fatalf("variant %d carries a %s record", info.ID, r.Type)
			}
		}
		var seq uint64
		if _, err := fmt.Sscanf(info.Mutation, "input (seq %d)", &seq); err != nil || !want[seq] {
			t.Errorf("variant %d mutates a non-neighbour: %s", info.ID, info.Mutation)
		}
	}

	again, _ := replay.RegressionVariants(j, 0)
	a, _ := json.Marshal(vs)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatal("regression variant generation is not deterministic")
	}
	few, _ := replay.RegressionVariants(j, 5)
	if len(few) != 5 {
		t.Fatalf("limit 5 gave %d variants", len(few))
	}
	// They are a budget of their own.
	inc, _ := replay.Variants(j)
	if len(inc) == 0 || inc[0].Header.Meta.Variant.Kind == replay.KindRegression {
		t.Fatal("incident variants changed")
	}
}

func TestRegressionNoNeighbours(t *testing.T) {
	j := recordJournal(t, newBitKV("old")(),
		`{"argv":["PING"]}`,
		`{"argv":["SET","k","v"]}`,
		`{"argv":["BITPOS","nokey","1"]}`,
	)
	vs, err := replay.RegressionVariants(j, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("got %d variants, want none: %s", len(vs), vs[0].Header.Meta.Variant.Mutation)
	}
}

func verifyBit(t *testing.T, j *journal.Journal, mode string, opts replay.VerifyOptions) *replay.Verification {
	t.Helper()
	opts.MinVariants = 3
	v, err := replay.Verify(j, newBitKV("old"), newBitKV(mode), opts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyRegressed(t *testing.T) {
	j := bitposJournal(t)
	v := verifyBit(t, j, "buggy", replay.VerifyOptions{})
	if v.Status != replay.StatusRegressed || v.Passed() {
		t.Fatalf("got %s (%s)", v, v.Detail)
	}
	if v.Regression == 0 || v.Seq == nil || v.Verdict != fmt.Sprintf("regressed(%d)@%d", v.Regression, *v.Seq) {
		t.Fatalf("verdict %q, regression %d, seq %v", v.Verdict, v.Regression, v.Seq)
	}
	if !strings.Contains(v.Detail, "reply") || !strings.Contains(v.Detail, ":") {
		t.Errorf("detail names no outputs: %s", v.Detail)
	}
	c := v.Regressions[v.Regression-1]
	if !c.Counted || c.Passed || c.Journal == nil {
		t.Fatalf("check = %+v", c)
	}

	serial := verifyBit(t, j, "buggy", replay.VerifyOptions{Parallel: 1})
	par := verifyBit(t, j, "buggy", replay.VerifyOptions{Parallel: 8})
	if serial.Verdict != par.Verdict || serial.Detail != par.Detail {
		t.Fatalf("parallelism changed the result: %s vs %s", serial, par)
	}

	off := verifyBit(t, j, "buggy", replay.VerifyOptions{Regressions: -1})
	if off.Status != replay.StatusFixed || off.RegressionCandidates != 0 {
		t.Fatalf("regressions off: got %s", off)
	}
}

func TestVerifyFixedIgnoresOldBuildFailures(t *testing.T) {
	j := bitposJournal(t)
	v := verifyBit(t, j, "fixed", replay.VerifyOptions{})
	if v.Status != replay.StatusFixed {
		t.Fatalf("got %s (%s)", v, v.Detail)
	}
	if v.Neighbours == 0 || v.RegressionCounted == 0 || v.RegressionCounted != v.RegressionPassing {
		t.Fatalf("neighbours %d, counted %d, passing %d", v.Neighbours, v.RegressionCounted, v.RegressionPassing)
	}
	// Mutations that make the old build fail (BITPOS on a missing key) say
	// nothing about the fix, though the new build answers them differently.
	ignored := 0
	for _, c := range v.Regressions {
		if !c.Counted {
			ignored++
			if c.Passed || c.New != "" {
				t.Errorf("uncounted check was judged: %+v", c)
			}
		}
	}
	if ignored == 0 {
		t.Fatal("no regression variant failed on the old build")
	}
}

func TestVerifyRegressedPrecedence(t *testing.T) {
	j := bitposJournal(t)

	// A fix too narrow for the incident's variants is reported as such first.
	if v := verifyBit(t, j, "narrow-buggy", replay.VerifyOptions{}); v.Status != replay.StatusVariantFailed {
		t.Errorf("narrow and buggy: got %s, want variant_failed", v)
	}

	// Too few reproducing variants does not hide a regression.
	v, err := replay.Verify(j, newBitKV("old"), newBitKV("buggy"), replay.VerifyOptions{MinVariants: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != replay.StatusRegressed {
		t.Errorf("buggy with few variants: got %s, want regressed", v)
	}

	// While the incident still fails, no regression variant runs.
	if v := verifyBit(t, j, "old", replay.VerifyOptions{}); v.Passed() || v.RegressionCandidates != 0 {
		t.Errorf("unfixed: got %s with %d regression candidates", v, v.RegressionCandidates)
	}
}
