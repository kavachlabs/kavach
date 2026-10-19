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
		tl.Steps = append(tl.Steps, TimelineStep{TMs: st.TMs, Kind: st.Kind, Title: clipTo(st.Title, 80), Tokens: st.Tokens, Cost: st.Cost})
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
		{Name: "only-the-agents-work", Check: func() error {
			// Every step is one the agent took, in the file the steps come
			// from: no more text, tool or usage steps than it holds, and
			// never the user's message.
			for _, s := range r.seen {
				w := agentWork(s.in)
				var text, tools, usage int
				for _, st := range s.steps {
					switch st.Kind {
					case KindText:
						text++
						for _, u := range w.user {
							if u != "" && st.Title == clip(u) {
								return fmt.Errorf("%s: the user's message is shown as a step", s.id)
							}
						}
					case KindUsage:
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
}

// agentWork reads the first of the event log, its live copy and the session
// export that the attempt has.
func agentWork(a AttemptInput) work {
	var w work
	countEvent := func(line string) {
		var ev struct {
			Type string `json:"type"`
		}
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil {
			return
		}
		switch ev.Type {
		case "text":
			w.text++
		case "tool_use", "error":
			w.tools++
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
	case a.Export != "":
		var export struct {
			Messages []struct {
				Info struct {
					Role string `json:"role"`
				} `json:"info"`
				Parts []struct {
					Type  string `json:"type"`
					Text  string `json:"text"`
					State struct {
						Status string `json:"status"`
					} `json:"state"`
				} `json:"parts"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(a.Export), &export) != nil {
			return w
		}
		for _, m := range export.Messages {
			for _, p := range m.Parts {
				switch {
				case m.Info.Role != "assistant":
					if p.Type == "text" {
						w.user = append(w.user, strings.TrimSpace(p.Text))
					}
				case p.Type == "text":
					w.text++
				case p.Type == "tool" && (p.State.Status == "completed" || p.State.Status == "error"):
					w.tools++
				case p.Type == "step-finish":
					w.usage++
				}
			}
		}
	}
	return w
}
