package replay_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/testrecorder"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		`./ledger`:                           {"./ledger"},
		`python -m ledger`:                   {"python", "-m", "ledger"},
		`  node   dist/main.js `:             {"node", "dist/main.js"},
		`sh -c 'echo "a b"; exit 3'`:         {"sh", "-c", `echo "a b"; exit 3`},
		`"a \"b\" \$x" c\ d ''`:              {`a "b" $x`, "c d", ""},
		`x$HOME 'a\b' "a\b" *`:               {"x$HOME", `a\b`, `a\b`, "*"},
		`java -jar "/opt/my app/ledger.jar"`: {"java", "-jar", "/opt/my app/ledger.jar"},
	} {
		got, err := replay.SplitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", `a 'b`, `a "b`, `a\`} {
		if got, err := replay.SplitCommand(in); err == nil {
			t.Errorf("%q: %q, want an error", in, got)
		}
	}
}

var (
	buildHost sync.Once
	hostDir   string
	hostBin   string
	hostErr   error
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 4 && os.Args[1] == "pool-host" && os.Args[len(os.Args)-1] == kavach.HostCommand {
		os.Exit(runPoolTestHost(os.Args[2 : len(os.Args)-1]))
	}
	code := testrecorder.Run(m)
	if hostDir != "" {
		os.RemoveAll(hostDir)
	}
	os.Exit(code)
}

func conformanceHost(t *testing.T) string {
	t.Helper()
	buildHost.Do(func() {
		if hostDir, hostErr = os.MkdirTemp("", "kavach-host-"); hostErr != nil {
			return
		}
		hostBin = filepath.Join(hostDir, "conformance-host")
		if out, err := exec.Command("go", "build", "-o", hostBin, "../cmd/kavach-conformance-host").CombinedOutput(); err != nil {
			hostErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if hostErr != nil {
		t.Fatal(hostErr)
	}
	return hostBin
}

func hostJournal(env []journal.Fact, recs ...journal.Record) *journal.Journal {
	all := []journal.Record{{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: env}}
	all = append(all, recs...)
	for i := range all {
		all[i].Seq = uint64(i)
	}
	meta := journal.Meta{Service: "conformance", Start: journal.StartGenesis}
	return &journal.Journal{Header: journal.Header{Major: journal.Major, Minor: journal.Minor, Meta: meta}, Records: all}
}

func opsInput(ops string) journal.Record {
	return journal.Record{Type: journal.TypeInput, Source: "test", Data: []byte(ops)}
}

func trace(s string) journal.Record {
	return journal.Record{Type: journal.TypeOutput, Sink: "trace", Data: []byte(s)}
}

func marker(kind, msg string) journal.Record {
	return journal.Record{Type: journal.TypeMarker, Kind: kind, Message: msg}
}

func TestReplayHostServesReadsAndEnvironment(t *testing.T) {
	sum := sha256.Sum256([]byte("what production had"))
	env := []journal.Fact{
		{Key: "env.KAVACH_CONF_VAR", Form: journal.FactValue, Value: []byte("prod-value")},
		{Key: "env.KAVACH_CONF_SECRET", Form: journal.FactSHA256, Value: sum[:]},
		{Key: "env.KAVACH_CONF_GONE", Form: journal.FactUnset},
		{Key: "host.os", Form: journal.FactValue, Value: []byte("plan9")},
		{Key: "flag.x", Form: journal.FactValue, Value: []byte("on")},
	}
	t.Setenv("KAVACH_CONF_GONE", "set here")
	j := hostJournal(env,
		opsInput(`[{"op":"getenv","name":"KAVACH_CONF_VAR"},{"op":"getenv","name":"KAVACH_CONF_GONE"},{"op":"clock"},{"op":"gateway","gateway":"fx","request":"eur"},{"op":"config","key":"k"},{"op":"emit","sink":"out","data":"d"}]`),
		journal.Record{Type: journal.TypeClock, UnixNanos: 42},
		journal.Record{Type: journal.TypeGateway, Gateway: "fx", Request: []byte("eur"), Response: []byte("1.1")},
		journal.Record{Type: journal.TypeConfig, Key: "k", Present: true, Value: []byte("v")},
		trace("prod-value"), trace(`{"unset":true}`), trace(`{"clock":"42"}`), trace("1.1"), trace("v"),
		journal.Record{Type: journal.TypeOutput, Sink: "out", Data: []byte("d")},
		journal.Record{Type: journal.TypeEnvironment, Facts: []journal.Fact{{Key: "host.sessions.u", Form: journal.FactValue, Value: []byte("0")}}},
		opsInput(`[{"op":"print","text":"not protocol"}]`),
	)
	res, err := replay.RunHost(j, conformanceHost(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "ok" || len(res.Steps) != 2 {
		t.Fatalf("%v: %s %+v", res, res.Detail, res.Steps)
	}
	drift := map[string]replay.Drift{}
	for _, d := range res.Drift {
		drift[d.Key] = d
	}
	if d := drift["host.os"]; d.Recorded != "plan9" || d.Replay == "" {
		t.Errorf("host.os: %+v", d)
	}
	if d := drift["env.KAVACH_CONF_SECRET"]; d.Replay != "(unset)" || !strings.HasPrefix(d.Recorded, "sha256:") {
		t.Errorf("secret: %+v", d)
	}
	if _, ok := drift["env.KAVACH_CONF_VAR"]; ok {
		t.Error("a served variable drifted")
	}
	if _, ok := drift["flag.x"]; ok {
		t.Error("a flag drifted")
	}
	if len(res.EnvChanges) != 1 || res.EnvChanges[0].After != 1 || res.EnvChanges[0].Facts[0].Key != "host.sessions.u" {
		t.Errorf("environment changes: %+v", res.EnvChanges)
	}
}

func TestReplayHostStatuses(t *testing.T) {
	bin := conformanceHost(t)
	for name, c := range map[string]struct {
		recs []journal.Record
		want string
	}{
		"panic": {[]journal.Record{opsInput(`[{"op":"panic","message":"boom"}]`), marker(journal.MarkerPanic, "boom")}, "still_failing@1"},
		"error": {[]journal.Record{opsInput(`[{"op":"error","message":"bad"}]`), marker(journal.MarkerError, "bad")}, "still_failing@1"},
		"invariant": {[]journal.Record{opsInput(`[{"op":"count","n":1000}]`), marker(journal.MarkerInvariant, "below_limit")},
			"invariant_violated(below_limit)@1"},
		"unread read":   {[]journal.Record{opsInput(`[]`), {Type: journal.TypeClock, UnixNanos: 1}}, "nondeterministic@2"},
		"unrecorded":    {[]journal.Record{opsInput(`[{"op":"clock"}]`)}, "nondeterministic@1"},
		"wrong request": {[]journal.Record{opsInput(`[{"op":"gateway","gateway":"g","request":"b"}]`), {Type: journal.TypeGateway, Gateway: "g", Request: []byte("a")}}, "nondeterministic@2"},
		"diverged":      {[]journal.Record{opsInput(`[{"op":"emit","sink":"s","data":"x"}]`), trace("y")}, "diverged@1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := replay.RunHost(hostJournal(nil, c.recs...), bin)
			if err != nil || res.String() != c.want {
				t.Fatalf("%v, %v; want %s (%s)", res, err, c.want, res.Detail)
			}
		})
	}

	// A failure recorded for a step is replayed leniently: the fixed handler
	// reads past the records, and the step passes.
	j := hostJournal(nil, opsInput(`[{"op":"clock"},{"op":"clock"}]`), journal.Record{Type: journal.TypeClock, UnixNanos: 7}, marker(journal.MarkerPanic, "x"))
	res, err := replay.RunHost(j, bin)
	if err != nil || res.String() != "fixed" || res.Steps[0].Synthesized != 1 {
		t.Fatalf("%v, %v", res, err)
	}
}

func script(body string) string { return "sh -c '" + body + "'" }

const ready = `echo "{\"t\":\"ready\",\"protocol\":1,\"invariants\":[],\"environment\":{}}"`

func TestReplayHostFailures(t *testing.T) {
	old := replay.StepTimeout
	replay.StepTimeout = 500 * time.Millisecond
	defer func() { replay.StepTimeout = old }()
	j := hostJournal(nil, opsInput(`[]`), marker(journal.MarkerCrash, ""))

	// A host that exits during a step crashed.
	res, err := replay.RunHost(j, script("read hello; "+ready+"; read step; echo oops >&2; exit 3"))
	if err != nil || res.String() != "still_failing@1" || !strings.Contains(res.Steps[0].Crash, "exit status 3): oops") {
		t.Fatalf("%v, %v: %+v", res, err, res.Steps)
	}

	// A step that takes too long fails and the host is killed.
	start := time.Now()
	res, err = replay.RunHost(j, script("read hello; "+ready+"; read step; sleep 30"))
	if err != nil || res.String() != "still_failing@1" || !strings.HasPrefix(res.Detail, "timeout: ") || res.Steps[0].Timeout == "" || time.Since(start) > 10*time.Second {
		t.Fatalf("%v, %v: %s", res, err, res.Detail)
	}

	// These cannot be replayed at all.
	for name, c := range map[string]struct{ cmd, want string }{
		"exits before ready": {script("echo hi >&2; exit 2"), "exited before ready (exit status 2): hi"},
		"fatal":              {script(`read hello; echo "{\"t\":\"fatal\",\"message\":\"no handler\"}"`), "no handler"},
		"not a message":      {script("read hello; echo hello"), "not a protocol message"},
		"wrong message":      {script("read hello; " + `echo "{\"t\":\"emit\"}"`), `"emit", want ready`},
		"no such command":    {"/nonexistent/host", "starting host"},
		"bad syntax":         {"a 'b", "unterminated"},
		"bad step message":   {script("read hello; " + ready + "; read step; " + `echo "{\"t\":\"ready\"}"`), `"ready" during a step`},
	} {
		if res, err := replay.RunHost(j, c.cmd); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, %v; want an error with %q", name, res, err, c.want)
		}
	}
}

func TestReplayHostTimeTravelAndProbes(t *testing.T) {
	bin := conformanceHost(t)
	j := hostJournal(nil,
		opsInput(`[{"op":"clock"},{"op":"config","key":"mode"},{"op":"count","n":10}]`),
		journal.Record{Type: journal.TypeClock, UnixNanos: 1234},
		journal.Record{Type: journal.TypeConfig, Key: "mode", Present: true, Value: []byte("live")},
		trace(`{"clock":"1234"}`), trace("live"),
		opsInput(`[{"op":"count","n":20}]`),
		opsInput(`[{"op":"panic","message":"crash at step 3"}]`),
		marker(journal.MarkerPanic, "crash at step 3"),
	)
	// Step 1 is at seq 1, step 2 is at seq 6, step 3 (panic) is at seq 7.
	until := uint64(6)
	res, err := replay.RunHostWith(j, bin, replay.Options{
		Until: &until,
		Probes: []kavach.Input{
			{Data: []byte(`[{"op":"clock"},{"op":"config","key":"mode"}]`)},
		},
		OnHold: func(probe func(kavach.Input) (replay.ProbeResult, error)) error {
			// After step 1 (count += 10 + 1 = 11), step 2 (count += 20 + 1 = 32),
			// and probe 1 (count += 1 = 33), adding 966 + 1 = 1000 triggers below_limit.
			_, err := probe(kavach.Input{Data: []byte(`[{"op":"count","n":966}]`)})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusOK || len(res.Steps) != 2 || len(res.Probes) != 2 {
		t.Fatalf("res = %s, steps=%d, probes=%+v", res, len(res.Steps), res.Probes)
	}
	if res.Probes[0].Outcome() != "ok" || len(res.Probes[0].Outputs) != 2 ||
		string(res.Probes[0].Outputs[0].Data) != `{"clock":"1234"}` ||
		string(res.Probes[0].Outputs[1].Data) != "live" {
		t.Fatalf("probe 0 = %+v", res.Probes[0])
	}
	if res.Probes[1].Outcome() != "invariant" || res.Probes[1].Invariant != "below_limit" {
		t.Fatalf("probe 1 = %+v", res.Probes[1])
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func poolHostCmd(t *testing.T, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parts := append([]string{exe, "pool-host"}, args...)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return strings.Join(quoted, " ")
}

func pidLoggedCommand(t *testing.T, cmd, pidLog string) string {
	t.Helper()
	words, err := replay.SplitCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	scriptPath := filepath.Join(t.TempDir(), "wrap.sh")
	body := fmt.Sprintf("#!/bin/sh\necho $$ >> %s\nexec %s \"$@\"\n", shellQuote(pidLog), strings.Join(quoted, " "))
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return shellQuote(scriptPath)
}

func readPIDs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func snapshotJournal(env []journal.Fact, snapshot string, recs ...journal.Record) *journal.Journal {
	all := []journal.Record{{Type: journal.TypeSnapshot, Flags: journal.FlagCritical, Data: []byte(snapshot)}}
	all = append(all, journal.Record{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: env})
	all = append(all, recs...)
	for i := range all {
		all[i].Seq = uint64(i)
	}
	meta := journal.Meta{Service: "conformance", Start: journal.StartSnapshot}
	return &journal.Journal{Header: journal.Header{Major: journal.Major, Minor: journal.Minor, Meta: meta}, Records: all}
}

func runPoolTestHost(args []string) int {
	if len(args) == 0 {
		return 2
	}
	var pidLog, runLog string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pid-log":
			if i+1 >= len(args) {
				return 2
			}
			pidLog = args[i+1]
			i++
		case "--run-log":
			if i+1 >= len(args) {
				return 2
			}
			runLog = args[i+1]
			i++
		default:
			pos = append(pos, args[i])
		}
	}
	if pidLog != "" {
		f, err := os.OpenFile(pidLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 2
		}
		fmt.Fprintln(f, os.Getpid())
		f.Close()
	}
	if len(pos) == 0 {
		return 2
	}
	switch pos[0] {
	case "ops":
		kavach.MaybeReplay(func() kavach.Handler {
			if runLog != "" {
				if f, err := os.OpenFile(runLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
					fmt.Fprintln(f, os.Getpid())
					f.Close()
				}
			}
			return &poolOpsHandler{limit: 100}
		})
	case "verify":
		if len(pos) < 2 {
			return 2
		}
		fixed := pos[1] == "fixed"
		kavach.MaybeReplay(func() kavach.Handler {
			return newVerifyJSONHandler(fixed)
		})
	case "no-snapshot":
		kavach.MaybeReplay(func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("out", in.Data)
				return nil
			})
		})
	case "legacy":
		return runLegacyTestHost()
	case "custom-reset":
		return runCustomResetTestHost()
	}
	return 2
}

type poolOpsHandler struct {
	count int
	limit int
}

func (h *poolOpsHandler) Snapshot() ([]byte, error) {
	return []byte(strconv.Itoa(h.count)), nil
}

func (h *poolOpsHandler) Restore(b []byte) error {
	v, err := strconv.Atoi(string(b))
	if err != nil {
		return err
	}
	h.count = v
	return nil
}

func (h *poolOpsHandler) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{
		Name: "under_limit",
		Check: func() error {
			if h.count >= h.limit {
				return fmt.Errorf("count %d >= %d", h.count, h.limit)
			}
			return nil
		},
	}}
}

func (h *poolOpsHandler) Handle(env kavach.Env, in kavach.Input) error {
	var cmd struct {
		Op   string `json:"op"`
		N    int    `json:"n"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(in.Data, &cmd); err != nil {
		return err
	}
	switch cmd.Op {
	case "ok":
		h.count += cmd.N
		env.Emit("count", []byte(strconv.Itoa(h.count)))
	case "stderr":
		fmt.Fprintln(os.Stderr, cmd.Text)
		env.Emit("count", []byte(strconv.Itoa(h.count)))
	case "crash":
		fmt.Fprintln(os.Stderr, cmd.Text)
		os.Exit(4)
	case "sleep":
		time.Sleep(10 * time.Second)
	default:
		return fmt.Errorf("unknown op %q", cmd.Op)
	}
	return nil
}

type verifyJSONHandler struct {
	fixed bool
	bal   map[string]int64
}

func newVerifyJSONHandler(fixed bool) kavach.Handler {
	return &verifyJSONHandler{fixed: fixed, bal: map[string]int64{}}
}

func (h *verifyJSONHandler) Handle(env kavach.Env, in kavach.Input) error {
	at := env.Now()
	var nonce [4]byte
	env.Read(nonce[:])
	var cmd struct {
		Cmd    string `json:"cmd"`
		From   string `json:"from"`
		To     string `json:"to"`
		Amount *int64 `json:"amount"`
		Memo   string `json:"memo"`
		Flag   bool   `json:"flag"`
	}
	if err := json.Unmarshal(in.Data, &cmd); err != nil {
		return err
	}
	if cmd.Amount == nil {
		if h.fixed {
			env.Emit("out", []byte(fmt.Sprintf("rejected:%s:%s:%s:%x", cmd.Cmd, cmd.From, cmd.To, nonce)))
			return nil
		}
		panic("nil amount")
	}
	h.bal[cmd.From] -= *cmd.Amount
	h.bal[cmd.To] += *cmd.Amount
	env.Emit("out", []byte(fmt.Sprintf("%s:%s->%s=%d:%d:%s:%t:%d:%x",
		cmd.Cmd, cmd.From, cmd.To, *cmd.Amount, h.bal[cmd.From], cmd.Memo, cmd.Flag, at.UnixNano(), nonce)))
	return nil
}

func stdioHostSession(fn func(recv func() (map[string]any, bool), send func(any)) int) int {
	in := bufio.NewReader(os.Stdin)
	send := func(v any) {
		b, _ := json.Marshal(v)
		os.Stdout.Write(append(b, '\n'))
	}
	recv := func() (map[string]any, bool) {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return nil, false
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			return nil, false
		}
		return m, true
	}
	return fn(recv, send)
}

func runLegacyTestHost() int {
	return stdioHostSession(func(recv func() (map[string]any, bool), send func(any)) int {
		hello, ok := recv()
		if !ok || hello["t"] != "hello" {
			return 1
		}
		send(map[string]any{"t": "ready", "protocol": 1, "sdk": "legacy-test/0.1", "invariants": []string{}, "environment": map[string]any{}})
		for {
			m, ok := recv()
			if !ok {
				return 1
			}
			switch m["t"] {
			case "end":
				return 0
			case "step":
				send(map[string]any{"t": "done", "outcome": "ok"})
			default:
				send(map[string]any{"t": "fatal", "message": fmt.Sprintf("unexpected %q between steps", m["t"])})
				return 1
			}
		}
	})
}

func runCustomResetTestHost() int {
	return stdioHostSession(func(recv func() (map[string]any, bool), send func(any)) int {
		hello, ok := recv()
		if !ok || hello["t"] != "hello" {
			return 1
		}
		send(map[string]any{
			"t":           "ready",
			"protocol":    1,
			"sdk":         "custom-test/0.1",
			"invariants":  []string{},
			"environment": map[string]any{"host.os": map[string]any{"value": []byte("plan9")}},
		})
		resets := 0
		for {
			m, ok := recv()
			if !ok {
				return 1
			}
			switch m["t"] {
			case "end":
				return 0
			case "step":
				send(map[string]any{"t": "done", "outcome": "ok"})
			case "reset":
				resets++
				switch resets {
				case 1:
					// Omit environment: driver keeps initial hello environment ("plan9").
					send(map[string]any{"t": "ready", "protocol": 1, "sdk": "custom-test/0.1", "invariants": []string{}})
				case 2:
					// Non-nil environment: driver updates h.environment ("freebsd").
					send(map[string]any{
						"t":           "ready",
						"protocol":    1,
						"sdk":         "custom-test/0.1",
						"invariants":  []string{},
						"environment": map[string]any{"host.os": map[string]any{"value": []byte("freebsd")}},
					})
				default:
					// Unexpected message on 3rd reset: driver evicts and cold-starts cleanly.
					send(map[string]any{"t": "emit", "sink": "x", "data": ""})
					return 1
				}
			default:
				send(map[string]any{"t": "fatal", "message": fmt.Sprintf("unexpected %q between steps", m["t"])})
				return 1
			}
		}
	})
}

func TestHostPoolSequentialReuseAndStateIsolation(t *testing.T) {
	pidLog := filepath.Join(t.TempDir(), "pids.txt")
	cmd := pidLoggedCommand(t, conformanceHost(t), pidLog)
	pool, err := replay.NewHostPool(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	env := []journal.Fact{{Key: "host.os", Form: journal.FactValue, Value: []byte("plan9")}}

	// Run 25 sequential replays mixing genesis state accumulation, invariant
	// failures, snapshot starts, and stderr printing; handler state must never leak.
	for i := 0; i < 25; i++ {
		var j *journal.Journal
		var want string
		switch i % 5 {
		case 0:
			j = hostJournal(env, opsInput(`[{"op":"count","n":600},{"op":"print","text":"noisy-run"}]`))
			want = "ok"
		case 1:
			j = hostJournal(env,
				opsInput(`[{"op":"count","n":999}]`),
				marker(journal.MarkerInvariant, "below_limit"),
			)
			want = "invariant_violated(below_limit)@1"
		case 2:
			// Must not retain failedInvariant or count=1000 from iteration i%5==1.
			j = hostJournal(env, opsInput(`[{"op":"count","n":500}]`))
			want = "ok"
		case 3:
			// Snapshot start restores 990; +5 +1 = 996 < 1000.
			j = snapshotJournal(env, "990", opsInput(`[{"op":"count","n":5}]`))
			want = "ok"
		case 4:
			// Genesis after snapshot must start back at count=0, not 996.
			j = hostJournal(env, opsInput(`[{"op":"count","n":500}]`))
			want = "ok"
		}
		res, err := pool.Run(j)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.String() != want {
			t.Fatalf("run %d: got %s (%s), want %s", i, res, res.Detail, want)
		}
		drift := map[string]replay.Drift{}
		for _, d := range res.Drift {
			drift[d.Key] = d
		}
		if d := drift["host.os"]; d.Recorded != "plan9" || d.Replay == "" {
			t.Fatalf("run %d: expected host.os drift from initial hello environment, got %+v", i, res.Drift)
		}
	}

	pids := readPIDs(t, pidLog)
	if len(pids) != 1 {
		t.Fatalf("expected 1 spawned OS PID across 25 sequential runs, got %d: %v", len(pids), pids)
	}

	// Verify stderr tail is cleared between pooled runs on the exact same OS PID.
	dir := t.TempDir()
	opsPidLog := filepath.Join(dir, "ops.pids")
	opsRunLog := filepath.Join(dir, "ops.runs")
	opsPool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--pid-log", opsPidLog, "--run-log", opsRunLog))
	if err != nil {
		t.Fatal(err)
	}
	defer opsPool.Close()

	countOut := func(n int) journal.Record {
		return journal.Record{Type: journal.TypeOutput, Sink: "count", Data: []byte(strconv.Itoa(n))}
	}

	r1, err := opsPool.Run(hostJournal(nil,
		opsInput(`{"op":"stderr","text":"LEAKED_FROM_RUN1"}`),
		countOut(0),
	))
	if err != nil || r1.Status != replay.StatusOK {
		t.Fatalf("stderr run1: %v, %v", r1, err)
	}

	r2, err := opsPool.Run(hostJournal(nil,
		opsInput(`{"op":"ok","n":1}`),
		countOut(1),
		opsInput(`{"op":"crash","text":"CRASH_RUN2"}`),
		marker(journal.MarkerCrash, ""),
	))
	if err != nil || r2.Status != replay.StatusStillFailing {
		t.Fatalf("stderr run2: %v, %v", r2, err)
	}
	crash := r2.Steps[1].Crash
	if !strings.Contains(crash, "CRASH_RUN2") || strings.Contains(crash, "LEAKED_FROM_RUN1") {
		t.Fatalf("crash detail leaked prior stderr: %q", crash)
	}
	if pids := readPIDs(t, opsPidLog); len(pids) != 1 {
		t.Fatalf("expected 1 spawned PID before crash, got %v", pids)
	}
	if runs := readPIDs(t, opsRunLog); len(runs) != 2 || runs[0] != runs[1] {
		t.Fatalf("expected run2 to reuse run1 PID, got %v", runs)
	}
}

func TestHostPoolVerifyWithVariantsAndRegressions(t *testing.T) {
	opts := testOptions(t)
	rec := newRecorder(t, newVerifyJSONHandler(false), opts)
	inputs := []string{
		`{"cmd":"transfer","from":"alice","to":"bob","amount":10,"memo":"m1","flag":true}`,
		`{"cmd":"transfer","from":"bob","to":"carol","amount":20,"memo":"m2","flag":false}`,
		`{"cmd":"transfer","from":"carol","to":"alice","amount":30,"memo":"m3","flag":true}`,
		`{"cmd":"transfer","from":"alice","to":"dave","amount":40,"memo":"m4","flag":false}`,
		`{"cmd":"transfer","from":"dave","to":"erin","amount":50,"memo":"m5","flag":true}`,
		`{"cmd":"transfer","from":"erin","to":"alice","amount":60,"memo":"m6","flag":false}`,
		`{"cmd":"transfer","from":"alice","to":"bob","amount":70,"memo":"m7","flag":true}`,
		`{"cmd":"transfer","from":"bob","to":"alice","amount":80,"memo":"m8","flag":false}`,
		`{"cmd":"transfer","from":"alice","to":"bob","amount":null,"memo":"boom","flag":true}`,
	}
	err := feed(t, rec, inputs...)
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected panic recording fixture, got %v", err)
	}
	j, err := journal.ReadFile(fixtureIn(t, rec, opts.Dir))
	if err != nil {
		t.Fatal(err)
	}

	oldRaw := poolHostCmd(t, "verify", "old")
	newRaw := poolHostCmd(t, "verify", "fixed")

	want, err := replay.VerifyWith(j,
		func(j *journal.Journal) (*replay.Result, error) { return replay.RunHost(j, oldRaw) },
		func(j *journal.Journal) (*replay.Result, error) { return replay.RunHost(j, newRaw) },
		replay.VerifyOptions{Parallel: 8},
	)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	oldPidLog := filepath.Join(dir, "old.pids")
	newPidLog := filepath.Join(dir, "new.pids")
	oldPool, err := replay.NewHostPool(poolHostCmd(t, "verify", "old", "--pid-log", oldPidLog))
	if err != nil {
		t.Fatal(err)
	}
	defer oldPool.Close()
	newPool, err := replay.NewHostPool(poolHostCmd(t, "verify", "fixed", "--pid-log", newPidLog))
	if err != nil {
		t.Fatal(err)
	}
	defer newPool.Close()

	const par = 4
	got, err := replay.VerifyWith(j, oldPool.Run, newPool.Run, replay.VerifyOptions{Parallel: par})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != replay.StatusFixed {
		t.Fatalf("got status %s (%s), want fixed", got, got.Detail)
	}
	if got.Candidates < 56 {
		t.Fatalf("got %d variant candidates, want >= 56", got.Candidates)
	}
	if got.RegressionCandidates == 0 {
		t.Fatal("expected regression candidates > 0")
	}

	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("pooled VerifyWith differs from cold RunHost:\nwant: %s\ngot:  %s", wantJSON, gotJSON)
	}

	oldPids := readPIDs(t, oldPidLog)
	newPids := readPIDs(t, newPidLog)
	if len(oldPids) == 0 || len(oldPids) > par {
		t.Fatalf("oldPool spawned %d processes (want 1..%d): %v", len(oldPids), par, oldPids)
	}
	if len(newPids) == 0 || len(newPids) > par {
		t.Fatalf("newPool spawned %d processes (want 1..%d): %v", len(newPids), par, newPids)
	}
}

func TestHostPoolStartGenesisAndSnapshotResets(t *testing.T) {
	pidLog := filepath.Join(t.TempDir(), "pids.txt")
	pool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--pid-log", pidLog))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	countOut := func(n int) journal.Record {
		return journal.Record{Type: journal.TypeOutput, Sink: "count", Data: []byte(strconv.Itoa(n))}
	}

	// Genesis -> Snapshot -> Genesis -> Snapshot all reuse the same worker.
	for _, tc := range []struct {
		name string
		j    *journal.Journal
		want string
	}{
		{"genesis-1", hostJournal(nil, opsInput(`{"op":"ok","n":10}`), countOut(10)), "ok"},
		{"snapshot-ok", snapshotJournal(nil, "90", opsInput(`{"op":"ok","n":5}`), countOut(95)), "ok"},
		{"genesis-2", hostJournal(nil, opsInput(`{"op":"ok","n":5}`), countOut(5)), "ok"},
		{"snapshot-inv", snapshotJournal(nil, "95", opsInput(`{"op":"ok","n":5}`), countOut(100)), "invariant_violated(under_limit)@2"},
		{"genesis-3", hostJournal(nil, opsInput(`{"op":"ok","n":1}`), countOut(1)), "ok"},
	} {
		res, err := pool.Run(tc.j)
		if err != nil || res.String() != tc.want {
			t.Fatalf("%s: got %v, %v; want %s", tc.name, res, err, tc.want)
		}
	}
	if pids := readPIDs(t, pidLog); len(pids) != 1 {
		t.Fatalf("expected 1 spawned process across genesis/snapshot resets, got %d: %v", len(pids), pids)
	}

	if _, err := pool.Run(snapshotJournal(nil, "not-an-int", opsInput(`{"op":"ok","n":1}`))); err == nil || !strings.Contains(err.Error(), "restoring snapshot") {
		t.Fatalf("invalid snapshot: got %v, want restoring snapshot error", err)
	}
	res, err := pool.Run(hostJournal(nil, opsInput(`{"op":"ok","n":1}`), countOut(1)))
	if err != nil || res.String() != "ok" {
		t.Fatalf("post-invalid-snapshot genesis: got %v, %v", res, err)
	}

	noSnapPool, err := replay.NewHostPool(poolHostCmd(t, "no-snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	defer noSnapPool.Close()
	if _, err := noSnapPool.Run(hostJournal(nil, opsInput(`hello`), journal.Record{Type: journal.TypeOutput, Sink: "out", Data: []byte("hello")})); err != nil {
		t.Fatal(err)
	}
	if _, err := noSnapPool.Run(snapshotJournal(nil, "1", opsInput(`hello`))); err == nil || !strings.Contains(err.Error(), "Snapshotter") {
		t.Fatalf("no-snapshot handler: got %v, want Snapshotter error", err)
	}
}

func TestHostPoolCrashAndTimeoutEviction(t *testing.T) {
	countOut := func(n int) journal.Record {
		return journal.Record{Type: journal.TypeOutput, Sink: "count", Data: []byte(strconv.Itoa(n))}
	}

	t.Run("crash", func(t *testing.T) {
		dir := t.TempDir()
		pidLog := filepath.Join(dir, "pids.txt")
		runLog := filepath.Join(dir, "runs.txt")
		pool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--pid-log", pidLog, "--run-log", runLog))
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()

		okJ := hostJournal(nil, opsInput(`{"op":"ok","n":1}`), countOut(1))
		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 1: %v, %v", res, err)
		}
		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 2: %v, %v", res, err)
		}

		crashRes, err := pool.Run(hostJournal(nil,
			opsInput(`{"op":"ok","n":1}`),
			countOut(1),
			opsInput(`{"op":"crash","text":"boom"}`),
			marker(journal.MarkerCrash, ""),
		))
		if err != nil || crashRes.Status != replay.StatusStillFailing || !strings.Contains(crashRes.Steps[1].Crash, "boom") {
			t.Fatalf("unexpected crash result: %v, %v (%+v)", crashRes, err, crashRes)
		}

		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 4 after crash: %v, %v", res, err)
		}
		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 5 reuse: %v, %v", res, err)
		}

		pids := readPIDs(t, pidLog)
		runs := readPIDs(t, runLog)
		if len(pids) != 2 || pids[0] == pids[1] {
			t.Fatalf("expected 2 distinct spawned PIDs (old evicted after crash), got %v", pids)
		}
		if len(runs) != 5 || runs[0] != pids[0] || runs[1] != pids[0] || runs[2] != pids[0] || runs[3] != pids[1] || runs[4] != pids[1] {
			t.Fatalf("unexpected run PIDs %v (spawns %v)", runs, pids)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		oldTimeout := replay.StepTimeout
		replay.StepTimeout = 250 * time.Millisecond
		defer func() { replay.StepTimeout = oldTimeout }()

		dir := t.TempDir()
		pidLog := filepath.Join(dir, "pids.txt")
		runLog := filepath.Join(dir, "runs.txt")
		pool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--pid-log", pidLog, "--run-log", runLog))
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()

		okJ := hostJournal(nil, opsInput(`{"op":"ok","n":1}`), countOut(1))
		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 1: %v, %v", res, err)
		}

		toRes, err := pool.Run(hostJournal(nil,
			opsInput(`{"op":"ok","n":1}`),
			countOut(1),
			opsInput(`{"op":"sleep"}`),
			marker(journal.MarkerCrash, ""),
		))
		if err != nil || toRes.Status != replay.StatusStillFailing || toRes.Steps[1].Timeout == "" {
			t.Fatalf("unexpected timeout result: %v, %v (%+v)", toRes, err, toRes)
		}

		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 3 after timeout: %v, %v", res, err)
		}
		if res, err := pool.Run(okJ); err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run 4 reuse: %v, %v", res, err)
		}

		pids := readPIDs(t, pidLog)
		runs := readPIDs(t, runLog)
		if len(pids) != 2 || pids[0] == pids[1] {
			t.Fatalf("expected 2 distinct spawned PIDs (old evicted after timeout), got %v", pids)
		}
		if len(runs) != 4 || runs[0] != pids[0] || runs[1] != pids[0] || runs[2] != pids[1] || runs[3] != pids[1] {
			t.Fatalf("unexpected run PIDs %v (spawns %v)", runs, pids)
		}
	})
}

func TestHostPoolLegacyFallback(t *testing.T) {
	pidLog := filepath.Join(t.TempDir(), "legacy.pids")
	pool, err := replay.NewHostPool(pidLoggedCommand(t, poolHostCmd(t, "legacy"), pidLog))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	j := hostJournal(nil, opsInput(`ignored`))
	for i := 0; i < 5; i++ {
		res, err := pool.Run(j)
		if err != nil || res.Status != replay.StatusOK {
			t.Fatalf("sequential run %d: %v, %v", i, res, err)
		}
	}
	if pids := readPIDs(t, pidLog); len(pids) != 5 {
		t.Fatalf("expected 5 cold spawns after legacy fallback, got %d: %v", len(pids), pids)
	}

	// Concurrent fallback across parallel goroutines must also succeed cleanly.
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := pool.Run(j)
			if err != nil {
				errs[idx] = err
				return
			}
			if res.Status != replay.StatusOK {
				errs[idx] = fmt.Errorf("status %s", res)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("parallel run %d: %v", i, err)
		}
	}
}

func TestHostPoolEnvKeyIsolationAndClose(t *testing.T) {
	dir := t.TempDir()
	runLog := filepath.Join(dir, "runs.txt")
	pool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--run-log", runLog))
	if err != nil {
		t.Fatal(err)
	}

	envA := []journal.Fact{{Key: "env.KAVACH_POOL_KEY", Form: journal.FactValue, Value: []byte("alpha")}}
	envB := []journal.Fact{{Key: "env.KAVACH_POOL_KEY", Form: journal.FactValue, Value: []byte("beta")}}
	countOut := journal.Record{Type: journal.TypeOutput, Sink: "count", Data: []byte("1")}

	runOnce := func(env []journal.Fact) {
		res, err := pool.Run(hostJournal(env, opsInput(`{"op":"ok","n":1}`), countOut))
		if err != nil || res.Status != replay.StatusOK {
			t.Fatalf("run: %v, %v", res, err)
		}
	}

	runOnce(envA)
	runOnce(envB)
	runOnce(envA)
	runOnce(envB)

	runs := readPIDs(t, runLog)
	if len(runs) != 4 || runs[0] == runs[1] || runs[2] != runs[0] || runs[3] != runs[1] {
		t.Fatalf("expected envA->A, envB->B reuse, got PIDs %v", runs)
	}

	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{runs[0], runs[1]} {
		pid, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("idle process %d still exists after Close: %v", pid, err)
		}
	}
	if _, err := pool.Run(hostJournal(envA, opsInput(`{"op":"ok","n":1}`), countOut)); err == nil {
		t.Fatal("expected error calling Run after Close")
	}
}

func TestHostPoolNondeterministicAndFirstRunCrash(t *testing.T) {
	// Crash on the very first run (before resetOK) must evict the dead worker
	// without disabling pooling for subsequent healthy runs.
	dir := t.TempDir()
	pidLog := filepath.Join(dir, "pids.txt")
	runLog := filepath.Join(dir, "runs.txt")
	pool, err := replay.NewHostPool(poolHostCmd(t, "ops", "--pid-log", pidLog, "--run-log", runLog))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	countOut := func(n int) journal.Record {
		return journal.Record{Type: journal.TypeOutput, Sink: "count", Data: []byte(strconv.Itoa(n))}
	}
	crashRes, err := pool.Run(hostJournal(nil,
		opsInput(`{"op":"ok","n":1}`),
		countOut(1),
		opsInput(`{"op":"crash","text":"first-run-crash"}`),
		marker(journal.MarkerCrash, ""),
	))
	if err != nil || crashRes.Status != replay.StatusStillFailing {
		t.Fatalf("first run crash: %v, %v", crashRes, err)
	}
	okJ := hostJournal(nil, opsInput(`{"op":"ok","n":1}`), countOut(1))
	for i := 0; i < 3; i++ {
		res, err := pool.Run(okJ)
		if err != nil || res.Status != replay.StatusOK {
			t.Fatalf("post-crash run %d: %v, %v", i, res, err)
		}
	}
	pids := readPIDs(t, pidLog)
	runs := readPIDs(t, runLog)
	if len(pids) != 2 || pids[0] == pids[1] {
		t.Fatalf("expected 2 spawns (evict crashed first worker, reuse replacement), got %v", pids)
	}
	if len(runs) != 4 || runs[1] != pids[1] || runs[2] != pids[1] || runs[3] != pids[1] {
		t.Fatalf("expected runs 2..4 to reuse %s, got %v", pids[1], runs)
	}

	// Nondeterministic abort on conformanceHost leaves the pool usable on the next Run.
	confPool, err := replay.NewHostPool(conformanceHost(t))
	if err != nil {
		t.Fatal(err)
	}
	defer confPool.Close()
	ndRes, err := confPool.Run(hostJournal(nil, opsInput(`[{"op":"clock"}]`)))
	if err != nil || ndRes.String() != "nondeterministic@1" {
		t.Fatalf("nd run: %v, %v", ndRes, err)
	}
	okRes, err := confPool.Run(hostJournal(nil, opsInput(`[]`)))
	if err != nil || okRes.Status != replay.StatusOK {
		t.Fatalf("post-nd run: %v, %v", okRes, err)
	}
}

func TestHostPoolResetEnvironmentAndInvalidCommands(t *testing.T) {
	for _, bad := range []string{"", "   ", "a 'b"} {
		if _, err := replay.NewHostPool(bad); err == nil {
			t.Errorf("NewHostPool(%q): expected error", bad)
		}
	}

	pidLog := filepath.Join(t.TempDir(), "custom.pids")
	pool, err := replay.NewHostPool(poolHostCmd(t, "custom-reset", "--pid-log", pidLog))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	j := hostJournal([]journal.Fact{{Key: "host.os", Form: journal.FactValue, Value: []byte("linux")}}, opsInput(`ignored`))

	// Run 1 (hello -> plan9): drift shows plan9.
	r1, err := pool.Run(j)
	if err != nil || len(r1.Drift) != 1 || r1.Drift[0].Replay != "plan9" {
		t.Fatalf("run 1 drift: %v, %+v", err, r1)
	}
	// Run 2 (reset 1 omits environment): keeps initial hello environment ("plan9").
	r2, err := pool.Run(j)
	if err != nil || len(r2.Drift) != 1 || r2.Drift[0].Replay != "plan9" {
		t.Fatalf("run 2 drift: %v, %+v", err, r2)
	}
	// Run 3 (reset 2 sends non-nil environment): updates environment ("freebsd").
	r3, err := pool.Run(j)
	if err != nil || len(r3.Drift) != 1 || r3.Drift[0].Replay != "freebsd" {
		t.Fatalf("run 3 drift: %v, %+v", err, r3)
	}
	// Run 4 (reset 3 returns unexpected "emit"): evicts dead worker and cold-starts fresh one ("plan9").
	r4, err := pool.Run(j)
	if err != nil || len(r4.Drift) != 1 || r4.Drift[0].Replay != "plan9" {
		t.Fatalf("run 4 drift: %v, %+v", err, r4)
	}
	if pids := readPIDs(t, pidLog); len(pids) != 2 {
		t.Fatalf("expected 2 spawned PIDs (1 reused across resets 1&2, 1 fresh after reset 3 failure), got %v", pids)
	}
}
