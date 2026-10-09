package journal

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// TestAsyncWriterAndEncodedRecords writes records through an async Writer fed
// pre-encoded payloads and expects the journal that Write would have made.
func TestAsyncWriterAndEncodedRecords(t *testing.T) {
	recs := []Record{genesisEnv(0)}
	for i := 1; i < 300; i++ {
		recs = append(recs, Record{Type: TypeOutput, Seq: uint64(i), Sink: "s", Data: bytes.Repeat([]byte{byte(i)}, 50)})
	}
	var buf bytes.Buffer
	w, err := NewWriterOptions(&buf, testMeta, WriterOptions{BlockBytes: 256, Async: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Type == TypeEnvironment {
			err = w.Write(r)
			continue
		}
		var p []byte
		if p, err = AppendPayload(nil, r); err == nil {
			if err = ValidatePayload(r.Type, r.Flags, p); err == nil {
				err = w.WriteEncoded(r.Seq, r.Type, r.Flags, p)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(j.Records, recs) {
		t.Fatalf("got %d records, want %d, or they differ", len(j.Records), len(recs))
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n--; f.n < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestAsyncWriterReportsWriteErrors(t *testing.T) {
	w, err := NewWriterOptions(&failingWriter{n: 1}, testMeta, WriterOptions{BlockBytes: 64, Async: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(genesisEnv(0)); err != nil {
		t.Fatal(err)
	}
	var werr error
	for i := 1; i < 50 && werr == nil; i++ {
		werr = w.Write(Record{Type: TypeRand, Seq: uint64(i), Data: bytes.Repeat([]byte{1}, 60)})
		if werr == nil {
			werr = w.Flush()
		}
	}
	if werr == nil || !strings.Contains(werr.Error(), "disk full") {
		t.Fatalf("write error = %v", werr)
	}
	w.Close()
}

func TestWriteEncodedChecksOrder(t *testing.T) {
	w, err := NewWriter(io.Discard, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEncoded(0, TypeInput, 0, []byte{0, 0, 0}); err == nil {
		t.Error("an input before the genesis environment was accepted")
	}
	w, _ = NewWriter(io.Discard, testMeta)
	if err := w.Write(genesisEnv(0)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEncoded(5, TypeClock, 0, make([]byte, 8)); err == nil {
		t.Error("a sequence gap was accepted")
	}
}

func TestValidatePayload(t *testing.T) {
	good, _ := AppendPayload(nil, Record{Type: TypeInput, Source: "s", Position: "p", Data: []byte("d")})
	if err := ValidatePayload(TypeInput, 0, good); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayload(TypeInput, 0, good[:len(good)-1]); err == nil {
		t.Error("a short payload was accepted")
	}
	if err := ValidatePayload(TypeInput, 0, append([]byte{1, 0xff}, good[2:]...)); err == nil {
		t.Error("a string that is not UTF-8 was accepted")
	}
	if err := ValidatePayload(0, 0, nil); err == nil {
		t.Error("type 0 was accepted")
	}
	if err := ValidatePayload(TypeClock, 0x80, make([]byte, 8)); err == nil {
		t.Error("reserved flag bits were accepted")
	}
}
