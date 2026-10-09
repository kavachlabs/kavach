package replay_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// The same crash, recorded over the pipe and over rings of two sizes (SPEC.md
// §10.7), replays the same: the transport does not show in the journal. A
// 64 KiB ring is filled many times over, and a 100 KiB input is larger than a
// whole ring, so both the wait for space and the split frame run.
func TestTransportsRecordTheSame(t *testing.T) {
	big := strings.Repeat("a", 100<<10)
	for _, tc := range []struct {
		name   string
		noRing bool
		ring   int
	}{{"pipe", true, 0}, {"ring", false, 0}, {"small ring", false, 64 << 10}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions(t)
			opts.NoRing, opts.RingBytes = tc.noRing, tc.ring
			var delivered int
			opts.Deliver = func(outs []kavach.Output) error { delivered += len(outs); return nil }
			r := newRecorder(t, newWallet(), opts)
			inputs := []string{"alice:10"}
			for i := 0; i < 300; i++ {
				inputs = append(inputs, fmt.Sprintf("bob:%d", i))
			}
			inputs = append(inputs, big+":1", "alice:7", "bob:null")
			err := feed(t, r, inputs...)
			var pe *kavach.PanicError
			if !errors.As(err, &pe) {
				t.Fatalf("expected a panic, got %v", err)
			}
			path := fixtureIn(t, r, opts.Dir)
			res, err := replay.RunFile(path, newWallet)
			if err != nil || res.Status != replay.StatusStillFailing {
				t.Fatalf("buggy build: %v %v", res, err)
			}
			res, err = replay.RunFile(path, fixedWallet)
			if err != nil || res.Status != replay.StatusFixed {
				t.Fatalf("fixed build: %v %v (%s)", res, err, res.Detail)
			}
			if delivered != 303 {
				t.Fatalf("delivered %d outputs", delivered)
			}
		})
	}
}
