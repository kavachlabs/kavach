//go:build unix

package recstream

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

func newTestRing(t *testing.T) (*Ring, *RingReader) {
	t.Helper()
	g, f, err := CreateRing(t.TempDir(), MinRing)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close(); f.Close() })
	return g, g.NewReader()
}

func TestRingFramesWrapAround(t *testing.T) {
	g, rr := newTestRing(t)
	rd := NewReader(rr)
	rec := journal.Record{Type: journal.TypeOutput, Sink: "s", Data: bytes.Repeat([]byte("x"), 1000)}
	// Enough frames to go round the ring several times, a few at a time.
	for round := 0; round < 200; round++ {
		var buf []byte
		for i := 0; i < 3; i++ {
			buf, _ = AppendRecordFrame(buf, rec)
		}
		if n, _ := g.TryPublish(buf); n != len(buf) {
			t.Fatalf("round %d: published %d of %d", round, n, len(buf))
		}
		for i := 0; i < 3; i++ {
			f, err := rd.Next()
			if err != nil || f.Kind != KindRecord {
				t.Fatalf("round %d: %v %v", round, f.Kind, err)
			}
		}
	}
}

func TestRingFull(t *testing.T) {
	g, rr := newTestRing(t)
	chunk := make([]byte, 20<<10)
	for i := 0; i < 3; i++ {
		if n, _ := g.TryPublish(chunk); n != len(chunk) {
			t.Fatalf("chunk %d: published %d", i, n)
		}
	}
	if n, used := g.TryPublish(chunk); n != 0 || used != 60<<10 {
		t.Fatalf("a full ring took %d bytes, used = %d", n, used)
	}
	buf := make([]byte, 30<<10)
	if _, err := io.ReadFull(rr, buf); err != nil {
		t.Fatal(err)
	}
	if n, _ := g.TryPublish(chunk); n != len(chunk) {
		t.Fatalf("after reading, published %d", n)
	}
}

func TestRingFrameLargerThanRing(t *testing.T) {
	g, rr := newTestRing(t)
	big, _ := AppendRecordFrame(nil, journal.Record{Type: journal.TypeRand, Data: bytes.Repeat([]byte{7}, 3*MinRing)})
	go func() {
		for len(big) > 0 {
			n, _ := g.TryPublish(big)
			big = big[n:]
			if n == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	f, err := NewReader(rr).Next()
	if err != nil || len(f.Payload) < 3*MinRing {
		t.Fatalf("frame of %d bytes, err %v", len(f.Payload), err)
	}
}

func TestRingReaderEnd(t *testing.T) {
	g, rr := newTestRing(t)
	g.TryPublish([]byte("tail"))
	rr.End()
	got, err := io.ReadAll(rr)
	if err != nil || string(got) != "tail" {
		t.Fatalf("drained %q, %v", got, err)
	}
}

func TestRingReaderRejectsBadHeader(t *testing.T) {
	g, rr := newTestRing(t)
	g.TryPublish(make([]byte, 100))
	g.read.Store(0)
	if _, err := io.ReadFull(rr, make([]byte, 50)); err != nil {
		t.Fatal(err)
	}
	g.write.Store(10)
	if _, err := rr.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("write going backwards: %v", err)
	}
	g.write.Store(rr.pos + g.cap + 1)
	if _, err := rr.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "unread") {
		t.Fatalf("write too far ahead: %v", err)
	}
}

func TestOpenRingValidates(t *testing.T) {
	g, f, err := CreateRing(t.TempDir(), MinRing)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer g.Close()
	if r, err := OpenRing(f, MinRing); err != nil {
		t.Fatal(err)
	} else {
		r.Close()
	}
	if _, err := OpenRing(f, 2*MinRing); err == nil {
		t.Error("a ring file smaller than the open frame says was accepted")
	}
	copy(g.mem, "NOTARING")
	if _, err := OpenRing(f, MinRing); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Errorf("bad magic: %v", err)
	}
	copy(g.mem, ringMagic)
	g.mem[offCapacity] ^= 1
	if _, err := OpenRing(f, MinRing); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("bad capacity: %v", err)
	}
}

func TestAppendRecordFrameLongRecord(t *testing.T) {
	for _, n := range []int{0, 100, 127, 128, 200, 20000, 300000} {
		rec := journal.Record{Type: journal.TypeRand, Data: bytes.Repeat([]byte{1}, n)}
		buf, err := AppendRecordFrame([]byte("pre"), rec)
		if err != nil {
			t.Fatal(err)
		}
		f, err := NewReader(bytes.NewReader(buf[3:])).Next()
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		got, err := ParseRecord(f.Payload)
		if err != nil || len(got.Data) != n {
			t.Fatalf("%d bytes: got %d, %v", n, len(got.Data), err)
		}
	}
}
