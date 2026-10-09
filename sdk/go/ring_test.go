//go:build unix

package kavach_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/testrecorder"
	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

const helperEnv = "KAVACH_RING_TEST_HELPER"

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperEnv); dir != "" {
		killedService(dir)
		return
	}
	os.Exit(testrecorder.Run(m))
}

type killer struct{}

// Handle kills its own process, as an abort or the OOM killer would, once the
// step's input is on record.
func (killer) Handle(env kavach.Env, in kavach.Input) error {
	if string(in.Data) == "die" {
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	env.Now()
	return nil
}

// killedService is the helper process: it records a step that completes, then
// one that kills the process.
func killedService(dir string) {
	r := kavach.NewRecorder(killer{}, kavach.Options{Service: "killed", Dir: dir, NoRing: os.Getenv(helperEnv+"_PIPE") != ""})
	r.Step(kavach.Input{Source: "t", Position: "0", Data: []byte("fine")})
	r.Step(kavach.Input{Source: "t", Position: "1", Data: []byte("die")})
}

// A service killed right after publishing a step's input still leaves a crash
// marker and a fixture, over the ring (SPEC.md §10.7) and over the pipe.
func TestKilledServiceLeavesCrashFixture(t *testing.T) {
	for _, transport := range []string{"ring", "pipe"} {
		t.Run(transport, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), helperEnv+"="+dir)
			if transport == "pipe" {
				cmd.Env = append(cmd.Env, helperEnv+"_PIPE=1")
			}
			err := cmd.Run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("the helper should die of SIGKILL, got %v", err)
			}

			// The recorder is no child of the test: it finishes on its own.
			var fixture string
			for deadline := time.Now().Add(10 * time.Second); fixture == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				m, _ := filepath.Glob(filepath.Join(dir, "fixtures", "*.kavach"))
				if len(m) == 1 {
					fixture = m[0]
				}
			}
			if fixture == "" {
				t.Fatal("no fixture was written")
			}
			j, err := journal.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			last := j.Records[len(j.Records)-1]
			if last.Type != journal.TypeMarker || last.Kind != journal.MarkerCrash {
				t.Fatalf("last record of the fixture = %+v", last)
			}
			var in journal.Record
			for _, r := range j.Records {
				if r.Type == journal.TypeInput {
					in = r
				}
			}
			if string(in.Data) != "die" {
				t.Fatalf("the fixture's failing input = %q", in.Data)
			}
		})
	}
}

type sink struct{}

func (sink) Handle(env kavach.Env, in kavach.Input) error {
	env.Emit("out", in.Data)
	return nil
}

// Frames larger than the ring, and a ring that keeps filling, lose nothing.
func TestSmallRingLosesNothing(t *testing.T) {
	dir := t.TempDir()
	r := kavach.NewRecorder(sink{}, kavach.Options{Service: "small", Dir: dir, RingBytes: 64 << 10})
	big := bytes.Repeat([]byte("b"), 200<<10)
	const steps = 400
	for i := 0; i < steps; i++ {
		data := []byte("small")
		if i%100 == 7 {
			data = big
		}
		if err := r.Step(kavach.Input{Source: "t", Position: "p", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	file := waitFile(t, r)
	r.Close()
	j, err := journal.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	inputs, bigs := 0, 0
	for _, rec := range j.Records {
		if rec.Type == journal.TypeInput {
			inputs++
			if len(rec.Data) == len(big) {
				bigs++
			}
		}
	}
	if inputs != steps || bigs != 4 {
		t.Fatalf("journal has %d inputs, %d large; want %d, 4", inputs, bigs, steps)
	}
}

func waitFile(t *testing.T, r *kavach.Recorder) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if f := r.File(); f != "" {
			return f
		}
	}
	t.Fatal("the recorder never reported its file")
	return ""
}

// A recorder that stops reading while the ring is full does not hang the
// service: recording stops (SPEC.md §10.1).
func TestDeadRecorderEndsTheWait(t *testing.T) {
	r := kavach.NewRecorder(sink{}, kavach.Options{Service: "dead", Dir: t.TempDir(), RingBytes: 64 << 10,
		RecorderCommand: []string{"sleep", "1"}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			r.Step(kavach.Input{Source: "t", Position: "p", Data: bytes.Repeat([]byte("x"), 30<<10)})
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the service is stuck waiting for a recorder that is gone")
	}
	r.Close()
}
