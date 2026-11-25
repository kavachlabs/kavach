package ring

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestKVRING02HeaderLayoutExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.shm")

	const capBytes = 65536
	r, err := Create(path, capBytes)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	// Verify file permissions are 0600 (SPEC §3.6).
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 0600", perm)
	}
	if wantSize := int64(HeaderBytes + capBytes); st.Size() != wantSize {
		t.Fatalf("size = %d, want %d", st.Size(), wantSize)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(raw[OffsetMagic : OffsetMagic+8]); got != Magic {
		t.Fatalf("magic = %q, want %q", got, Magic)
	}
	if got := binary.LittleEndian.Uint64(raw[OffsetCapacity : OffsetCapacity+8]); got != capBytes {
		t.Fatalf("capacity = %d, want %d", got, capBytes)
	}
	if got := binary.LittleEndian.Uint64(raw[OffsetWrite : OffsetWrite+8]); got != 0 {
		t.Fatalf("initial write cursor = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint64(raw[OffsetRead : OffsetRead+8]); got != 0 {
		t.Fatalf("initial read cursor = %d, want 0", got)
	}
}

func TestRingWrapAroundAndDropOnFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrap.shm")

	r, err := Create(path, MinCapacity)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	chunk := bytes.Repeat([]byte("A"), 10000)
	// Publish 6 chunks (60,000 bytes), consume them, then publish 4 more to wrap past 65,536.
	for i := 0; i < 6; i++ {
		if ok, _ := r.TryPublish(chunk); !ok {
			t.Fatalf("TryPublish %d failed unexpectedly", i)
		}
	}
	// Next 10,000 byte chunk exceeds free space (5,536 left) -> must drop without blocking.
	if ok, _ := r.TryPublish(chunk); ok {
		t.Fatalf("expected drop-on-full when ring has < 10000 bytes free")
	}
	if r.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", r.Dropped())
	}

	got, err := r.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(got) != 60000 {
		t.Fatalf("polled %d bytes, want 60000", len(got))
	}

	// Now write across the wrap-around boundary (cursor 60000 -> 80000).
	wrapPayload := bytes.Repeat([]byte("BCDE"), 5000) // 20000 bytes
	if ok, _ := r.TryPublish(wrapPayload); !ok {
		t.Fatalf("wrap TryPublish failed")
	}
	got2, err := r.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce wrap: %v", err)
	}
	if !bytes.Equal(got2, wrapPayload) {
		t.Fatalf("wrap payload mismatch (len=%d)", len(got2))
	}
	if r.WriteCursor() != 80000 || r.ReadCursor() != 80000 {
		t.Fatalf("cursors write=%d read=%d, want 80000/80000", r.WriteCursor(), r.ReadCursor())
	}
}

func TestWriterRoundTripAllFrameAndRecordTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "all.shm")

	w, err := OpenPath(path, MinCapacity, OpenMeta{
		Service:  "events-processor",
		Producer: "kavach-go/0.2.0",
	})
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}

	if err := w.WriteFacts([]Fact{{Key: "host.runtime", Form: FactValue, Value: []byte("go1.23")}}); err != nil {
		t.Fatalf("WriteFacts: %v", err)
	}
	if err := w.WriteSnapshot([]byte("snap-state")); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	recs := []Record{
		{Type: RecordInput, Source: "kafka:raw_events", Position: "0:42", Data: []byte(`{"tx":"1"}`)},
		{Type: RecordClock, UnixNanos: 1741007009001000000},
		{Type: RecordRand, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{Type: RecordGateway, Flags: FlagCritical, Gateway: "redis:zadd", Request: []byte(`{"key":"k"}`), Response: []byte(`1`), Scope: ScopeRemote},
		{Type: RecordConfig, Flags: FlagCritical, Key: "FF_BILL", Present: true, Value: []byte("1"), Source: "env"},
		{Type: RecordOutput, Sink: "kafka:events_enriched", Data: []byte(`{"tx":"1"}`), Scope: ScopeRemote},
		{Type: RecordMarker, Flags: FlagCritical, Kind: MarkerError, Message: "transient", Data: []byte("stack")},
	}
	if err := w.WriteStep(recs, true); err != nil {
		t.Fatalf("WriteStep: %v", err)
	}
	if err := w.WriteFlush(true); err != nil {
		t.Fatalf("WriteFlush: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Open as an independent reader (simulating C++ ShmRingConsumer::open_path).
	consumer, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer consumer.Close()

	raw, err := consumer.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	var dec FrameDecoder
	if _, err := dec.Feed(raw); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	res := dec.FinishEOF()
	if !res.CleanClose {
		t.Fatalf("expected CleanClose=true, got false (synthesized=%v)", res.SynthesizedMarker)
	}
	if res.DiscardedTornBytes != 0 {
		t.Fatalf("DiscardedTornBytes=%d, want 0", res.DiscardedTornBytes)
	}
	// Open + Facts + Snapshot + 7 records + StepEnd + Flush + Close = 13 frames
	if len(res.Frames) != 13 {
		t.Fatalf("decoded %d frames, want 13", len(res.Frames))
	}
	if res.Frames[0].Kind != FrameOpen || res.Frames[1].Kind != FrameFacts || res.Frames[2].Kind != FrameSnapshot {
		t.Fatalf("unexpected header frames: %v, %v, %v", res.Frames[0].Kind, res.Frames[1].Kind, res.Frames[2].Kind)
	}
	if res.Frames[10].Kind != FrameStepEnd || res.Frames[11].Kind != FrameFlush || res.Frames[12].Kind != FrameClose {
		t.Fatalf("unexpected tail frames: %v, %v, %v", res.Frames[10].Kind, res.Frames[11].Kind, res.Frames[12].Kind)
	}

	for i, want := range recs {
		f := res.Frames[3+i]
		if f.Kind != FrameRecord {
			t.Fatalf("frame %d kind=%v, want FrameRecord", 3+i, f.Kind)
		}
		got, err := DecodeRecord(f.Payload)
		if err != nil {
			t.Fatalf("DecodeRecord(%d): %v", i, err)
		}
		if got.Type != want.Type || got.Flags != want.Flags {
			t.Fatalf("rec %d header mismatch: got (%v,%d) want (%v,%d)", i, got.Type, got.Flags, want.Type, want.Flags)
		}
		if got.Gateway != want.Gateway || got.Sink != want.Sink || got.UnixNanos != want.UnixNanos || got.Kind != want.Kind || got.Key != want.Key {
			t.Fatalf("rec %d fields mismatch: got %+v want %+v", i, got, want)
		}
	}
}

func TestStreamFallbackWriter(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewStreamWriter(&buf, OpenMeta{Service: "dev-svc"})
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := w.WriteStep([]Record{
		{Type: RecordInput, Source: "http:GET /ping", Data: []byte("hi")},
		{Type: RecordOutput, Sink: "http:response", Data: []byte("pong")},
	}, true); err != nil {
		t.Fatalf("WriteStep: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var dec FrameDecoder
	if _, err := dec.Feed(buf.Bytes()); err != nil {
		t.Fatalf("Feed stream: %v", err)
	}
	res := dec.FinishEOF()
	if !res.CleanClose || len(res.Frames) != 5 {
		t.Fatalf("stream decode mismatch: clean=%v frames=%d", res.CleanClose, len(res.Frames))
	}
}

func TestDecoderCrashSynthesisOnEOF(t *testing.T) {
	var buf []byte
	buf, _ = AppendOpenFrame(buf, OpenMeta{Service: "crash-svc"})
	buf, _ = AppendRecordFrame(buf, Record{Type: RecordInput, Source: "kafka:t", Data: []byte("x")})
	// Simulate sudden process crash before StepEnd + trailing torn bytes.
	buf = append(buf, 0x80)

	var dec FrameDecoder
	if _, err := dec.Feed(buf); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	res := dec.FinishEOF()
	if res.CleanClose {
		t.Fatalf("expected CleanClose=false")
	}
	if res.DiscardedTornBytes != 1 {
		t.Fatalf("DiscardedTornBytes=%d, want 1", res.DiscardedTornBytes)
	}
	if res.SynthesizedMarker == nil || res.SynthesizedMarker.Kind != MarkerCrash {
		t.Fatalf("expected synthesized crash marker, got %+v", res.SynthesizedMarker)
	}
}

func BenchmarkRingTryPublish(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.shm")
	r, err := Create(path, DefaultCapacity)
	if err != nil {
		b.Fatalf("Create: %v", err)
	}
	defer r.Close()

	frame, err := AppendRecordFrame(nil, Record{
		Type:     RecordGateway,
		Flags:    FlagCritical,
		Gateway:  "redis:zadd",
		Request:  []byte(`{"driver":"redis","normalized_op":"ZADD","key_args":["sub_refresh"]}`),
		Response: []byte(`1`),
		Scope:    ScopeRemote,
	})
	if err != nil {
		b.Fatalf("AppendRecordFrame: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, used := r.TryPublish(frame); !ok || used > DefaultCapacity/2 {
			// Advance read cursor directly to simulate daemon draining the ring.
			r.read.Store(r.write.Load())
		}
	}
}

func BenchmarkRingWriterWriteRecord(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench_writer.shm")
	r, err := Create(path, DefaultCapacity)
	if err != nil {
		b.Fatalf("Create: %v", err)
	}
	w, err := NewRingWriter(r, OpenMeta{Service: "bench"})
	if err != nil {
		b.Fatalf("NewRingWriter: %v", err)
	}
	defer w.Close()

	rec := Record{
		Type:      RecordClock,
		UnixNanos: 1741007009001000000,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.WriteRecord(rec)
		if i&1023 == 0 {
			r.read.Store(r.write.Load())
		}
	}
}
