package replay_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// quoter queries a gateway and a config value for every input. For the input
// "c", a rejecting version panics when the flag is on, and more makes it read
// more than the journal holds.
type quoter struct {
	pair   string // what it asks the gateway
	last   string // what it asks in the last step instead, if set
	reject bool
	more   func(env kavach.Env) []byte
}

func (q *quoter) Handle(env kavach.Env, in kavach.Input) error {
	last := string(in.Data) == "c"
	pair := q.pair
	if last && q.last != "" {
		pair = q.last
	}
	resp, err := env.Query("fx", []byte(pair+string(in.Data)))
	if err != nil {
		return err
	}
	flag, _ := env.Config("flag.strict")
	if q.reject && last && string(flag) == "on" {
		panic("strict mode")
	}
	if q.more != nil && last {
		resp = append(resp, q.more(env)...)
	}
	env.Emit("quotes", append(resp, flag...))
	return nil
}

func recordQuotes(t *testing.T, h kavach.Handler) *journal.Journal {
	t.Helper()
	dir := t.TempDir()
	r := newRecorder(t, h, kavach.Options{
		Service: "quotes", Dir: dir, RecoverPanics: true,
		Gateway: func(gateway string, request []byte) ([]byte, error) { return []byte("rate:" + string(request)), nil },
		Config:  func(key string) ([]byte, string, bool) { return []byte("on"), "test", key == "flag.strict" },
	})
	for _, d := range []string{"a", "b", "c"} {
		r.Step(kavach.Input{Source: "s", Data: []byte(d)})
	}
	if err := r.Flush(true); err != nil {
		t.Fatal(err)
	}
	// The incident's fixture when the last step failed, else the journal itself.
	path := r.File()
	if m, _ := filepath.Glob(filepath.Join(dir, "fixtures", "*.kavach")); len(m) == 1 {
		path = m[0]
	}
	j, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestGatewayAndConfigAreRecordedAndReplayed(t *testing.T) {
	j := recordQuotes(t, &quoter{pair: "eur", reject: true})
	var gw, cfg int
	for _, r := range j.Records {
		switch r.Type {
		case journal.TypeGateway:
			gw++
			if r.Flags&journal.FlagCritical == 0 || r.Scope != journal.ScopeRemote {
				t.Errorf("gateway record %+v", r)
			}
		case journal.TypeConfig:
			cfg++
			if r.Flags&journal.FlagCritical == 0 || !r.Present || r.Source != "test" {
				t.Errorf("config record %+v", r)
			}
		}
	}
	if gw != 3 || cfg != 3 {
		t.Fatalf("recorded %d gateway and %d config records", gw, cfg)
	}

	res, err := replay.Run(j, func() kavach.Handler { return &quoter{pair: "eur", reject: true} })
	if err != nil || res.String() != "still_failing@9" {
		t.Fatalf("%v %v", res, err)
	}
	res, err = replay.Run(j, func() kavach.Handler { return &quoter{pair: "eur"} })
	if err != nil || res.String() != "fixed" || res.Steps[2].Synthesized != 0 {
		t.Fatalf("%v %v %+v", res, err, res.Steps)
	}
}

func TestChangedRequestIsNondeterministic(t *testing.T) {
	j := recordQuotes(t, &quoter{pair: "eur"})
	res, err := replay.Run(j, func() kavach.Handler { return &quoter{pair: "gbp"} })
	if err != nil || res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "queried fx") {
		t.Fatalf("%v %v", res, err)
	}
}

func TestLenientStepSynthesizesReads(t *testing.T) {
	j := recordQuotes(t, &quoter{pair: "eur", reject: true})
	j.Records[0].Facts = append(j.Records[0].Facts, journal.Fact{Key: "flag.beta", Form: journal.FactValue, Value: []byte("yes")})
	replayStep := func(more func(kavach.Env) []byte) replay.StepResult {
		t.Helper()
		res, err := replay.Run(j, func() kavach.Handler { return &quoter{pair: "eur", more: more} })
		if err != nil || res.Status != replay.StatusFixed {
			t.Fatalf("%v %v", res, err)
		}
		return res.Steps[2]
	}

	// A query whose request differs is served by the step's unread record and
	// counted.
	res, err := replay.Run(j, func() kavach.Handler { return &quoter{pair: "eur", last: "gbp"} })
	if err != nil || res.String() != "fixed" || res.Steps[2].Synthesized != 1 || string(res.Steps[2].Outputs[0].Data) != "rate:eurcon" {
		t.Fatalf("%v %v %+v", res, err, res.Steps[2])
	}

	// With no record left, a query fails.
	st := replayStep(func(env kavach.Env) []byte {
		a, _ := env.Query("fx", []byte("zzz"))
		_, err := env.Query("fx", []byte("zzz"))
		return append(a, err.Error()...)
	})
	if st.Synthesized != 2 || string(st.Outputs[0].Data) != "rate:eurckavach: no recorded responseon" {
		t.Fatalf("%+v %q", st, st.Outputs[0].Data)
	}

	// A config key the step has no record of is served from the environment.
	st = replayStep(func(env kavach.Env) []byte {
		v, _ := env.Config("beta")
		w, ok := env.Config("nothing")
		return append(append(v, w...), boolByte(ok))
	})
	if st.Synthesized != 2 || string(st.Outputs[0].Data) != "rate:eurcyesfon" {
		t.Fatalf("%+v %q", st, st.Outputs[0].Data)
	}
}

func boolByte(b bool) byte {
	if b {
		return 't'
	}
	return 'f'
}
