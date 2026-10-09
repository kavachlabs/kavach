package recstream

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

func TestEncoderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	in := journal.Record{Type: journal.TypeInput, Source: "kafka:t", Position: "0:1", Data: []byte("x")}
	gw := journal.Record{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: "g", Request: []byte("q"), Error: "timeout", Scope: journal.ScopeLocal}
	facts := []journal.Fact{{Key: "flag.a", Form: journal.FactValue, Value: []byte("1")}, {Key: "env.B", Form: journal.FactUnset}}
	steps := []func() error{
		func() error { return e.Open(Open{Service: "svc", Start: journal.StartGenesis, Snapshots: true}) },
		func() error { return e.Facts(facts) },
		func() error { return e.Record(in) },
		func() error { return e.Records([]journal.Record{gw}, true) },
		func() error { return e.Snapshot([]byte("state")) },
		func() error { return e.Flush(true) },
		func() error { return e.Close() },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			t.Fatal(err)
		}
	}

	r := NewReader(&buf)
	next := func(kind byte) Frame {
		t.Helper()
		f, err := r.Next()
		if err != nil || f.Kind != kind {
			t.Fatalf("frame = %+v, %v; want %s", f, err, KindName(kind))
		}
		return f
	}
	o, err := ParseOpen(next(KindOpen).Payload)
	if err != nil || o.Service != "svc" || !o.Snapshots || o.Protocol != Protocol {
		t.Fatalf("open = %+v, %v", o, err)
	}
	if o = o.Defaults(); o.Dir != DefaultDir || o.Level != 3 || o.BlockBytes != 4<<20 || o.FlushMS != 1000 || o.RetainSegments != 24 || o.SegmentSeconds != 3600 || o.SegmentBytes != 256<<20 || o.Compression != "zstd" {
		t.Fatalf("defaults = %+v", o)
	}
	if got, err := ParseFacts(next(KindFacts).Payload); err != nil || !reflect.DeepEqual(got, facts) {
		t.Fatalf("facts = %+v, %v", got, err)
	}
	if got, err := ParseRecord(next(KindRecord).Payload); err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("input = %+v, %v", got, err)
	}
	if got, err := ParseRecord(next(KindRecord).Payload); err != nil || !reflect.DeepEqual(got, gw) {
		t.Fatalf("gateway = %+v, %v", got, err)
	}
	next(KindStepEnd)
	if s, err := ParseSnapshot(next(KindSnapshot).Payload); err != nil || string(s) != "state" {
		t.Fatalf("snapshot = %q, %v", s, err)
	}
	if d, err := ParseFlush(next(KindFlush).Payload); err != nil || !d {
		t.Fatalf("flush = %v, %v", d, err)
	}
	next(KindClose)
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the last frame: %v", err)
	}
}

func TestReaderErrors(t *testing.T) {
	frame := AppendFrame(nil, KindRecord, []byte{1, 2, 3})
	if _, err := NewReader(bytes.NewReader(frame[:len(frame)-1])).Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated frame: %v", err)
	}
	if _, err := NewReader(bytes.NewReader([]byte{0})).Next(); err == nil || err == io.EOF {
		t.Fatalf("zero length: %v", err)
	}
	if _, err := NewReader(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0x7f})).Next(); err == nil {
		t.Fatal("huge length accepted")
	}
	for _, p := range [][]byte{nil, {1}} {
		if _, err := ParseRecord(p); err == nil {
			t.Errorf("ParseRecord(%v) succeeded", p)
		}
	}
	if _, err := ParseFlush([]byte{2}); err == nil {
		t.Fatal("flush byte 2 accepted")
	}
	for _, bad := range []string{`{"protocol":2,"service":"s","start":"genesis"}`, `{"protocol":1,"start":"genesis"}`, `{"protocol":1,"service":"s","start":"x"}`, `{"protocol":1,"service":"s","start":"genesis","compression":"gzip"}`, `nope`} {
		if _, err := ParseOpen([]byte(bad)); err == nil {
			t.Errorf("ParseOpen(%s) succeeded", bad)
		}
	}
}
