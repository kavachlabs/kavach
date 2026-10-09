package bench

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// TestBenchmark records each planted bug, then checks three builds against the
// fixture with replay.Verify: the buggy build must reproduce the recorded
// failure, the correct fix must be verified as fixed, and the narrow fix is
// run to see whether variants reject it. Run with -v for the table.
func TestBenchmark(t *testing.T) {
	var correct, rejected, reproduced, variants, reproducing int
	var rows []string
	bugs := Bugs()
	for _, b := range bugs {
		t.Run(b.Name, func(t *testing.T) {
			path, err := Record(b, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			j, err := journal.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			old, err := replay.Run(j, func() kavach.Handler { return b.New(Buggy) })
			if err != nil {
				t.Fatal(err)
			}
			wantKind := map[string]replay.Status{"panic": replay.StatusStillFailing, "error": replay.StatusStillFailing, "invariant": replay.StatusInvariantViolated}[b.Class]
			if old.Status != wantKind {
				t.Fatalf("buggy build: %s (%s), want %s", old, old.Detail, wantKind)
			}
			reproduced++

			verify := func(m Mode) *replay.Verification {
				v, err := replay.Verify(j, func() kavach.Handler { return b.New(Buggy) }, func() kavach.Handler { return b.New(m) }, replay.VerifyOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			fixed, narrow := verify(Fixed), verify(Narrow)
			variants += fixed.Candidates
			reproducing += fixed.Reproducing
			if fixed.Status == replay.StatusFixed {
				correct++
			}
			if !narrow.Passed() {
				rejected++
			}
			rows = append(rows, fmt.Sprintf("%-22s %-9s old=%-34s fixed=%-14s (%2d/%2d variants reproduce) narrow=%s",
				b.Name, b.Class, old, fixed, fixed.Reproducing, fixed.Candidates, narrow))
			t.Logf("%s", rows[len(rows)-1])
			if fixed.Status != replay.StatusFixed {
				t.Errorf("correct fix: %s: %s", fixed, fixed.Detail)
			}
			if !narrow.Passed() && narrow.Status != replay.StatusVariantFailed {
				t.Errorf("narrow fix: %s, want variant_failed", narrow)
			}
		})
	}
	t.Logf("\n%s", strings.Join(rows, "\n"))
	t.Logf("bugs=%d reproduced=%d correct fixes verified=%d narrow fixes rejected=%d variants=%d reproducing=%d",
		len(bugs), reproduced, correct, rejected, variants, reproducing)
}
