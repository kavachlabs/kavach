package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"strconv"
	"testing"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

// This file is the recorder half of the conformance suite (SPEC.md §10.6). The
// cases live in spec/recorder/journal and are described in spec/recorder/README.md.
// `go test ./cmd/kavach-recorder -update` regenerates them from the builders below:
// the streams and facts are written as they are defined here, and the expected
// output is what the recorder produces, to be reviewed.

var update = flag.Bool("update", false, "regenerate spec/recorder/journal")

const casesDir = "../../spec/recorder/journal"

// jf is a frame as it appears in stream.json.
type jf map[string]any

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

func val(s string) map[string]any { return map[string]any{"value": b64(s)} }

func fOpen(extra map[string]any) jf {
	o := map[string]any{"protocol": 1, "service": "conformance", "start": "genesis", "producer": "kavach-go/conformance"}
	for k, v := range extra {
		o[k] = v
	}
	return jf{"frame": "open", "open": o}
}

func fFacts(facts map[string]any) jf { return jf{"frame": "facts", "facts": facts} }
func fStepEnd() jf                   { return jf{"frame": "step_end"} }
func fClose() jf                     { return jf{"frame": "close"} }
func fFlush(durable bool) jf         { return jf{"frame": "flush", "durable": durable} }
func fSnapshot(s string) jf          { return jf{"frame": "snapshot", "data": b64(s)} }

func rec(typ string, critical bool, fields map[string]any) jf {
	f := jf{"frame": "record", "type": typ, "critical": critical}
	for k, v := range fields {
		f[k] = v
	}
	return f
}

func fInput(pos, data string) jf {
	return rec("input", false, map[string]any{"source": "test", "position": pos, "data": b64(data)})
}
func fClock(n int64) jf {
	return rec("clock", false, map[string]any{"unix_nanos": strconv.FormatInt(n, 10)})
}
func fRand(s string) jf { return rec("rand", false, map[string]any{"data": b64(s)}) }
func fOutput(sink, data, scope string) jf {
	return rec("output", false, map[string]any{"sink": sink, "data": b64(data), "scope": scope})
}
func fGateway(name, req, resp, errMsg, scope string) jf {
	return rec("gateway", true, map[string]any{"gateway": name, "request": b64(req), "response": b64(resp), "error": errMsg, "scope": scope})
}
func fConfig(key string, present bool, value, source string) jf {
	return rec("config", true, map[string]any{"key": key, "present": present, "value": b64(value), "source": source})
}
func fMarker(kind, msg, data string) jf {
	return rec("marker", false, map[string]any{"kind": kind, "message": msg, "data": b64(data)})
}

// step is a complete, ordinary step.
func step(pos, data string) []jf {
	return []jf{fInput(pos, data), fClock(1_000 + int64(len(pos))), fOutput("sink", "out-"+data, "remote"), fStepEnd()}
}

func cat(parts ...[]jf) []jf {
	var out []jf
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type testCase struct {
	name, desc string
	frames     []jf
}

func baseFacts() map[string]any {
	return map[string]any{
		"env.MAX_TRANSFER":      val("1000"),
		"env.API_TOKEN":         map[string]any{"sha256": sha("s3cret")},
		"host.arch":             val("amd64"),
		"host.kernel":           val("6.8.0-45-generic"),
		"host.mount./dev/shm":   val("tmpfs rw,nosuid,nodev"),
		"host.os":               val("linux"),
		"host.sessions.wfinfra": val("1"),
	}
}

func cases() []testCase {
	failing0 := func(kind, msg string) []jf {
		return []jf{fInput("f", "bad"), fClock(5), fMarker(kind, msg, ""), fStepEnd()}
	}
	failing := func(kind, msg, data string) []jf {
		return []jf{fInput("f", "bad"), fClock(5), fMarker(kind, msg, data), fStepEnd()}
	}
	return []testCase{
		{"genesis-basic", "Every record type, in a genesis journal that closes cleanly.", cat(
			[]jf{fOpen(nil), fFacts(map[string]any{"host.runtime": val("go1.24.7"), "flag.ledger-v2": val("true")})},
			[]jf{
				fInput("0", "[1]"), fClock(1759752000000000000), fRand("\x01\x02\xff"),
				fGateway("fx-rates", "EURUSD", "1.07", "", "remote"),
				fGateway("fx-rates", "GBPUSD", "", "timeout", "remote"),
				fGateway("shm:/dev/shm/orderbook", "read", "rows", "", "local"),
				fConfig("flag.ledger-v2", true, "true", "launchdarkly"),
				fConfig("MAX_TRANSFER", false, "", "env"),
				fOutput("postgres:balances", "alice=10", "remote"),
				fOutput("file:/tmp/audit", "line", "local"),
				fStepEnd(),
			},
			step("1", "[2]"),
			[]jf{fClose()},
		)},
		{"genesis-none", "Uncompressed data frames.", cat(
			[]jf{fOpen(map[string]any{"compression": "none"})}, step("0", "a"), step("1", "b"), []jf{fClose()},
		)},
		{"genesis-level", "A compression level and a tiny block size make many blocks.", cat(
			[]jf{fOpen(map[string]any{"level": 9, "block_bytes": 1})}, step("0", "a"), step("1", "b"), step("2", "c"), []jf{fClose()},
		)},
		{"close-empty", "Closing before any record writes no journal.", []jf{fOpen(nil), fClose()}},
		{"facts-merge", "Facts before the first input join the genesis environment. Facts between steps become a change record after the next step_end, and only if they differ.", cat(
			[]jf{fOpen(nil), fFacts(map[string]any{"host.runtime": val("go1.24.7"), "flag.a": val("1"), "host.x.extra": val("v")})},
			step("0", "a"),
			[]jf{
				fFacts(map[string]any{"flag.a": val("2"), "host.runtime": val("go1.24.7"), "host.x.extra": map[string]any{"unset": true}, "flag.never-set": map[string]any{"unset": true}}),
			},
			step("1", "b"),
			[]jf{fFacts(map[string]any{"flag.a": map[string]any{"sha256": sha("2")}})},
			step("2", "c"),
			[]jf{fClose()},
		)},
		{"flush", "flush frames close the open block; a durable one is answered.", cat(
			[]jf{fOpen(nil)}, step("0", "a"), []jf{fFlush(false), fFlush(true)}, step("1", "b"), []jf{fFlush(true), fClose()},
		)},
		{"flush-durable-twice", "Each durable flush is answered by exactly one durable message, in order, and nothing else is: not a non-durable flush, not a failure's sync, not close.", cat(
			[]jf{fOpen(nil), fFlush(true), fFlush(true)}, step("0", "a"),
			failing0("panic", "boom"),
			[]jf{fFlush(false), fFlush(true), fFlush(true), fClose()},
		)},
		{"markers-between-steps", "Trigger and dropped markers between steps.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fMarker("trigger", "manual", ""), fMarker("dropped", "12", "")},
			step("1", "b"), []jf{fClose()},
		)},
		{"snapshot-start", "A journal that starts from a snapshot.", cat(
			[]jf{fOpen(map[string]any{"start": "snapshot", "snapshots": true}), fFacts(map[string]any{"host.runtime": val("go1.24.7")}), fSnapshot(`{"alice":10}`)},
			step("0", "a"), []jf{fClose()},
		)},
		{"segment-change", "The recorder asks for a snapshot when a segment is full and starts the next one with it.", cat(
			[]jf{fOpen(map[string]any{"start": "snapshot", "snapshots": true, "segment_bytes": 1}), fSnapshot("s0")},
			step("0", "a"),
			[]jf{fFacts(map[string]any{"flag.a": val("on")}), fSnapshot("s1")},
			step("1", "b"),
			[]jf{fSnapshot("s2")},
			step("2", "c"),
			[]jf{fClose()},
		)},
		{"segment-genesis", "A genesis journal's first segment has no snapshot; the next one does.", cat(
			[]jf{fOpen(map[string]any{"snapshots": true, "segment_bytes": 1})},
			step("0", "a"), []jf{fSnapshot("s1")}, step("1", "b"), []jf{fClose()},
		)},
		{"segment-retention", "Segments beyond retain_segments are deleted, oldest first; fixtures never are.", cat(
			[]jf{fOpen(map[string]any{"snapshots": true, "segment_bytes": 1, "retain_segments": 2})},
			failing("panic", "boom", "stack"),
			[]jf{fSnapshot("s1")}, step("1", "b"),
			[]jf{fSnapshot("s2")}, step("2", "c"),
			[]jf{fSnapshot("s3")}, step("3", "d"),
			[]jf{fClose()},
		)},
		{"fixture-failures", "A panic, an error and an invariant each write a fixture after their step_end.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			failing("panic", "nil pointer", "goroutine 1 [running]"), step("1", "b"),
			failing("error", "insufficient funds", ""),
			failing("invariant", "balance_non_negative", "alice=-5"),
			[]jf{fClose()},
		)},
		{"fixture-after-segment", "A fixture is cut from the segment that holds the failure, starting at its snapshot.", cat(
			[]jf{fOpen(map[string]any{"snapshots": true, "segment_bytes": 1})},
			step("0", "a"), []jf{fSnapshot("s1")}, step("1", "b"),
			failing("panic", "late", "trace"),
			[]jf{fClose()},
		)},
		{"end-eof-between-steps", "The stream ends between steps without close: an exit marker.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
		)},
		{"end-eof-mid-step", "The stream ends inside a step: a crash marker and a fixture.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fInput("1", "dies"), fClock(9), fOutput("sink", "partial", "remote")},
		)},
		{"end-eof-mid-step-marker", "The stream ends after a step's own marker but before step_end: the marker stands, with a fixture.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fInput("1", "bad"), fMarker("panic", "boom", "")},
		)},
		{"end-eof-before-journal", "The stream ends after open, before any record: nothing is written.", []jf{fOpen(nil)}},
		{"fatal-facts-in-step", "A facts frame inside a step is fatal; the incomplete step is not in the journal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fInput("1", "b"), fClock(2), fFacts(map[string]any{"flag.a": val("1")}), fStepEnd()},
		)},
		{"fatal-input-in-step", "An input inside a step, which has no step_end, is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fInput("1", "b"), fInput("2", "c"), fStepEnd()},
		)},
		{"fatal-step-end-outside", "step_end outside a step is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"), []jf{fStepEnd()},
		)},
		{"fatal-read-between-steps", "A clock record between steps is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"), []jf{fClock(3)},
		)},
		{"fatal-record-after-marker", "A record after a step's marker is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"),
			[]jf{fInput("1", "b"), fMarker("panic", "boom", ""), fClock(1), fStepEnd()},
		)},
		{"fatal-unrequested-snapshot", "A snapshot the recorder did not ask for is fatal.", cat(
			[]jf{fOpen(map[string]any{"snapshots": true})}, step("0", "a"), []jf{fSnapshot("s")},
		)},
		{"fatal-environment-record", "The SDK never sends environment records.", cat(
			[]jf{fOpen(nil), rec("environment", true, nil)},
		)},
		{"fatal-unknown-kind", "An unknown frame kind is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"), []jf{{"frame": "raw", "raw_kind": 99, "payload": ""}},
		)},
		{"fatal-malformed-payload", "A malformed payload is fatal.", cat(
			[]jf{fOpen(nil)}, step("0", "a"), []jf{{"frame": "raw", "raw_kind": 2, "payload": b64("\x01")}},
		)},
		{"fatal-no-open", "The first frame must be open.", []jf{fStepEnd()}},
		{"fatal-bad-open", "An open object with an unsupported protocol is fatal.", []jf{fOpen(map[string]any{"protocol": 2})}},
		{"fatal-snapshot-first", "With start \"snapshot\", the first frame after open must be a snapshot.", cat(
			[]jf{fOpen(map[string]any{"start": "snapshot"})}, step("0", "a"),
		)},
	}
}

// frameJSON is the decoded form of a stream.json frame.
type frameJSON struct {
	Frame    string                       `json:"frame"`
	Open     map[string]any               `json:"open"`
	Facts    map[string]envfacts.JSONFact `json:"facts"`
	Type     string                       `json:"type"`
	Critical bool                         `json:"critical"`
	Source   string                       `json:"source"`
	Position string                       `json:"position"`
	Sink     string                       `json:"sink"`
	Kind     string                       `json:"kind"`
	RawKind  int                          `json:"raw_kind"`
	Message  string                       `json:"message"`
	Nanos    string                       `json:"unix_nanos"`
	Data     []byte                       `json:"data"`
	Gateway  string                       `json:"gateway"`
	Request  []byte                       `json:"request"`
	Response []byte                       `json:"response"`
	Error    string                       `json:"error"`
	Scope    string                       `json:"scope"`
	Key      string                       `json:"key"`
	Present  bool                         `json:"present"`
	Value    []byte                       `json:"value"`
	Durable  bool                         `json:"durable"`
	Payload  []byte                       `json:"payload"`
}

// encodeFrames turns stream.json into the bytes of a record stream, with the
// open object's dir set to dir.
func encodeFrames(t *testing.T, raw []byte, dir string) []byte {
	t.Helper()
	var s struct {
		Frames []json.RawMessage `json:"frames"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, rm := range s.Frames {
		dec := json.NewDecoder(bytes.NewReader(rm))
		var f frameJSON
		if err := dec.Decode(&f); err != nil {
			t.Fatal(err)
		}
		switch f.Frame {
		case "open":
			f.Open["dir"] = dir
			b, _ := json.Marshal(f.Open)
			out = recstream.AppendFrame(out, recstream.KindOpen, b)
		case "facts":
			facts, err := envfacts.FromJSON(f.Facts)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := journal.AppendPayload(nil, journal.Record{Type: journal.TypeEnvironment, Facts: facts})
			out = recstream.AppendFrame(out, recstream.KindFacts, p)
		case "record":
			r := journal.Record{Source: f.Source, Position: f.Position, Sink: f.Sink, Kind: f.Kind, Message: f.Message,
				Data: f.Data, Gateway: f.Gateway, Request: f.Request, Response: f.Response, Error: f.Error,
				Key: f.Key, Present: f.Present, Value: f.Value}
			if f.Critical {
				r.Flags = journal.FlagCritical
			}
			if f.Nanos != "" {
				n, err := strconv.ParseInt(f.Nanos, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				r.UnixNanos = n
			}
			if f.Scope == "local" {
				r.Scope = journal.ScopeLocal
			}
			if err := r.Type.UnmarshalText([]byte(f.Type)); err != nil {
				t.Fatal(err)
			}
			p, err := journal.AppendPayload([]byte{byte(r.Type), r.Flags}, r)
			if err != nil {
				t.Fatal(err)
			}
			out = recstream.AppendFrame(out, recstream.KindRecord, p)
		case "step_end":
			out = recstream.AppendFrame(out, recstream.KindStepEnd, nil)
		case "snapshot":
			p, _ := journal.AppendPayload(nil, journal.Record{Type: journal.TypeSnapshot, Data: f.Data})
			out = recstream.AppendFrame(out, recstream.KindSnapshot, p)
		case "flush":
			b := byte(0)
			if f.Durable {
				b = 1
			}
			out = recstream.AppendFrame(out, recstream.KindFlush, []byte{b})
		case "close":
			out = recstream.AppendFrame(out, recstream.KindClose, nil)
		case "raw":
			k := f.RawKind
			out = recstream.AppendFrame(out, byte(k), f.Payload)
		default:
			t.Fatalf("unknown frame %q", f.Frame)
		}
	}
	return out
}
