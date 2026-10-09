package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

var update = flag.Bool("update", false, "regenerate spec/testdata conformance fixtures")

const conformanceDir = "../spec/testdata"

// expected is the JSON form of a decoded journal used by the conformance suite.
type expected struct {
	Major     uint16   `json:"major"`
	Minor     uint16   `json:"minor"`
	Meta      Meta     `json:"meta"`
	Records   []Record `json:"records"`
	Truncated bool     `json:"truncated"`
}

func header(t *testing.T, meta Meta) []byte {
	t.Helper()
	if meta.Compression == "" {
		meta.Compression = CompressionZstd
	}
	b, err := appendHeader(nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// frames encodes records back to back, without checking their order.
func frames(t *testing.T, recs ...Record) []byte {
	t.Helper()
	var out []byte
	for _, r := range recs {
		var err error
		out, err = AppendFrame(out, r)
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// block encodes records as one block, without checking their order.
func block(t *testing.T, comp string, recs ...Record) []byte {
	t.Helper()
	return blockOf(t, comp, recs[0].Seq, len(recs), frames(t, recs...))
}

func blockOf(t *testing.T, comp string, firstSeq uint64, count int, raw []byte) []byte {
	t.Helper()
	b, err := appendBlock(nil, firstSeq, count, raw, comp, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// splitBlock returns the block frame and data frame of a one-block encoding.
func splitBlock(b []byte) (bf, df []byte) { return b[:8+blockFrameLen], b[8+blockFrameLen:] }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func skippable(magic uint32, data []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, magic)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

// streamedFrame encodes raw with the streaming encoder, which cannot know the
// content size in advance and so does not declare it.
func streamedFrame(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	w.Write(raw)
	w.Close()
	return buf.Bytes()
}

func sha(s string) []byte { b := sha256.Sum256([]byte(s)); return b[:] }

// conformanceCases returns the files of the suite. Names under valid/ must decode
// to the paired JSON; names under invalid/ must be rejected.
func conformanceCases(t *testing.T) map[string][]byte {
	const z, n = CompressionZstd, CompressionNone
	meta := Meta{Service: "conformance", Start: StartGenesis, Producer: "kavach-go/conformance"}
	noneMeta := meta
	noneMeta.Compression = n
	snapMeta := Meta{Service: "conformance", Start: StartSnapshot}
	hdr := header(t, meta)
	env := genesisEnv(0)
	all := append(sampleRecords(),
		Record{Type: TypeGateway, Flags: FlagCritical, Seq: 7, Gateway: "fx-rates", Request: []byte(`{"pair":"EURUSD"}`), Response: []byte(`{"rate":1.08}`)},
		Record{Type: TypeConfig, Flags: FlagCritical, Seq: 8, Key: "flag.ledger-v2", Present: true, Value: []byte("true"), Source: "launchdarkly"},
		Record{Type: TypeConfig, Flags: FlagCritical, Seq: 9, Key: "MAX_TRANSFER", Source: "env"},
		Record{Type: TypeOutput, Seq: 10, Sink: "shm:/dev/shm/orderbook", Data: []byte("x"), Scope: ScopeLocal},
	)

	gatewayRecs := []Record{
		env,
		{Type: TypeInput, Seq: 1, Source: "kafka:orders", Position: "0:1", Data: []byte("o1")},
		{Type: TypeGateway, Flags: FlagCritical, Seq: 2, Gateway: "fx-rates", Request: []byte("EURUSD"), Response: []byte("1.08")},
		{Type: TypeGateway, Flags: FlagCritical, Seq: 3, Gateway: "postgres:accounts", Request: []byte("select 1"), Error: "timeout"},
		{Type: TypeGateway, Flags: FlagCritical, Seq: 4, Gateway: "shm:/dev/shm/orderbook", Request: []byte("read"), Error: "ENOENT", Scope: ScopeLocal},
		{Type: TypeGateway, Flags: FlagCritical, Seq: 5, Gateway: "shm:/dev/shm/orderbook", Request: []byte("read"), Response: []byte("rows"), Scope: ScopeLocal},
	}
	configRecs := []Record{
		env,
		{Type: TypeInput, Seq: 1, Source: "s", Position: "1", Data: []byte("x")},
		{Type: TypeConfig, Flags: FlagCritical, Seq: 2, Key: "flag.ledger-v2", Present: true, Value: []byte("true"), Source: "launchdarkly"},
		{Type: TypeConfig, Flags: FlagCritical, Seq: 3, Key: "flag.new-ui", Present: false, Source: "launchdarkly"},
		{Type: TypeConfig, Flags: FlagCritical, Seq: 4, Key: "MAX_TRANSFER", Present: true, Value: []byte("1000"), Source: "env"},
	}
	envRecs := []Record{
		{Type: TypeEnvironment, Flags: FlagCritical, Seq: 0, Facts: []Fact{
			{Key: "env.MAX_TRANSFER", Form: FactValue, Value: []byte("1000")},
			{Key: "env.API_TOKEN", Form: FactSHA256, Value: sha("s3cret")},
			{Key: "env.UNUSED", Form: FactUnset},
			{Key: "host.mount./dev/shm", Form: FactValue, Value: []byte("tmpfs rw,nosuid,nodev")},
			{Key: "host.sessions.wfinfra", Form: FactValue, Value: []byte("1")},
		}},
		{Type: TypeInput, Seq: 1, Source: "s", Position: "1", Data: []byte("x")},
		{Type: TypeOutput, Seq: 2, Sink: "sink", Data: []byte("y")},
		{Type: TypeEnvironment, Flags: FlagCritical, Seq: 3, Facts: []Fact{
			{Key: "host.sessions.wfinfra", Form: FactValue, Value: []byte("0")},
		}},
		{Type: TypeInput, Seq: 4, Source: "s", Position: "2", Data: []byte("z")},
		{Type: TypeMarker, Seq: 5, Kind: MarkerPanic, Message: "segment vanished"},
		{Type: TypeEnvironment, Flags: FlagCritical, Seq: 6, Facts: []Fact{
			{Key: "host.mount./dev/shm", Form: FactUnset},
			{Key: "host.sessions.wfinfra", Form: FactSHA256, Value: sha("1")},
		}},
		{Type: TypeMarker, Seq: 7, Kind: MarkerExit, Message: "process ended without closing the recorder"},
	}

	// Several blocks, with records of every kind.
	multi := func(comp string) []byte {
		return cat(header(t, Meta{Service: "conformance", Start: StartGenesis, Compression: comp}),
			block(t, comp, all[0:3]...), block(t, comp, all[3:4]...), block(t, comp, all[4:]...))
	}

	allBlock := block(t, z, all...)
	dataFrameBad := func(comp string, mutate func(df []byte) []byte) []byte {
		b := block(t, comp, env, Record{Type: TypeClock, Seq: 1, UnixNanos: 1})
		bf, df := splitBlock(b)
		return cat(header(t, Meta{Service: "conformance", Start: StartGenesis, Compression: comp}), bf, mutate(append([]byte(nil), df...)))
	}

	newerMinor := append([]byte(nil), hdr...)
	binary.LittleEndian.PutUint16(newerMinor[8+8+2:], Minor+1)
	newerMinor = fixHeaderCRC(newerMinor)

	badHeaderCRC := append([]byte(nil), hdr...)
	badHeaderCRC[len(badHeaderCRC)-1] ^= 0xff

	badBlockHeaderCRC := cat(hdr, block(t, z, env))
	badBlockHeaderCRC[len(hdr)+8+blockFrameLen-1] ^= 0xff

	patchBlockHeader := func(mutate func(h *blockHeader)) []byte {
		b := block(t, z, env, Record{Type: TypeClock, Seq: 1, UnixNanos: 1})
		_, df := splitBlock(b)
		h, err := parseBlockHeader(b[8 : 8+blockFrameLen])
		if err != nil {
			t.Fatal(err)
		}
		mutate(&h)
		return cat(hdr, h.append(nil), df)
	}

	// A record whose last bytes fall in the next block.
	spanA := frames(t, env, Record{Type: TypeInput, Seq: 1, Source: "s", Data: []byte("0123456789")})
	spanRaw := spanA[:len(spanA)-5]

	short := rawFrame([]byte("abc"))

	badUTF8 := rawBody(TypeOutput, 0, 1, append([]byte{2, 0xff, 0xfe}, 0, 0))
	inputBody := rawBody(TypeInput, 0, 1, []byte{0, 0, 0})
	oneRaw := func(recs ...[]byte) []byte {
		return cat(hdr, blockOf(t, z, 0, len(recs), cat(recs...)))
	}
	envFrame := frames(t, env)

	return map[string][]byte{
		"valid/all-types.kavach": cat(hdr, allBlock),
		"valid/empty.kavach":     hdr,
		"valid/snapshot-start.kavach": cat(header(t, snapMeta), block(t, z,
			Record{Type: TypeSnapshot, Flags: FlagCritical, Seq: 100, Data: []byte(`{"alice":10}`)},
			genesisEnv(101),
			Record{Type: TypeInput, Seq: 102, Source: "s", Position: "7", Data: []byte("x")},
		)),
		"valid/multi-block-zstd.kavach": multi(z),
		"valid/multi-block-none.kavach": multi(n),
		"valid/unknown-skippable-frame.kavach": cat(hdr,
			block(t, z, all[0:2]...),
			skippable(0x184D2A5C, []byte("an index, perhaps")),
			skippable(0x184D2A50, nil),
			block(t, z, all[2:4]...)),
		"valid/truncated-in-block.kavach":       cat(hdr, block(t, z, all[0:3]...), block(t, z, all[3:5]...)[:40]),
		"valid/truncated-in-block-frame.kavach": cat(hdr, block(t, n, all[0:3]...), []byte{0x5b, 0x2a, 0x4d, 0x18, 24, 0}),
		"valid/unknown-noncritical.kavach": cat(hdr, block(t, z,
			env,
			Record{Type: 0x90, Seq: 1, Data: []byte("ext")},
			Record{Type: TypeClock, Seq: 2, UnixNanos: 2},
		)),
		"valid/trailing-payload.kavach": oneRaw(envFrame, recordFrame(rawBody(TypeRand, 0, 1, []byte{2, 0xaa, 0xbb, 0x01, 0x02}))),
		"valid/gateway.kavach":          cat(hdr, block(t, z, gatewayRecs...)),
		"valid/config.kavach":           cat(hdr, block(t, z, configRecs...)),
		"valid/environment-forms.kavach": cat(header(t, noneMeta),
			block(t, n, envRecs[:5]...), block(t, n, envRecs[5:]...)),
		"valid/recorder-meta.kavach": cat(header(t, Meta{Service: "conformance", Start: StartGenesis, Recorder: "kavach-recorder/0.2.0",
			Run: "r1", Segment: intp(2), CutFrom: &CutFrom{Run: "r1", Segment: 2, FirstSeq: 0, LastSeq: 1}}), block(t, z, env, Record{Type: TypeInput, Seq: 1, Source: "s"})),

		"invalid/bad-magic.kavach":            cat(hdr[:8], []byte("NOTKAVJ!"), hdr[16:]),
		"invalid/bad-header-crc.kavach":       badHeaderCRC,
		"invalid/newer-minor.kavach":          newerMinor,
		"invalid/no-header-frame.kavach":      cat(block(t, z, env, Record{Type: TypeClock, Seq: 1, UnixNanos: 1})),
		"invalid/header-not-first.kavach":     cat(skippable(0x184D2A5C, nil), hdr),
		"invalid/bad-block-header-crc.kavach": badBlockHeaderCRC,
		"invalid/data-frame-bad-checksum-zstd.kavach": dataFrameBad(z, func(df []byte) []byte {
			df[len(df)-1] ^= 0xff
			return df
		}),
		"invalid/data-frame-bad-checksum-none.kavach": dataFrameBad(n, func(df []byte) []byte {
			df[len(df)-1] ^= 0xff
			return df
		}),
		"invalid/data-frame-no-content-size.kavach": func() []byte {
			raw := cat(envFrame, frames(t, Record{Type: TypeClock, Seq: 1, UnixNanos: 1}))
			df := streamedFrame(t, raw)
			return cat(hdr, blockHeader{0, 2, uint32(len(raw)), uint32(len(df))}.append(nil), df)
		}(),
		"invalid/data-frame-wrong-length.kavach": patchBlockHeader(func(h *blockHeader) { h.rawLen++ }),
		"invalid/data-frame-short-content.kavach": func() []byte {
			raw := cat(envFrame, frames(t, Record{Type: TypeClock, Seq: 1, UnixNanos: 1}))
			df := rawFrame(raw)
			binary.LittleEndian.PutUint32(df[5:], uint32(len(raw))+1)
			return cat(hdr, blockHeader{0, 2, uint32(len(raw)) + 1, uint32(len(df))}.append(nil), df)
		}(),
		"invalid/block-too-many-records.kavach":   patchBlockHeader(func(h *blockHeader) { h.count++ }),
		"invalid/block-too-few-records.kavach":    patchBlockHeader(func(h *blockHeader) { h.count-- }),
		"invalid/block-first-seq-mismatch.kavach": patchBlockHeader(func(h *blockHeader) { h.firstSeq = 7 }),
		"invalid/record-spans-blocks.kavach": cat(hdr,
			blockOf(t, z, 0, 2, spanRaw),
			blockOf(t, z, 2, 1, spanA[len(spanA)-5:])),
		"invalid/block-first-seq-gap.kavach": cat(hdr,
			block(t, z, env),
			block(t, z, Record{Type: TypeInput, Seq: 5, Source: "s"})),
		"invalid/block-first-seq-repeat.kavach": cat(hdr,
			block(t, z, env, Record{Type: TypeClock, Seq: 1, UnixNanos: 1}),
			block(t, z, Record{Type: TypeClock, Seq: 1, UnixNanos: 1})),
		"invalid/data-frame-first.kavach":   cat(hdr, short),
		"invalid/seq-gap.kavach":            oneRaw(envFrame, frames(t, Record{Type: TypeClock, Seq: 2})),
		"invalid/genesis-nonzero.kavach":    cat(hdr, block(t, z, genesisEnv(5))),
		"invalid/unknown-critical.kavach":   cat(hdr, block(t, z, env, Record{Type: 0x90, Flags: FlagCritical, Seq: 1})),
		"invalid/reserved-flags.kavach":     oneRaw(envFrame, recordFrame(rawBody(TypeClock, 0x04, 1, make([]byte, 8)))),
		"invalid/bad-utf8.kavach":           oneRaw(envFrame, recordFrame(badUTF8)),
		"invalid/short-payload.kavach":      oneRaw(envFrame, recordFrame(rawBody(TypeClock, 0, 1, []byte{1, 2}))),
		"invalid/body-too-short.kavach":     oneRaw(envFrame, recordFrame([]byte{byte(TypeClock), 0, 0})),
		"invalid/snapshot-not-first.kavach": cat(header(t, snapMeta), blockOf(t, z, 1, 1, recordFrame(inputBody))),
		"invalid/gateway-no-critical.kavach": cat(hdr, block(t, z, env,
			Record{Type: TypeInput, Seq: 1, Source: "s"},
			Record{Type: TypeGateway, Seq: 2, Gateway: "g", Request: []byte("q"), Response: []byte("r")})),
		"invalid/config-no-critical.kavach": cat(hdr, block(t, z, env,
			Record{Type: TypeInput, Seq: 1, Source: "s"},
			Record{Type: TypeConfig, Seq: 2, Key: "k", Present: true, Value: []byte("v")})),
		"invalid/environment-no-critical.kavach": cat(hdr, block(t, z, Record{Type: TypeEnvironment, Seq: 0, Facts: env.Facts})),
		"invalid/environment-repeated-key.kavach": cat(hdr, block(t, z,
			Record{Type: TypeEnvironment, Flags: FlagCritical, Seq: 0, Facts: []Fact{{Key: "env.A", Value: []byte("1")}, {Key: "env.A", Value: []byte("2")}}})),
		"invalid/environment-after-input.kavach": cat(hdr, block(t, z,
			Record{Type: TypeInput, Seq: 0, Source: "s"}, genesisEnv(1))),
		"invalid/environment-missing.kavach": cat(hdr, block(t, z,
			Record{Type: TypeInput, Seq: 0, Source: "s"})),
		"invalid/environment-bad-form.kavach": cat(hdr, block(t, z,
			Record{Type: TypeEnvironment, Flags: FlagCritical, Seq: 0, Facts: []Fact{{Key: "env.A", Form: 3}}})),
	}
}

func intp(v int) *int { return &v }

func fixHeaderCRC(h []byte) []byte {
	metaLen := binary.LittleEndian.Uint32(h[8+8+4:])
	end := 8 + 8 + 8 + int(metaLen)
	binary.LittleEndian.PutUint32(h[end:], checksum(h[16:end]))
	return h
}

func decodedJSON(t *testing.T, j *Journal) []byte {
	t.Helper()
	out, err := json.MarshalIndent(expected{j.Header.Major, j.Header.Minor, j.Header.Meta, j.Records, j.Truncated}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestConformance(t *testing.T) {
	cases := conformanceCases(t)
	if *update {
		os.RemoveAll(conformanceDir)
		for name, b := range cases {
			path := filepath.Join(conformanceDir, name)
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(name, "valid/") {
				j, err := Decode(bytes.NewReader(b))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				os.WriteFile(strings.TrimSuffix(path, ".kavach")+".json", decodedJSON(t, j), 0o644)
			}
		}
	}

	files, _ := filepath.Glob(filepath.Join(conformanceDir, "*", "*.kavach"))
	sort.Strings(files)
	if len(files) != len(cases) {
		t.Fatalf("found %d conformance files, expected %d; run go test ./journal -update", len(files), len(cases))
	}
	zstdTool, _ := exec.LookPath("zstd")
	for _, path := range files {
		name, _ := filepath.Rel(conformanceDir, path)
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Compressed bytes depend on the zstd version, so the checked-in
			// file is not compared with the generator's output; the generator
			// only decides what is in the suite.
			if _, ok := cases[filepath.ToSlash(name)]; !ok {
				t.Fatalf("%s is not produced by the generator; run go test ./journal -update", name)
			}
			j, err := Decode(bytes.NewReader(b))
			if strings.HasPrefix(name, "invalid") {
				if err == nil {
					t.Fatalf("decoded an invalid journal")
				}
				t.Log(err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(strings.TrimSuffix(path, ".kavach") + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if got := decodedJSON(t, j); !bytes.Equal(got, want) {
				t.Fatalf("decoding differs from %s.json:\n%s", strings.TrimSuffix(name, ".kavach"), got)
			}
			// Every valid file is a Zstandard stream, except one that ends
			// inside a frame: zstd rightly calls that truncated.
			if zstdTool != "" && !j.Truncated {
				if out, err := exec.Command(zstdTool, "-t", path).CombinedOutput(); err != nil {
					t.Fatalf("zstd -t: %v\n%s", err, out)
				}
			}
		})
	}
}
