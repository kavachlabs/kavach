package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// answers serves one step's scripted answers (spec/recorder/sdk/README.md), in
// order, per kind.
type answers struct {
	t *testing.T
	q map[string][]json.RawMessage
}

func (a *answers) pop(kind string) json.RawMessage {
	a.t.Helper()
	if len(a.q[kind]) == 0 {
		a.t.Fatalf("the handler read a %s the case has no answer for", kind)
	}
	v := a.q[kind][0]
	a.q[kind] = a.q[kind][1:]
	return v
}

func (a *answers) b64(kind string) []byte {
	var s string
	json.Unmarshal(a.pop(kind), &s)
	b, _ := base64.StdEncoding.DecodeString(s)
	return b
}

func (a *answers) clock() time.Time {
	var s string
	json.Unmarshal(a.pop("clock"), &s)
	var ns int64
	if err := json.Unmarshal([]byte(s), &ns); err != nil {
		a.t.Fatal(err)
	}
	return time.Unix(0, ns)
}

func (a *answers) Read(p []byte) (int, error) {
	b := a.b64("rand")
	if len(b) != len(p) {
		a.t.Fatalf("case rand answer is %d bytes, handler asked for %d", len(b), len(p))
	}
	return copy(p, b), nil
}

func (a *answers) gateway(string, []byte) ([]byte, error) {
	var g struct {
		Response []byte `json:"response"`
		Error    string `json:"error"`
	}
	json.Unmarshal(a.pop("gateway"), &g)
	if g.Error != "" {
		return nil, errors.New(g.Error)
	}
	return g.Response, nil
}

func (a *answers) config(string) ([]byte, string, bool) {
	var c struct {
		Value []byte `json:"value"`
		Unset bool   `json:"unset"`
	}
	json.Unmarshal(a.pop("config"), &c)
	return c.Value, "case", !c.Unset
}

// TestRecorderCases runs every case of spec/recorder/sdk through the Go SDK,
// with the fake recorder in place of kavach-recorder.
func TestRecorderCases(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	specDir := "../../spec/recorder/sdk"
	cases, _ := filepath.Glob(filepath.Join(specDir, "*.json"))
	if len(cases) != 7 {
		t.Fatalf("found %d cases, want 7", len(cases))
	}
	for _, path := range cases {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var c struct {
				Open struct {
					Service   string `json:"service"`
					Start     string `json:"start"`
					Snapshots bool   `json:"snapshots"`
				} `json:"open"`
				Snapshot string            `json:"snapshot"`
				Flags    map[string]string `json:"flags"`
				Actions  []struct {
					Step *struct {
						Source   string `json:"source"`
						Position string `json:"position"`
						Data     []byte `json:"data"`
					} `json:"step"`
					Answers map[string][]json.RawMessage `json:"answers"`
					Flush   *struct {
						Durable bool `json:"durable"`
					} `json:"flush"`
				} `json:"actions"`
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			h := &handler{}
			if c.Snapshot != "" {
				if err := h.Restore([]byte(c.Snapshot)); err != nil {
					t.Fatal(err)
				}
			}
			a := &answers{t: t}
			opts := kavach.Options{
				Service:         c.Open.Service,
				Start:           c.Open.Start,
				NoSnapshots:     !c.Open.Snapshots,
				RecorderCommand: []string{py, "-I", filepath.Join(specDir, "fake_recorder.py"), path, filepath.Join(t.TempDir(), "result.json")},
				Clock:           a.clock,
				Rand:            a,
				Gateway:         a.gateway,
				Config:          a.config,
			}
			if len(c.Flags) > 0 {
				opts.Flags = func() map[string][]byte {
					m := map[string][]byte{}
					for k, v := range c.Flags {
						m[k] = []byte(v)
					}
					return m
				}
			}
			result := opts.RecorderCommand[len(opts.RecorderCommand)-1]
			opts.RecoverPanics = true
			r := kavach.NewRecorder(h, opts)
			for _, act := range c.Actions {
				switch {
				case act.Step != nil:
					a.q = act.Answers
					r.Step(kavach.Input{Source: act.Step.Source, Position: act.Step.Position, Data: act.Step.Data})
				case act.Flush != nil:
					if err := r.Flush(act.Flush.Durable); err != nil {
						t.Fatal(err)
					}
				}
			}
			r.Close()

			out, err := os.ReadFile(result)
			if err != nil {
				t.Fatalf("the fake recorder wrote no result: %v", err)
			}
			var res struct {
				Pass  bool   `json:"pass"`
				Error string `json:"error"`
			}
			json.Unmarshal(out, &res)
			if !res.Pass {
				t.Fatal(res.Error)
			}
		})
	}
}
