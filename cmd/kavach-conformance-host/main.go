// Command kavach-conformance-host is the Go SDK's host conformance handler
// (SPEC.md §9.6), run against the transcripts in spec/host by host_test.go.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach"
)

func main() {
	kavach.MaybeReplay(func() kavach.Handler { return &handler{} })
	fmt.Fprintln(os.Stderr, "kavach-conformance-host: run with kavach-host as the last argument")
	os.Exit(2)
}

const limit = 1000

type handler struct{ count int }

func (h *handler) Snapshot() ([]byte, error) { return []byte(strconv.Itoa(h.count)), nil }

func (h *handler) Restore(b []byte) (err error) {
	h.count, err = strconv.Atoi(string(b))
	return err
}

func (h *handler) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "below_limit", Check: func() error {
		if h.count >= limit {
			return fmt.Errorf("count %d reached %d", h.count, limit)
		}
		return nil
	}}}
}

type op struct {
	Op      string `json:"op"`
	N       int    `json:"n"`
	Gateway string `json:"gateway"`
	Request string `json:"request"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Sink    string `json:"sink"`
	Data    string `json:"data"`
	Message string `json:"message"`
	Text    string `json:"text"`
}

func (h *handler) Handle(env kavach.Env, in kavach.Input) error {
	var ops []op
	if err := json.Unmarshal(in.Data, &ops); err != nil {
		return err
	}
	trace := func(s string) { env.Emit("trace", []byte(s)) }
	for _, o := range ops {
		switch o.Op {
		case "clock":
			trace(`{"clock":"` + strconv.FormatInt(env.Now().UnixNano(), 10) + `"}`)
		case "rand":
			p := make([]byte, o.N)
			env.Read(p)
			trace(string(p))
		case "gateway":
			resp, err := env.Query(o.Gateway, []byte(o.Request))
			if err != nil {
				trace(`{"error":` + quote(err.Error()) + `}`)
			} else {
				trace(string(resp))
			}
		case "config":
			if v, ok := env.Config(o.Key); ok {
				trace(string(v))
			} else {
				trace(`{"unset":true}`)
			}
		case "getenv":
			if v, ok := os.LookupEnv(o.Name); ok {
				trace(v)
			} else {
				trace(`{"unset":true}`)
			}
		case "emit":
			env.Emit(o.Sink, []byte(o.Data))
		case "panic":
			panic(o.Message)
		case "error":
			return errors.New(o.Message)
		case "print":
			fmt.Println(o.Text)
		case "count":
			h.count += o.N
		default:
			return fmt.Errorf("unknown op %q", o.Op)
		}
	}
	h.count++
	return nil
}

// quote is the JSON string encoding of §9.6, which escapes less than encoding/json.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
