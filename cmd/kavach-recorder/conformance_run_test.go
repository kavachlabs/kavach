package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

type expectedJournal struct {
	Major     uint16           `json:"major"`
	Minor     uint16           `json:"minor"`
	Meta      journal.Meta     `json:"meta"`
	Records   []journal.Record `json:"records"`
	Truncated bool             `json:"truncated"`
}

type expectedOutput struct {
	Exit    int                        `json:"exit"`
	Control []map[string]any           `json:"control"`
	Files   map[string]expectedJournal `json:"files"`
}

func buildRecorder(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kavach-recorder")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// ringStream moves every frame after the open frame of a record stream into a
// new ring (SPEC.md §10.7), which cmd gets as descriptor 3. It returns the open
// frame, now naming the ring. A stream that does not start with a well-formed
// open frame is returned as it is and goes over the pipe.
func ringStream(t *testing.T, cmd *exec.Cmd, stream []byte) []byte {
	t.Helper()
	n, k := binary.Uvarint(stream)
	if k <= 0 || n < 1 || uint64(len(stream)-k) < n || stream[k] != recstream.KindOpen {
		return stream
	}
	var open map[string]any
	if json.Unmarshal(stream[k+1:k+int(n)], &open) != nil {
		return stream
	}
	const capacity = 4 << 20
	open["ring"] = capacity
	ring, f, err := recstream.CreateRing(t.TempDir(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ring.Close(); f.Close() })
	rest := stream[k+int(n):]
	if len(rest) > capacity {
		t.Fatalf("the case's frames (%d bytes) do not fit the test ring", len(rest))
	}
	if m, _ := ring.TryPublish(rest); m != len(rest) {
		t.Fatalf("published %d of %d bytes", m, len(rest))
	}
	cmd.ExtraFiles = []*os.File{f}
	b, _ := json.Marshal(open)
	// The doorbell follows the open frame; the end of input says the service ended.
	return append(recstream.AppendFrame(nil, recstream.KindOpen, b), 1)
}

// runCase feeds stream, the bytes of a record stream, to the recorder over a
// pipe, or over a ring if ring is set, and returns what it did: its exit
// status, its control messages (file names relative to dir) and every journal
// under dir, decoded.
func runCase(t *testing.T, bin, factsPath string, stream []byte, dir string, ring bool) expectedOutput {
	t.Helper()
	cmd := exec.Command(bin, "--test-facts", factsPath)
	if ring {
		stream = ringStream(t, cmd, stream)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stdout)
		read <- b
	}()
	// The recorder may exit before reading everything (after a fatal error), so
	// a write error is not a failure.
	stdin.Write(stream)
	stdin.Close()
	out := <-read
	exit := 0
	if err := cmd.Wait(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		exit = ee.ExitCode()
	}

	res := expectedOutput{Exit: exit, Files: map[string]expectedJournal{}, Control: []map[string]any{}}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("control line %q: %v", line, err)
		}
		if f, ok := m["file"].(string); ok {
			rel, err := filepath.Rel(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			m["file"] = filepath.ToSlash(rel)
		}
		res.Control = append(res.Control, m)
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if !strings.HasSuffix(path, ".kavach") || strings.HasPrefix(d.Name(), ".") {
			t.Errorf("unexpected file %s (stderr: %s)", rel, stderr.String())
			return nil
		}
		j, err := journal.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return nil
		}
		if j.Records == nil {
			j.Records = []journal.Record{}
		}
		res.Files[filepath.ToSlash(rel)] = expectedJournal{j.Header.Major, j.Header.Minor, j.Header.Meta, j.Records, j.Truncated}
		if zstd, err := exec.LookPath("zstd"); err == nil {
			if o, err := exec.Command(zstd, "-t", path).CombinedOutput(); err != nil {
				t.Errorf("zstd -t %s: %v\n%s", rel, err, o)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestRecorderConformance(t *testing.T) {
	bin := buildRecorder(t)
	all := cases()
	if *update {
		os.RemoveAll(casesDir)
		for _, c := range all {
			dir := filepath.Join(casesDir, c.name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, "stream.json"), marshal(t, map[string]any{"description": c.desc, "frames": c.frames}), 0o644)
			facts := map[string]any{"run": "test-run", "recorded_at": "2026-01-01T00:00:00Z", "facts": baseFacts()}
			os.WriteFile(filepath.Join(dir, "facts.json"), marshal(t, facts), 0o644)
		}
	}

	dirs, _ := filepath.Glob(filepath.Join(casesDir, "*"))
	sort.Strings(dirs)
	if len(dirs) != len(all) {
		t.Fatalf("found %d cases, expected %d; run go test ./cmd/kavach-recorder -update", len(dirs), len(all))
	}
	// Every case runs over both transports and must give the same output.
	for _, ring := range []bool{false, true} {
		name := "pipe"
		if ring {
			name = "ring"
		}
		t.Run(name, func(t *testing.T) {
			for _, dir := range dirs {
				t.Run(filepath.Base(dir), func(t *testing.T) {
					raw, err := os.ReadFile(filepath.Join(dir, "stream.json"))
					if err != nil {
						t.Fatal(err)
					}
					out := t.TempDir()
					got := marshal(t, runCase(t, bin, filepath.Join(dir, "facts.json"), encodeFrames(t, raw, out), out, ring))
					expPath := filepath.Join(dir, "expected.json")
					if *update && !ring {
						os.WriteFile(expPath, got, 0o644)
					}
					want, err := os.ReadFile(expPath)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("output differs from %s:\n%s", expPath, got)
					}
				})
			}
		})
	}
}
