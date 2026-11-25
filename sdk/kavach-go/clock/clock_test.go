package clock

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestRecordAndReplayProviderDeterminism(t *testing.T) {
	fixedTime := time.Date(2026, 3, 3, 13, 3, 29, 1000000, time.UTC)
	var recClocks []int64
	var recRands [][]byte

	recProv := &RecordProvider{
		ClockFn: func() time.Time { return fixedTime },
		RandSrc: bytes.NewReader([]byte{
			0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
			0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01,
			0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09,
		}),
		OnClock: func(ns int64) { recClocks = append(recClocks, ns) },
		OnRand:  func(b []byte) { recRands = append(recRands, b) },
	}

	recCtx := WithProvider(context.Background(), recProv)
	t1 := Now(recCtx)
	rng1 := Rand(recCtx)
	u1 := rng1.UUIDv4()
	n1 := rng1.Uint64()

	if !t1.Equal(fixedTime) {
		t.Fatalf("Now = %v, want %v", t1, fixedTime)
	}
	if len(recClocks) != 1 || len(recRands) != 2 {
		t.Fatalf("recorded clocks=%d rands=%d, want 1/2", len(recClocks), len(recRands))
	}

	// Replay using recorded clocks and rands -> must produce bit-identical results.
	repProv := &ReplayProvider{
		StepSeq: 7,
		Clocks:  recClocks,
		Rands:   recRands,
		Lenient: true,
	}
	repCtx := WithProvider(context.Background(), repProv)
	t2 := Now(repCtx)
	rng2 := Rand(repCtx)
	u2 := rng2.UUIDv4()
	n2 := rng2.Uint64()

	if !t2.Equal(t1) {
		t.Fatalf("replay Now = %v, want %v", t2, t1)
	}
	if u2 != u1 {
		t.Fatalf("replay UUID = %q, want %q", u2, u1)
	}
	if n2 != n1 {
		t.Fatalf("replay Uint64 = %d, want %d", n2, n1)
	}

	// Extra lenient reads must synthesize deterministically.
	extra1 := rng2.Intn(1000)
	repProv2 := &ReplayProvider{StepSeq: 7, Clocks: recClocks, Rands: recRands, Lenient: true}
	repCtx2 := WithProvider(context.Background(), repProv2)
	_ = Now(repCtx2)
	rng3 := Rand(repCtx2)
	_ = rng3.UUIDv4()
	_ = rng3.Uint64()
	extra2 := rng3.Intn(1000)
	if extra1 != extra2 {
		t.Fatalf("synthesized reads not deterministic: %d vs %d", extra1, extra2)
	}
	if repProv.Synthesized() != 1 {
		t.Fatalf("Synthesized() = %d, want 1", repProv.Synthesized())
	}
}
