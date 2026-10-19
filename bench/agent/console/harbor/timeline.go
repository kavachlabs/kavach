package harbor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// TimelineReader reads attempts' steps as the Steps tab shows them, running
// or finished. A service of its own, like StepsReader, so that its outputs
// do not change the other services' fixtures.
type TimelineReader struct {
	seen []seenTimeline
}

type seenTimeline struct {
	id    string
	in    AttemptInput
	steps []Step
}

// Timeline is what the Steps tab shows for an attempt: each step's time,
// kind and title, a model call's usage, or the error that kept the steps
// from being read.
type Timeline struct {
	Steps []TimelineStep `json:"steps"`
	Error string         `json:"error,omitempty"`
}

// TimelineStep is one step of a Timeline.
type TimelineStep struct {
	TMs    int64    `json:"t_ms"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Tokens *Tokens  `json:"tokens,omitempty"`
	Cost   *float64 `json:"cost,omitempty"`
	// A tool call that has not ended, and the end of its output so far.
	Running bool   `json:"running,omitempty"`
	Output  string `json:"output,omitempty"`
}

// Handle reads one attempt's steps and emits them on "timeline".
func (r *TimelineReader) Handle(env kavach.Env, in kavach.Input) error {
	var a AttemptInput
	if err := json.Unmarshal(in.Data, &a); err != nil {
		return fmt.Errorf("attempt: %w", err)
	}
	tl := Timeline{Steps: []TimelineStep{}}
	steps, err := stepsFrom(a.files())
	if err != nil {
		tl.Error = clipTo(err.Error(), 200)
		steps = nil
	}
	for _, st := range steps {
		ts := TimelineStep{TMs: st.TMs, Kind: st.Kind, Title: clipTo(st.Title, 80), Tokens: st.Tokens, Cost: st.Cost, Running: st.Running}
		if st.Running {
			ts.Output = st.Detail
		}
		tl.Steps = append(tl.Steps, ts)
	}
	b, err := json.Marshal(tl)
	if err != nil {
		return err
	}
	env.Emit("timeline", b)
	r.seen = append(r.seen, seenTimeline{id: a.ID, in: a, steps: steps})
	return nil
}

// Invariants are the Steps tab's rules, from the specification of live
// steps (what an attempt's steps come from, and how they are timed). They
// read the attempt's files themselves, not through the reader.
func (r *TimelineReader) Invariants() []kavach.Invariant {
	return []kavach.Invariant{
		{Name: "steps-in-time-order", Check: func() error {
			// Times count from the first event or part the steps are read
			// from, and never go back.
			for _, s := range r.seen {
				for i, st := range s.steps {
					if st.TMs < 0 {
						return fmt.Errorf("%s: step %d (%s) is at %d ms, before the first event", s.id, i, clipTo(st.Title, 60), st.TMs)
					}
					if i > 0 && st.TMs < s.steps[i-1].TMs {
						return fmt.Errorf("%s: step %d (%s) is at %d ms, before step %d at %d ms", s.id, i, clipTo(st.Title, 60), st.TMs, i-1, s.steps[i-1].TMs)
					}
				}
			}
			return nil
		}},
		{Name: "running-calls-while-live", Check: func() error {
			// Only a live attempt has running steps: one per call the export
			// shows running that the steps do not hold as ended, in the
			// export's order, after every other step, described as the
			// ended call will be, with the end of its output so far.
			for _, s := range r.seen {
				w := agentWork(s.in)
				var running []Step
				for i, st := range s.steps {
					if st.Running {
						running = append(running, st)
					} else if len(running) > 0 {
						return fmt.Errorf("%s: step %d (%s) comes after a running step", s.id, i, clipTo(st.Title, 60))
					}
				}
				if len(running) > 0 && !liveAttempt(s.in) {
					return fmt.Errorf("%s: %d running steps, and the attempt has ended", s.id, len(running))
				}
				var calls []runningCall
				for _, c := range w.running {
					if !w.ended[c.id] {
						calls = append(calls, c)
					}
				}
				if len(running) > len(calls) {
					return fmt.Errorf("%s: %d running steps, the export has %d running calls not yet ended", s.id, len(running), len(calls))
				}
				for i, st := range running {
					c := calls[i]
					switch {
					case st.Exit != nil || st.Verdict != "" || st.Passed != nil:
						return fmt.Errorf("%s: running step %q has an exit status or a verdict", s.id, clipTo(st.Title, 60))
					case c.tool == "bash" && st.Title != clip(c.command):
						return fmt.Errorf("%s: running step %q is not its command %q", s.id, clipTo(st.Title, 60), clipTo(c.command, 60))
					case len(st.Detail) > 2000:
						return fmt.Errorf("%s: running step %q has %d bytes of output", s.id, clipTo(st.Title, 60), len(st.Detail))
					case !strings.HasSuffix(c.output, st.Detail):
						return fmt.Errorf("%s: running step %q's output is not the end of the call's", s.id, clipTo(st.Title, 60))
					}
					if cut := len(c.output) - len(st.Detail); cut > 0 && c.output[cut-1] != '\n' && strings.Contains(st.Detail, "\n") {
						return fmt.Errorf("%s: running step %q's output starts inside a line", s.id, clipTo(st.Title, 60))
					}
				}
			}
			return nil
		}},
		{Name: "only-the-agents-work", Check: func() error {
			// Every step is one the agent took, in the file the steps come
			// from: no more text, tool or usage steps than it holds, and
			// never the user's message.
			for _, s := range r.seen {
				w := agentWork(s.in)
				var text, tools, usage int
				for _, st := range s.steps {
					switch {
					case st.Running:
						// Calls that have not ended: running-calls-while-live.
					case st.Kind == KindText:
						text++
						for _, u := range w.user {
							if u != "" && st.Title == clip(u) {
								return fmt.Errorf("%s: the user's message is shown as a step", s.id)
							}
						}
					case st.Kind == KindUsage:
						usage++
					default:
						tools++
					}
				}
				switch {
				case text > w.text:
					return fmt.Errorf("%s: %d text steps, the agent wrote %d texts", s.id, text, w.text)
				case usage > w.usage:
					return fmt.Errorf("%s: %d usage steps, the agent made %d model calls", s.id, usage, w.usage)
				case tools > w.tools:
					return fmt.Errorf("%s: %d tool and error steps, the agent has %d finished tool calls and errors", s.id, tools, w.tools)
				}
			}
			return nil
		}},
	}
}

// work counts what the agent did, in the file an attempt's steps come
// from, and holds the user's messages.
type work struct {
	text, tools, usage int
	user               []string
	// The ids of the ended tool calls the steps come from, and the calls
	// the session export shows running, whichever the steps come from.
	ended   map[string]bool
	running []runningCall
}

type runningCall struct {
	id, tool, command, output string
}

// liveAttempt reports whether the attempt's agent may still be running: it
// has no result, and the agent has not marked its live file finished.
func liveAttempt(a AttemptInput) bool {
	if len(a.Result) > 0 && string(a.Result) != "null" {
		return false
	}
	var f struct {
		Finished string `json:"finished"`
	}
	return json.Unmarshal(a.LiveFile, &f) != nil || f.Finished == ""
}

// agentWork reads the first of the event log, its live copy and the session
// export that the attempt has.
func agentWork(a AttemptInput) work {
	w := work{ended: map[string]bool{}}
	countEvent := func(line string) {
		var ev struct {
			Type string `json:"type"`
			Part struct {
				ID string `json:"id"`
			} `json:"part"`
		}
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil {
			return
		}
		switch ev.Type {
		case "text":
			w.text++
		case "tool_use", "error":
			w.tools++
			if ev.Part.ID != "" {
				w.ended[ev.Part.ID] = true
			}
		case "step_finish":
			w.usage++
		}
	}
	switch {
	case len(a.Log) > 0:
		for _, ev := range a.Log {
			countEvent(string(ev))
		}
	case a.Live != "":
		sc := bufio.NewScanner(strings.NewReader(a.Live))
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			countEvent(sc.Text())
		}
	}
	var export struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
			Parts []struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Text  string `json:"text"`
				Tool  string `json:"tool"`
				State struct {
					Status string `json:"status"`
					Input  struct {
						Command string `json:"command"`
					} `json:"input"`
					Metadata struct {
						Output string `json:"output"`
					} `json:"metadata"`
				} `json:"state"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if a.Export == "" || json.Unmarshal([]byte(a.Export), &export) != nil {
		return w
	}
	fromExport := len(a.Log) == 0 && a.Live == ""
	for _, m := range export.Messages {
		for _, p := range m.Parts {
			ended := p.Type == "tool" && (p.State.Status == "completed" || p.State.Status == "error")
			if m.Info.Role == "assistant" && p.Type == "tool" && p.State.Status == "running" {
				w.running = append(w.running, runningCall{p.ID, p.Tool, p.State.Input.Command, p.State.Metadata.Output})
			}
			if !fromExport {
				continue
			}
			switch {
			case m.Info.Role != "assistant":
				if p.Type == "text" {
					w.user = append(w.user, strings.TrimSpace(p.Text))
				}
			case p.Type == "text":
				w.text++
			case ended:
				w.tools++
				if p.ID != "" {
					w.ended[p.ID] = true
				}
			case p.Type == "step-finish":
				w.usage++
			}
		}
	}
	return w
}
