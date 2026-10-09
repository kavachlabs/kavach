package main

import (
	"encoding/json"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// Candidate fixes for the null-amount incident, as an agent might write them.
// Each wraps the planted-bug ledger and guards some inputs before it.
type guardedLedger struct {
	*Ledger
	guard func(Event) bool // true: reject the event as missing its amount
}

func (g guardedLedger) Handle(env kavach.Env, in kavach.Input) error {
	var ev Event
	if json.Unmarshal(in.Data, &ev) == nil && g.guard(ev) {
		return reject(env, ev, "missing amount")
	}
	return g.Ledger.Handle(env, in)
}

func candidate(guard func(Event) bool) func() kavach.Handler {
	return func() kavach.Handler { return guardedLedger{NewLedger(), guard} }
}

var candidates = []struct {
	name string
	new  func() kavach.Handler
	want string
}{
	// The real fix: any event without an amount is rejected.
	{"nil check", candidate(func(ev Event) bool { return ev.Amount == nil }), "fixed"},
	// Overfit to the recorded event.
	{"skip evt-008", candidate(func(ev Event) bool { return ev.ID == "evt-008" }), "variant_failed"},
	// Only the event type seen in the incident is guarded.
	{"deposits only", candidate(func(ev Event) bool { return ev.Type == "deposit" && ev.Amount == nil }), "variant_failed"},
}

func TestVerifyCandidateFixes(t *testing.T) {
	if fixNullAmount {
		t.Skip("candidates wrap the planted bug")
	}
	j, err := journal.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		t.Run(c.name, func(t *testing.T) {
			v, err := replay.Verify(j, newHandler, c.new, replay.VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d candidates, %d reproduce, %d pass; %s", v, v.Candidates, v.Reproducing, v.Passing, v.Detail)
			for _, vc := range v.Variants {
				t.Logf("  %2d repro=%-5v pass=%-5v old=%-28s new=%-24s %s", vc.ID, vc.Reproduces, vc.Passed, vc.Old, vc.New, vc.Mutation)
			}
			if string(v.Status) != c.want {
				t.Fatalf("got %s, want %s", v, c.want)
			}
		})
	}
}

// BenchmarkVerifyFixture measures in-process verification of the correct fix:
// two replays of the fixture, 44 variants under the old build and the 41 that
// reproduce under the new one.
func BenchmarkVerifyFixture(b *testing.B) {
	j, err := journal.ReadFile(fixture)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, err := replay.Verify(j, newHandler, candidates[0].new, replay.VerifyOptions{})
		if err != nil || (!fixNullAmount && v.Status != replay.StatusFixed) {
			b.Fatal(v, err)
		}
	}
}
