package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testMeta = Meta{Service: "test", Start: StartGenesis, Handler: "rev1", Producer: "kavach-go/test"}

func sampleRecords() []Record {
	return []Record{
		{Type: TypeInput, Seq: 0, Source: "kafka:wallet", Position: "3:1042", Data: []byte(`{"amount":10}`)},
		{Type: TypeClock, Seq: 1, UnixNanos: -5},
		{Type: TypeRand, Seq: 2, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{Type: TypeOutput, Seq: 3, Sink: "postgres:balances", Data: []byte("alice=10")},
		{Type: TypeMarker, Seq: 4, Kind: MarkerPanic, Message: "boom", Data: []byte("stack")},
		{Type: TypeInput, Seq: 5, Source: "s", Position: "", Data: nil},
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
	if j.Header.Meta != testMeta || j.Header.Major != Major || j.Header.Minor != Minor {
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
		{Type: TypeInput, Seq: 42, Source: "s", Data: []byte("x")},
	}
	j, err := Decode(bytes.NewReader(encode(t, meta, recs)))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Records) != 2 || j.Records[0].Seq != 41 || !j.Records[0].Critical() {
		t.Fatalf("records = %+v", j.Records)
	}
}

func TestTruncatedTail(t *testing.T) {
	b := encode(t, testMeta, sampleRecords())
	full, _ := Decode(bytes.NewReader(b))
	for cut := 1; cut < 20; cut++ {
		j, err := Decode(bytes.NewReader(b[:len(b)-cut]))
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if !j.Truncated {
			t.Fatalf("cut %d: not reported as truncated", cut)
		}
		if len(j.Records) != len(full.Records)-1 {
			t.Fatalf("cut %d: got %d records", cut, len(j.Records))
		}
	}
}

func TestWriterRejects(t *testing.T) {
	cases := map[string]struct {
		meta Meta
		recs []Record
	}{
		"gap":              {testMeta, []Record{{Type: TypeClock, Seq: 0}, {Type: TypeClock, Seq: 2}}},
		"genesis nonzero":  {testMeta, []Record{{Type: TypeClock, Seq: 1}}},
		"genesis snapshot": {testMeta, []Record{{Type: TypeSnapshot, Seq: 0}}},
		"late snapshot":    {Meta{Service: "s", Start: StartSnapshot}, []Record{{Type: TypeSnapshot, Seq: 3}, {Type: TypeSnapshot, Seq: 4}}},
		"snapshot first":   {Meta{Service: "s", Start: StartSnapshot}, []Record{{Type: TypeInput, Seq: 3}}},
		"type zero":        {testMeta, []Record{{Type: 0, Seq: 0}}},
		"flags":            {testMeta, []Record{{Type: TypeClock, Flags: 2, Seq: 0}}},
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
	for i := 0; i < 3; i++ {
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

	// Simulate a crash mid-write: a partial frame at the end.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{40, 0, 0, 0, 1, 2})
	f.Close()

	jf, err = OpenFile(path, testMeta, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := jf.Append(Record{Type: TypeClock, UnixNanos: 7}); err != nil || seq != 3 {
		t.Fatalf("append after reopen: seq %d err %v", seq, err)
	}
	var seqs []uint64
	if err := jf.Iterate(func(r Record) error { seqs = append(seqs, r.Seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqs, []uint64{0, 1, 2, 3}) {
		t.Fatalf("seqs = %v", seqs)
	}
	stop := errors.New("stop")
	if err := jf.Iterate(func(Record) error { return stop }); err != stop {
		t.Fatalf("iterate error = %v", err)
	}
	jf.Close()

	j, err := ReadFile(path)
	if err != nil || j.Truncated || len(j.Records) != 4 {
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
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
}

// rawFrame builds a frame around an arbitrary body, for malformed-input tests.
func rawFrame(body []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(body)))
	out = append(out, body...)
	return binary.LittleEndian.AppendUint32(out, checksum(body))
}

func rawBody(ty Type, flags uint8, seq uint64, payload []byte) []byte {
	b := []byte{byte(ty), flags}
	b = binary.LittleEndian.AppendUint64(b, seq)
	return append(b, payload...)
}
