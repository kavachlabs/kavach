package replay_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/testrecorder"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
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
	code := testrecorder.Run(m)
	if hostDir != "" {
		os.RemoveAll(hostDir)
	}
	os.Exit(code)
}

// conformanceHost builds the Go conformance host once and returns its path.
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

// script is a host command that is a shell script.
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
	if err != nil || res.String() != "still_failing@1" || !strings.Contains(res.Detail, "within") || time.Since(start) > 10*time.Second {
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
