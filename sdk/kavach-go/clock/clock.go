// Package clock provides a context-scoped deterministic clock and PRNG backed
// by Kavach RecordType::Clock (0x02) and RecordType::Rand (0x03).
package clock

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"
)

// Provider supplies wall-clock timestamps and random bytes for a Kavach step.
type Provider interface {
	Now() time.Time
	ReadRand(p []byte) (int, error)
}

type ctxKey struct{}

// WithProvider attaches p to ctx so clock.Now(ctx) and clock.Rand(ctx) route
// through it.
func WithProvider(ctx context.Context, p Provider) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the Provider attached to ctx, if any.
func FromContext(ctx context.Context) (Provider, bool) {
	if ctx == nil {
		return nil, false
	}
	p, ok := ctx.Value(ctxKey{}).(Provider)
	return p, ok && p != nil
}

// Now returns the current UTC timestamp from the step's deterministic clock
// provider in ctx, falling back to time.Now().UTC() when outside a step.
func Now(ctx context.Context) time.Time {
	if p, ok := FromContext(ctx); ok {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Read fills p with random bytes from the step's deterministic Rand provider in
// ctx, falling back to crypto/rand.Reader when outside a step.
func Read(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if prov, ok := FromContext(ctx); ok {
		return prov.ReadRand(p)
	}
	return io.ReadFull(cryptorand.Reader, p)
}

// RNG is a deterministic random generator backed by a Provider (RecordType::Rand).
// It implements io.Reader, math/rand.Source, and math/rand.Source64.
type RNG struct {
	prov Provider
}

// Rand returns a deterministic *RNG bound to ctx's Provider (or crypto/rand
// if ctx has no Provider).
func Rand(ctx context.Context) *RNG {
	p, _ := FromContext(ctx)
	return &RNG{prov: p}
}

// NewRNG returns an *RNG backed by p.
func NewRNG(p Provider) *RNG {
	return &RNG{prov: p}
}

// Read implements io.Reader.
func (r *RNG) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r != nil && r.prov != nil {
		return r.prov.ReadRand(p)
	}
	return io.ReadFull(cryptorand.Reader, p)
}

// Uint64 implements math/rand.Source64.
func (r *RNG) Uint64() uint64 {
	var b [8]byte
	_, _ = r.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// Int63 implements math/rand.Source.
func (r *RNG) Int63() int64 {
	return int64(r.Uint64() & ((1 << 63) - 1))
}

// Seed is a no-op because entropy is controlled by the Kavach step journal.
func (r *RNG) Seed(int64) {}

// Intn returns a deterministic non-negative integer in [0, n).
func (r *RNG) Intn(n int) int {
	if n <= 0 {
		panic("clock: invalid argument to Intn")
	}
	return int(r.Uint64() % uint64(n))
}

// Float64 returns a deterministic float64 in [0.0, 1.0).
func (r *RNG) Float64() float64 {
	return float64(r.Int63()>>10) / (1 << 53)
}

// Bytes returns n deterministic random bytes.
func (r *RNG) Bytes(n int) []byte {
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

// UUIDv4 generates a deterministic RFC 4122 v4 UUID backed by RecordType::Rand.
func (r *RNG) UUIDv4() string {
	var u [16]byte
	_, _ = r.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// MathRand returns a standard library *math/rand.Rand backed by this RNG.
func (r *RNG) MathRand() *rand.Rand {
	return rand.New(r)
}

// DeterministicBytes fills p deterministically from (stepSeq, counter) via
// SHA-256, matching replay.fillDeterministic in github.com/kavachlabs/kavach/replay.
func DeterministicBytes(stepSeq uint64, counter uint64, p []byte) {
	var block [24]byte
	binary.LittleEndian.PutUint64(block[0:8], stepSeq)
	binary.LittleEndian.PutUint64(block[8:16], counter)
	for i := 0; i < len(p); i += sha256.Size {
		binary.LittleEndian.PutUint64(block[16:24], uint64(i))
		sum := sha256.Sum256(block[:])
		copy(p[i:], sum[:])
	}
}

// RecordProvider wraps a live clock and CSPRNG, invoking onClock and onRand
// callbacks for every read so callers can emit RecordType::Clock and RecordType::Rand.
type RecordProvider struct {
	mu      sync.Mutex
	ClockFn func() time.Time
	RandSrc io.Reader
	OnClock func(unixNanos int64)
	OnRand  func(data []byte)
}

// Now captures the wall timestamp, invokes OnClock, and returns it in UTC.
func (p *RecordProvider) Now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn := p.ClockFn
	if fn == nil {
		fn = time.Now
	}
	ns := fn().UTC().UnixNano()
	if p.OnClock != nil {
		p.OnClock(ns)
	}
	return time.Unix(0, ns).UTC()
}

// ReadRand fills b from RandSrc, invokes OnRand with a copy, and returns len(b).
func (p *RecordProvider) ReadRand(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.RandSrc
	if src == nil {
		src = cryptorand.Reader
	}
	n, err := io.ReadFull(src, b)
	if err != nil {
		return n, err
	}
	if p.OnRand != nil {
		cp := make([]byte, n)
		copy(cp, b[:n])
		p.OnRand(cp)
	}
	return n, nil
}

// ReplayProvider replays recorded Clock timestamps and Rand byte chunks
// deterministically, synthesizing fallback values cleanly when Lenient is true.
type ReplayProvider struct {
	mu          sync.Mutex
	StepSeq     uint64
	Clocks      []int64
	Rands       [][]byte
	LastClock   int64
	Lenient     bool
	clockIdx    int
	randIdx     int
	synthesized uint64
}

// Now returns the next recorded timestamp (or LastClock when lenient).
func (p *ReplayProvider) Now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clockIdx < len(p.Clocks) {
		ns := p.Clocks[p.clockIdx]
		p.clockIdx++
		p.LastClock = ns
		return time.Unix(0, ns).UTC()
	}
	if p.Lenient {
		p.synthesized++
		return time.Unix(0, p.LastClock).UTC()
	}
	return time.Unix(0, p.LastClock).UTC()
}

// ReadRand fills b from the next recorded Rand chunk (or SHA-256 synthesis when lenient).
func (p *ReplayProvider) ReadRand(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.randIdx < len(p.Rands) && len(p.Rands[p.randIdx]) == len(b) {
		n := copy(b, p.Rands[p.randIdx])
		p.randIdx++
		return n, nil
	}
	p.synthesized++
	DeterministicBytes(p.StepSeq, p.synthesized, b)
	return len(b), nil
}

// Synthesized returns the number of synthesized clock/rand reads.
func (p *ReplayProvider) Synthesized() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.synthesized
}
