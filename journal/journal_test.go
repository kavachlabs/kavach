package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testMeta = Meta{Service: "test", Start: StartGenesis, Handler: "rev1", Producer: "kavach-go/test"}

// genesisEnv is the environment record every journal starts with.
func genesisEnv(seq uint64) Record {
	return Record{Type: TypeEnvironment, Flags: FlagCritical, Seq: seq, Facts: []Fact{
		{Key: "env.MODE", Form: FactValue, Value: []byte("test")},
		{Key: "host.os", Form: FactValue, Value: []byte("linux")},
	}}
}

func sampleRecords() []Record {
	return []Record{
		genesisEnv(0),
		{Type: TypeInput, Seq: 1, Source: "kafka:wallet", Position: "3:1042", Data: []byte(`{"amount":10}`)},
		{Type: TypeClock, Seq: 2, UnixNanos: -5},
		{Type: TypeRand, Seq: 3, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{Type: TypeOutput, Seq: 4, Sink: "postgres:balances", Data: []byte("alice=10"), Scope: ScopeLocal},
		{Type: TypeMarker, Seq: 5, Kind: MarkerPanic, Message: "boom", Data: []byte("stack")},
		{Type: TypeInput, Seq: 6, Source: "s", Position: "", Data: nil},
	}
}

func encode(t *testing.T, meta Meta, recs []Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Encode(&buf, meta, recs); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// normalize maps empty byte slices to nil so round-trips compare equal.
func normalize(recs []Record) []Record {
	out := make([]Record, len(recs))
	for i, r := range recs {
		if len(r.Data) == 0 {
			r.Data = nil
		}
		out[i] = r
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	b := encode(t, testMeta, sampleRecords())
	j, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	wantMeta := testMeta
	wantMeta.Compression = CompressionZstd
	if !reflect.DeepEqual(j.Header.Meta, wantMeta) || j.Header.Major != Major || j.Header.Minor != Minor {
		t.Fatalf("header = %+v", j.Header)
	}
	if j.Truncated {
		t.Fatal("unexpected truncation")
	}
	if !reflect.DeepEqual(normalize(j.Records), normalize(sampleRecords())) {
		t.Fatalf("records differ:\n got %+v\nwant %+v", j.Records, sampleRecords())
	}
}

func TestSnapshotStart(t *testing.T) {
	meta := testMeta
	meta.Start = StartSnapshot
	recs := []Record{
		{Type: TypeSnapshot, Flags: FlagCritical, Seq: 41, Data: []byte("state")},
		genesisEnv(42),
		{Type: TypeInput, Seq: 43, Source: "s", Data: []byte("x")},
	}
	j, err := Decode(bytes.NewReader(encode(t, meta, recs)))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Records) != 3 || j.Records[0].Seq != 41 || !j.Records[0].Critical() {
		t.Fatalf("records = %+v", j.Records)
	}
}

func TestTruncatedTail(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	recs := sampleRecords()
	for i, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			w.Flush()
		}
	}
	w.Close()
	b := buf.Bytes()
	// The second block holds the last three records.
	for cut := 1; cut < 40; cut++ {
		j, err := Decode(bytes.NewReader(b[:len(b)-cut]))
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if !j.Truncated || j.IgnoredBytes <= 0 {
			t.Fatalf("cut %d: not reported as truncated (%d bytes ignored)", cut, j.IgnoredBytes)
		}
		if len(j.Records) != 4 {
			t.Fatalf("cut %d: got %d records", cut, len(j.Records))
		}
	}
}

func TestBlocksAndCompression(t *testing.T) {
	for _, comp := range []string{CompressionZstd, CompressionNone} {
		meta := testMeta
		meta.Compression = comp
		var buf bytes.Buffer
		w, err := NewWriterOptions(&buf, meta, WriterOptions{BlockBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		recs := []Record{genesisEnv(0)}
		for i := 1; i < 200; i++ {
			recs = append(recs, Record{Type: TypeRand, Seq: uint64(i), Data: bytes.Repeat([]byte{byte(i)}, 40)})
		}
		for _, r := range recs {
			if err := w.Write(r); err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		j, err := Decode(&buf)
		if err != nil {
			t.Fatalf("%s: %v", comp, err)
		}
		if !reflect.DeepEqual(j.Records, recs) {
			t.Fatalf("%s: records differ", comp)
		}
	}
}

// TestLargeUncompressed writes a block that needs several raw zstd blocks.
func TestLargeUncompressed(t *testing.T) {
	meta := testMeta
	meta.Compression = CompressionNone
	recs := []Record{genesisEnv(0), {Type: TypeRand, Seq: 1, Data: bytes.Repeat([]byte("abc"), 200_000)}}
	var buf bytes.Buffer
	if err := Encode(&buf, meta, recs); err != nil {
		t.Fatal(err)
	}
	j, err := Decode(&buf)
	if err != nil || !reflect.DeepEqual(j.Records, recs) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestXXH64(t *testing.T) {
	// Known values of XXH64 with seed 0.
	if got := xxh64(nil); got != 0xef46db3751d8e999 {
		t.Fatalf("xxh64(\"\") = %x", got)
	}
	if got := xxh64([]byte("a")); got != 0xd24ec4f1a98c6e5b {
		t.Fatalf("xxh64(a) = %x", got)
	}
}

// TestZstdTool checks every encoding against the reference zstd tool, if installed.
func TestZstdTool(t *testing.T) {
	tool, err := exec.LookPath("zstd")
	if err != nil {
		t.Skip("zstd is not installed")
	}
	for _, comp := range []string{CompressionZstd, CompressionNone} {
		meta := testMeta
		meta.Compression = comp
		recs := append(sampleRecords(), Record{Type: TypeRand, Seq: 7, Data: bytes.Repeat([]byte("xyz"), 100_000)})
		var buf bytes.Buffer
		w, _ := NewWriterOptions(&buf, meta, WriterOptions{BlockBytes: 1})
		for _, r := range recs {
			if err := w.Write(r); err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		path := filepath.Join(t.TempDir(), "j.kavach")
		os.WriteFile(path, buf.Bytes(), 0o644)
		if out, err := exec.Command(tool, "-t", path).CombinedOutput(); err != nil {
			t.Fatalf("%s: zstd -t: %v\n%s", comp, err, out)
		}
	}
}

func TestWriterRejects(t *testing.T) {
	cases := map[string]struct {
		meta Meta
		recs []Record
	}{
		"gap":              {testMeta, []Record{genesisEnv(0), {Type: TypeClock, Seq: 2}}},
		"genesis nonzero":  {testMeta, []Record{genesisEnv(1)}},
		"input before env": {testMeta, []Record{{Type: TypeInput, Seq: 0}}},
		"env no critical":  {testMeta, []Record{{Type: TypeEnvironment, Seq: 0}}},
		"gateway critical": {testMeta, []Record{genesisEnv(0), {Type: TypeGateway, Seq: 1}}},
		"env repeats key":  {testMeta, []Record{{Type: TypeEnvironment, Flags: FlagCritical, Seq: 0, Facts: []Fact{{Key: "a"}, {Key: "a"}}}}},
		"compression":      {Meta{Service: "s", Start: StartGenesis, Compression: "gzip"}, nil},
		"genesis snapshot": {testMeta, []Record{{Type: TypeSnapshot, Seq: 0}}},
		"late snapshot":    {Meta{Service: "s", Start: StartSnapshot}, []Record{{Type: TypeSnapshot, Seq: 3}, {Type: TypeSnapshot, Seq: 4}}},
		"snapshot first":   {Meta{Service: "s", Start: StartSnapshot}, []Record{{Type: TypeInput, Seq: 3}}},
		"type zero":        {testMeta, []Record{{Type: 0, Seq: 0}}},
		"flags":            {testMeta, []Record{genesisEnv(0), {Type: TypeClock, Flags: 2, Seq: 1}}},
		"no service":       {Meta{Start: StartGenesis}, nil},
		"bad start":        {Meta{Service: "s", Start: "middle"}, nil},
	}
	for name, c := range cases {
		if err := Encode(io.Discard, c.meta, c.recs); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRecordTooLarge(t *testing.T) {
	_, err := AppendFrame(nil, Record{Type: TypeRand, Data: make([]byte, MaxBodyLen)})
	if err == nil {
		t.Fatal("expected size error")
	}
}

func TestTypeText(t *testing.T) {
	for _, ty := range []Type{TypeInput, TypeSnapshot, 0x90} {
		b, _ := ty.MarshalText()
		var back Type
		if err := back.UnmarshalText(b); err != nil || back != ty {
			t.Fatalf("%v: got %v, %v", ty, back, err)
		}
	}
	var ty Type
	if err := ty.UnmarshalText([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
	if Type(0x90).String() != "type(0x90)" || Type(0x90).Known() {
		t.Fatal("unexpected unknown-type behavior")
	}
}

func TestFileAppendReopenRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.kavach")
	jf, err := OpenFile(path, testMeta, FileOptions{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	env := genesisEnv(0)
	if seq, err := jf.Append(env); err != nil || seq != 0 {
		t.Fatalf("append env: seq %d err %v", seq, err)
	}
	for i := 1; i < 4; i++ {
		seq, err := jf.Append(Record{Type: TypeInput, Source: "s", Data: []byte{byte(i)}})
		if err != nil || seq != uint64(i) {
			t.Fatalf("append %d: seq %d err %v", i, seq, err)
		}
	}
	if err := jf.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := jf.Append(Record{Type: TypeInput}); err == nil {
		t.Fatal("append after close should fail")
	}

	// Simulate a crash mid-write: a partial block at the end.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0x5b, 0x2a, 0x4d, 0x18, 24, 0, 0, 0, 1, 2})
	f.Close()

	jf, err = OpenFile(path, testMeta, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := jf.Append(Record{Type: TypeClock, UnixNanos: 7}); err != nil || seq != 4 {
		t.Fatalf("append after reopen: seq %d err %v", seq, err)
	}
	var seqs []uint64
	if err := jf.Iterate(func(r Record) error { seqs = append(seqs, r.Seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqs, []uint64{0, 1, 2, 3, 4}) {
		t.Fatalf("seqs = %v", seqs)
	}
	stop := errors.New("stop")
	if err := jf.Iterate(func(Record) error { return stop }); err != stop {
		t.Fatalf("iterate error = %v", err)
	}
	jf.Close()

	j, err := ReadFile(path)
	if err != nil || j.Truncated || len(j.Records) != 5 {
		t.Fatalf("ReadFile: %v truncated=%v n=%d", err, j.Truncated, len(j.Records))
	}
}

func TestOpenFileRejectsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.kavach")
	if _, err := OpenFile(path, Meta{Service: "s", Start: StartSnapshot}, FileOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if err := WriteFile(path, Meta{Service: "s", Start: StartSnapshot}, []Record{{Type: TypeSnapshot, Seq: 9}}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path, testMeta, FileOptions{}); err == nil {
		t.Fatal("expected error opening a snapshot journal for append")
	}
}

func TestOpenFileCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.kavach")
	os.WriteFile(path, []byte("not a journal at all"), 0o644)
	if _, err := OpenFile(path, testMeta, FileOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}

func TestReaderErrorsMentionCause(t *testing.T) {
	b := encode(t, testMeta, sampleRecords())
	b[len(b)-1] ^= 0xff
	_, err := Decode(bytes.NewReader(b))
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("err = %v", err)
	}
}

// recordFrame builds a framed record around an arbitrary body, for malformed-input tests.
func recordFrame(body []byte) []byte {
	return append(binary.AppendUvarint(nil, uint64(len(body))), body...)
}

func rawBody(ty Type, flags uint8, seq uint64, payload []byte) []byte {
	b := []byte{byte(ty), flags}
	b = binary.LittleEndian.AppendUint64(b, seq)
	return append(b, payload...)
}
