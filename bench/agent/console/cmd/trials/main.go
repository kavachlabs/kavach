// Command trials runs the console's attempt reader (harbor.TrialReader) as a
// Kavach service, so that the reader's bugs become fixtures like any
// service's incidents.
//
//	trials -jobs ../results/harbor -fixtures /tmp/trials 2026-10-15__15-47-13
//
// feeds the finished attempts of the named jobs (all of them if none is
// named) to the reader (with -steps, harbor.StepsReader, which reads their
// steps), in the order they finished, through the flight
// recorder (kavach-recorder, found through $KAVACH_RECORDER or PATH). An
// attempt the reader gets wrong stops it, and the recorder writes a fixture.
// With "kavach-host" as its last argument it serves the kavach CLI instead:
//
//	kavach replay harbor/testdata/kavach/provider-error.kavach --bin ./trials
//	kavach replay harbor/testdata/kavach/steps/cli-steps.kavach --bin "./trials -steps"
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// logEvents is how many of OpenCode's last events an input keeps, besides
// its Kavach tool calls.
const logEvents = 8

func main() {
	jobs := flag.String("jobs", "../results/harbor", "Harbor's jobs directory")
	dir := flag.String("fixtures", "", "directory for the journal; fixtures go in its fixtures/ subdirectory (default kavach)")
	steps := flag.Bool("steps", false, "run the steps reader (harbor.StepsReader) instead of the trial reader")
	flag.Parse()
	reader := func() kavach.Handler { return &harbor.TrialReader{} }
	service := "trials"
	if *steps {
		reader = func() kavach.Handler { return &harbor.StepsReader{} }
		service = "steps"
	}
	kavach.MaybeReplay(reader)

	attempts, err := finished(*jobs, flag.Args())
	if err != nil {
		log.Fatal(err)
	}
	rec := kavach.NewRecorder(reader(), kavach.Options{
		Service:   service,
		Dir:       *dir,
		OnFixture: func(file, failure string) { fmt.Printf("fixture %s\n  %s\n", file, failure) },
	})
	defer rec.Close()
	for _, a := range attempts {
		b, err := json.Marshal(a.AttemptInput)
		if err != nil {
			log.Fatal(err)
		}
		if err := rec.Step(kavach.Input{Source: "harbor:" + a.Job, Position: a.ID, Data: b}); err != nil {
			var inv *kavach.InvariantError
			if errors.As(err, &inv) {
				fmt.Printf("%s/%s: %v\n", a.Job, a.ID, err)
				return
			}
			log.Fatalf("%s/%s: %v", a.Job, a.ID, err)
		}
	}
	fmt.Printf("%d attempts read\n", len(attempts))
}

type attempt struct {
	harbor.AttemptInput
	finishedAt string
}

// finished returns the finished attempts of the named jobs under root, in the
// order they finished.
func finished(root string, names []string) ([]attempt, error) {
	if len(names) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	var out []attempt
	for _, job := range names {
		dirs, err := filepath.Glob(filepath.Join(root, job, "*__*"))
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			result, err := os.ReadFile(filepath.Join(d, "result.json"))
			if err != nil {
				continue
			}
			var r struct {
				FinishedAt string `json:"finished_at"`
			}
			if json.Unmarshal(result, &r) != nil || r.FinishedAt == "" {
				continue
			}
			a := attempt{AttemptInput: harbor.AttemptInput{Job: job, ID: filepath.Base(d), Result: compact(result)}, finishedAt: r.FinishedAt}
			if b, err := os.ReadFile(filepath.Join(d, "lock.json")); err == nil {
				a.Lock = compact(b)
			}
			if b, err := os.ReadFile(filepath.Join(d, "verifier", "kavach.json")); err == nil {
				a.Kavach = compact(b)
			}
			a.Log = events(filepath.Join(d, "agent", "opencode.txt"), logEvents)
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].finishedAt < out[j].finishedAt })
	return out, nil
}

// home is the user's home directory, which Harbor writes into paths and
// stack traces. Fixtures are committed, and the reader never reads them.
var home, _ = os.UserHomeDir()

// compact returns a JSON file without its whitespace and with the home
// directory written as ~, or nil if it is not JSON.
func compact(b []byte) json.RawMessage {
	var c bytes.Buffer
	if json.Compact(&c, b) != nil {
		return nil
	}
	if home == "" {
		return c.Bytes()
	}
	return bytes.ReplaceAll(c.Bytes(), []byte(home), []byte("~"))
}

// events returns, in order, the events of an OpenCode log that call a tool
// with "kavach" in them, and its last n events.
func events(path string, n int) []json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []json.RawMessage
	var keep []bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		c := compact(sc.Bytes())
		if c == nil {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		json.Unmarshal(c, &ev)
		kavach := ev.Type == "tool_use" && bytes.Contains(c, []byte("kavach"))
		if kavach {
			c = trimTool(c)
		}
		all = append(all, c)
		keep = append(keep, kavach)
	}
	var out []json.RawMessage
	for i, c := range all {
		if keep[i] || i >= len(all)-n {
			out = append(out, c)
		}
	}
	return out
}

// trimTool drops what the reader never reads from a tool event: the
// model's reasoning metadata, and the copy of the output OpenCode keeps in
// state.metadata. The reader prefers that copy, so it goes only when it is
// the same as state.output.
func trimTool(c json.RawMessage) json.RawMessage {
	var ev map[string]json.RawMessage
	var part map[string]json.RawMessage
	if json.Unmarshal(c, &ev) != nil || json.Unmarshal(ev["part"], &part) != nil {
		return c
	}
	delete(part, "metadata")
	var state map[string]json.RawMessage
	if json.Unmarshal(part["state"], &state) == nil {
		var meta map[string]json.RawMessage
		if json.Unmarshal(state["metadata"], &meta) == nil && bytes.Equal(meta["output"], state["output"]) {
			delete(meta, "output")
			state["metadata"], _ = json.Marshal(meta)
			part["state"], _ = json.Marshal(state)
		}
	}
	ev["part"], _ = json.Marshal(part)
	out, err := json.Marshal(ev)
	if err != nil {
		return c
	}
	return out
}
