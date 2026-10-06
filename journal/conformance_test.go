package journal

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
	var buf bytes.Buffer
	if _, err := NewWriter(&buf, meta); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

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

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// conformanceCases returns the files of the suite. Names under valid/ must decode
// to the paired JSON; names under invalid/ must be rejected.
func conformanceCases(t *testing.T) map[string][]byte {
	meta := Meta{Service: "conformance", Start: StartGenesis, Producer: "kavach-go/conformance"}
	snapMeta := Meta{Service: "conformance", Start: StartSnapshot}
	hdr := header(t, meta)
	all := frames(t, sampleRecords()...)

	newerMinor := append([]byte(nil), hdr...)
	binary.LittleEndian.PutUint16(newerMinor[10:], Minor+1)
	newerMinor = fixHeaderCRC(newerMinor)

	badHeaderCRC := append([]byte(nil), hdr...)
	badHeaderCRC[len(badHeaderCRC)-1] ^= 0xff

	badRecordCRC := cat(hdr, frames(t, Record{Type: TypeClock, Seq: 0, UnixNanos: 1}))
	badRecordCRC[len(badRecordCRC)-1] ^= 0xff

	badUTF8 := rawBody(TypeOutput, 0, 0, append([]byte{2, 0xff, 0xfe}, 0))

	return map[string][]byte{
		"valid/all-types.kavach": cat(hdr, all),
		"valid/empty.kavach":     hdr,
		"valid/snapshot-start.kavach": cat(header(t, snapMeta), frames(t,
			Record{Type: TypeSnapshot, Flags: FlagCritical, Seq: 100, Data: []byte(`{"alice":10}`)},
			Record{Type: TypeInput, Seq: 101, Source: "s", Position: "7", Data: []byte("x")},
		)),
		"valid/truncated-tail.kavach": cat(hdr, all)[:len(hdr)+len(all)-3],
		"valid/unknown-noncritical.kavach": cat(hdr,
			frames(t, Record{Type: TypeClock, Seq: 0, UnixNanos: 1}),
			rawFrame(rawBody(0x90, 0, 1, []byte("ext"))),
			frames(t, Record{Type: TypeClock, Seq: 2, UnixNanos: 2}),
		),
		"valid/trailing-payload.kavach": cat(hdr,
			rawFrame(rawBody(TypeRand, 0, 0, []byte{2, 0xaa, 0xbb, 0x01, 0x02})),
		),

		"invalid/bad-magic.kavach":        cat([]byte("NOTKAVJ!"), hdr[8:]),
		"invalid/bad-header-crc.kavach":   badHeaderCRC,
		"invalid/bad-record-crc.kavach":   badRecordCRC,
		"invalid/newer-minor.kavach":      newerMinor,
		"invalid/seq-gap.kavach":          cat(hdr, frames(t, Record{Type: TypeClock, Seq: 0}), rawFrame(rawBody(TypeClock, 0, 2, make([]byte, 8)))),
		"invalid/genesis-nonzero.kavach":  cat(hdr, rawFrame(rawBody(TypeClock, 0, 5, make([]byte, 8)))),
		"invalid/unknown-critical.kavach": cat(hdr, rawFrame(rawBody(0x90, FlagCritical, 0, nil))),
		"invalid/reserved-flags.kavach":   cat(hdr, rawFrame(rawBody(TypeClock, 0x04, 0, make([]byte, 8)))),
		"invalid/bad-utf8.kavach":         cat(hdr, rawFrame(badUTF8)),
		"invalid/short-payload.kavach":    cat(hdr, rawFrame(rawBody(TypeClock, 0, 0, []byte{1, 2}))),
		"invalid/body-too-short.kavach":   cat(hdr, rawFrame([]byte{byte(TypeClock), 0, 0})),
		"invalid/snapshot-not-first.kavach": cat(header(t, snapMeta),
			rawFrame(rawBody(TypeInput, 0, 0, []byte{0, 0, 0}))),
	}
}

func fixHeaderCRC(h []byte) []byte {
	metaLen := binary.LittleEndian.Uint32(h[12:16])
	end := 16 + int(metaLen)
	binary.LittleEndian.PutUint32(h[end:], checksum(h[8:end]))
	return h
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
				out, _ := json.MarshalIndent(expected{j.Header.Major, j.Header.Minor, j.Header.Meta, j.Records, j.Truncated}, "", "  ")
				os.WriteFile(strings.TrimSuffix(path, ".kavach")+".json", append(out, '\n'), 0o644)
			}
		}
	}

	files, _ := filepath.Glob(filepath.Join(conformanceDir, "*", "*.kavach"))
	sort.Strings(files)
	if len(files) != len(cases) {
		t.Fatalf("found %d conformance files, expected %d; run go test ./journal -update", len(files), len(cases))
	}
	for _, path := range files {
		name, _ := filepath.Rel(conformanceDir, path)
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, cases[filepath.ToSlash(name)]) {
				t.Fatalf("file differs from generator; run go test ./journal -update")
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
			got, _ := json.MarshalIndent(expected{j.Header.Major, j.Header.Minor, j.Header.Meta, j.Records, j.Truncated}, "", "  ")
			if !bytes.Equal(append(got, '\n'), want) {
				t.Fatalf("decoding differs from %s.json:\n%s", strings.TrimSuffix(name, ".kavach"), got)
			}
		})
	}
}
