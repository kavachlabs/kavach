//go:build unix

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

// runRing starts the recorder with ring as descriptor 3 (none if nil), sends
// the open frame naming a ring of the given capacity and closes standard
// input. It returns the exit status and the control stream.
func runRing(t *testing.T, ring *os.File, capacity int) (int, string) {
	t.Helper()
	cmd := exec.Command(buildRecorder(t))
	if ring != nil {
		cmd.ExtraFiles = []*os.File{ring}
	}
	open, _ := json.Marshal(map[string]any{"protocol": 1, "service": "ring", "start": "genesis", "dir": t.TempDir(), "ring": capacity})
	cmd.Stdin = bytes.NewReader(recstream.AppendFrame(nil, recstream.KindOpen, open))
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

func TestRingFatalErrors(t *testing.T) {
	newFile := func(t *testing.T, capacity int) (*recstream.Ring, *os.File) {
		g, f, err := recstream.CreateRing(t.TempDir(), capacity)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close(); f.Close() })
		return g, f
	}
	word := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

	tests := []struct {
		name string
		file func(t *testing.T) *os.File
		cap  int
		want string
	}{
		{"no descriptor 3", func(*testing.T) *os.File { return nil }, recstream.MinRing, "ring:"},
		{"bad magic", func(t *testing.T) *os.File {
			_, f := newFile(t, recstream.MinRing)
			f.WriteAt([]byte("NOTARING"), 0)
			return f
		}, recstream.MinRing, "bad magic"},
		{"capacity differs from the open frame", func(t *testing.T) *os.File {
			_, f := newFile(t, 2*recstream.MinRing)
			return f
		}, recstream.MinRing, "capacity"},
		{"file smaller than the open frame says", func(t *testing.T) *os.File {
			_, f := newFile(t, recstream.MinRing)
			return f
		}, 2 * recstream.MinRing, "file is"},
		{"more unread bytes than the capacity", func(t *testing.T) *os.File {
			_, f := newFile(t, recstream.MinRing)
			f.WriteAt(word(recstream.MinRing+1), 64)
			return f
		}, recstream.MinRing, "unread"},
		{"capacity not a power of two", func(t *testing.T) *os.File {
			_, f := newFile(t, recstream.MinRing)
			return f
		}, recstream.MinRing + 1, "power of two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runRing(t, tc.file(t), tc.cap)
			if code != 1 || !strings.Contains(out, `"fatal":true`) || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, control stream:\n%s", code, out)
			}
		})
	}
}

// A step published into the ring before the service died, with no step_end, is
// the recorded failure (SPEC.md §10.7, Ending).
func TestRingEndsInsideStep(t *testing.T) {
	g, f, err := recstream.CreateRing(t.TempDir(), recstream.MinRing)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defer f.Close()
	frame, _ := recstream.AppendRecordFrame(nil, journal.Record{Type: journal.TypeInput, Source: "s", Position: "1", Data: []byte("x")})
	if n, _ := g.TryPublish(frame); n != len(frame) {
		t.Fatal("ring full")
	}

	dir := t.TempDir()
	cmd := exec.Command(buildRecorder(t))
	cmd.ExtraFiles = []*os.File{f}
	open, _ := json.Marshal(map[string]any{"protocol": 1, "service": "ring", "start": "genesis", "dir": dir, "ring": recstream.MinRing})
	cmd.Stdin = bytes.NewReader(recstream.AppendFrame(nil, recstream.KindOpen, open))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"t":"fixture"`) || !strings.Contains(out.String(), `"failure":"crash"`) {
		t.Fatalf("control stream:\n%s", out.String())
	}
}
