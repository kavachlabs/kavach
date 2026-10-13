// Command storehost is a key-value service for the kavach contract tests. Its
// first argument picks a build: base (SET and GET), good (adds INCR), sloppy
// (INCR of a non-integer stores garbage) or regressing (also changes GET of
// a missing key).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

type store struct {
	mode    string
	kv      map[string]string
	counted map[string]bool
}

func (s *store) Handle(env kavach.Env, in kavach.Input) error {
	var cmd struct{ Argv []string }
	if err := json.Unmarshal(in.Data, &cmd); err != nil {
		return err
	}
	reply := func(r string) { env.Emit("reply", []byte(r)) }
	a := cmd.Argv
	switch {
	case len(a) == 3 && a[0] == "SET":
		s.kv[a[1]] = a[2]
		reply("OK")
	case len(a) == 2 && a[0] == "GET":
		if v, ok := s.kv[a[1]]; ok {
			reply(v)
		} else if s.mode == "regressing" {
			reply("(nil)")
		} else {
			reply("nil")
		}
	case len(a) == 2 && a[0] == "INCR" && s.mode != "base":
		cur := s.kv[a[1]]
		if cur == "" {
			cur = "0"
		}
		n, err := strconv.ParseInt(cur, 10, 64)
		v := strconv.FormatInt(n+1, 10)
		if err != nil {
			if s.mode != "sloppy" {
				reply("ERR value is not an integer")
				return nil
			}
			v = cur + "+1"
		}
		s.kv[a[1]], s.counted[a[1]] = v, true
		reply(v)
	default:
		reply("ERR unknown command")
	}
	return nil
}

func (s *store) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "counter-holds-integer", Check: func() error {
		for k := range s.counted {
			if _, err := strconv.ParseInt(s.kv[k], 10, 64); err != nil {
				return fmt.Errorf("%s holds %q", k, s.kv[k])
			}
		}
		return nil
	}}}
}

func main() {
	if len(os.Args) != 3 || os.Args[2] != kavach.HostCommand {
		fmt.Fprintln(os.Stderr, "usage: storehost base|good|sloppy|regressing kavach-host")
		os.Exit(2)
	}
	mode := os.Args[1]
	err := kavach.ServeHost(func() kavach.Handler {
		return &store{mode: mode, kv: map[string]string{}, counted: map[string]bool{}}
	}, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
