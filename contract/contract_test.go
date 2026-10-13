package contract_test

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/contract"
	"github.com/kavachlabs/kavach/internal/testrecorder"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func TestMain(m *testing.M) { os.Exit(testrecorder.Run(m)) }

// store is a key-value handler. Inputs are {"argv":[...]}. The base build
// knows SET and GET; the feature is INCR.
type store struct {
	kv      map[string]string
	incr    bool // implements INCR
	sloppy  bool // INCR on a non-integer value stores garbage instead of failing
	newNil  bool // GET of a missing key answers differently: a regression
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
		v, ok := s.kv[a[1]]
		switch {
		case ok:
			reply(v)
		case s.newNil:
			reply("(nil)")
		default:
			reply("nil")
		}
	case len(a) == 2 && a[0] == "INCR" && s.incr:
		n, err := strconv.ParseInt(orZero(s.kv[a[1]]), 10, 64)
		if err != nil && !s.sloppy {
			reply("ERR value is not an integer")
			return nil
		}
		v := strconv.FormatInt(n+1, 10)
		if err != nil {
			v = s.kv[a[1]] + "+1"
		}
		s.kv[a[1]] = v
		s.counted[a[1]] = true
		reply(v)
	default:
		reply("ERR unknown command")
	}
	return nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// Invariants: from the spec, "a key INCR has counted holds an integer".
func (s *store) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "counter-holds-integer", Check: func() error {
		for k := range s.counted {
			if _, err := strconv.ParseInt(s.kv[k], 10, 64); err != nil {
				return &json.UnsupportedValueError{Str: k + " holds " + s.kv[k]}
			}
		}
		return nil
	}}}
}

func build(mod func(*store)) contract.ReplayFunc {
	return func(j *journal.Journal, opts replay.Options) (*replay.Result, error) {
		return replay.RunWith(j, func() kavach.Handler {
			s := &store{kv: map[string]string{}, counted: map[string]bool{}}
			mod(s)
			return s
		}, opts)
	}
}

var (
	base       = build(func(s *store) {})
	good       = build(func(s *store) { s.incr = true })
	sloppy     = build(func(s *store) { s.incr, s.sloppy = true, true })
	regressing = build(func(s *store) { s.incr, s.newNil = true, true })
)

// record runs the traffic on the base build and returns the journal's path.
func record(t *testing.T) string {
	t.Helper()
	n := int64(1_700_000_000_000_000_000)
	dir := t.TempDir()
	r := kavach.NewRecorder(&store{kv: map[string]string{}, counted: map[string]bool{}}, kavach.Options{
		Service: "store", Dir: dir,
		Clock: func() time.Time { n += 1_000_000; return time.Unix(0, n) },
		Rand:  rand.New(rand.NewSource(1)),
	})
	defer r.Close()
	for i, argv := range [][]string{
		{"SET", "greeting", "hello"}, {"SET", "n", "41"}, {"GET", "greeting"}, {"GET", "missing"},
		{"INCR", "n"}, {"GET", "n"}, {"INCR", "fresh"},
	} {
		b, _ := json.Marshal(map[string]any{"argv": argv})
		if err := r.Step(kavach.Input{Source: "test", Position: strconv.Itoa(i), Data: b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(true); err != nil {
		t.Fatal(err)
	}
	var m []string
	filepath.WalkDir(dir, func(p string, _ os.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".kavach") {
			m = append(m, p)
		}
		return nil
	})
	if len(m) != 1 {
		t.Fatalf("want one journal under %s, got %v", dir, m)
	}
	return m[0]
}

func newContract(t *testing.T) *contract.Dir {
	t.Helper()
	traffic := record(t)
	work := t.TempDir()
	spec := filepath.Join(work, "SPEC.md")
	os.WriteFile(spec, []byte("INCR adds one to the integer at a key; a missing key counts as 0; a non-integer value is an error.\n"), 0o644)
	d, err := contract.Init(filepath.Join(work, "contract"), contract.InitOptions{
		Spec: spec, Traffic: traffic,
		// The GET of the counter reads what INCR wrote, so its output changes
		// too: a step that depends on the feature is a feature step.
		FeatureMatch: []string{`$.argv[0]=="INCR"`, `$.argv==["GET","n"]`},
		Invariants:   []string{"counter-holds-integer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func check(t *testing.T, d *contract.Dir, newBuild contract.ReplayFunc) *contract.Report {
	t.Helper()
	r, err := d.Check(contract.CheckOptions{Old: base, New: newBuild, Parallel: 4})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFeatureMatchSelectsSteps(t *testing.T) {
	d := newContract(t)
	if got := len(d.Features); got != 3 {
		t.Fatalf("feature steps: got %v, want the two INCR inputs and the GET of the counter", d.FeatureSeqs())
	}
}

func TestCheckVerdicts(t *testing.T) {
	d := newContract(t)

	r := check(t, d, base)
	if !strings.HasPrefix(r.Verdict, "unreached@") || len(r.Unreached) != 3 {
		t.Errorf("base build: got %s, unreached %v", r.Verdict, r.Unreached)
	}

	r = check(t, d, regressing)
	if !strings.HasPrefix(r.Verdict, "regression@") {
		t.Errorf("regressing build: got %s (%s)", r.Verdict, r.Detail)
	}

	r = check(t, d, sloppy)
	if !strings.HasPrefix(r.Verdict, "variant_failed(") {
		t.Errorf("sloppy build: got %s (%s); a variant that INCRs the string key must break counter-holds-integer", r.Verdict, r.Detail)
	}
	if len(r.VariantFailures) == 0 || r.VariantFailures[0].File == "" {
		t.Errorf("sloppy build: failing variants not saved: %+v", r.VariantFailures)
	}
	argVariant := false
	for _, f := range r.VariantFailures {
		argVariant = argVariant || strings.Contains(f.Mutation, `"argv"[1] "n" → "greeting"`)
	}
	if !argVariant {
		t.Errorf("sloppy build: no failing variant moves INCR to the string key: %+v", r.VariantFailures)
	}

	r = check(t, d, good)
	if r.Verdict != "unapproved" || len(r.Changed) != 3 {
		t.Errorf("good build, unapproved: got %s, changed %d", r.Verdict, len(r.Changed))
	}
	if _, err := d.Approve(false, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	r = check(t, d, good)
	if !r.Passed || r.Verdict != "pass" {
		t.Errorf("good build, approved: got %s (%s)", r.Verdict, r.Detail)
	}
	if r.VariantsPassed == 0 {
		t.Errorf("good build: no variants passed of %d", r.VariantCandidates)
	}

	os.WriteFile(filepath.Join(d.Path, d.Contract.Spec), []byte("changed\n"), 0o644)
	r = check(t, d, good)
	if r.Verdict != "stale_lock" {
		t.Errorf("after editing the spec: got %s", r.Verdict)
	}
}

func TestStreakCountsRepeatedOutcomes(t *testing.T) {
	d := newContract(t)
	var r *contract.Report
	for i := 0; i < 3; i++ {
		r = check(t, d, regressing)
	}
	if r.Round != 3 || r.Streak != 3 {
		t.Errorf("got round %d streak %d, want 3 and 3", r.Round, r.Streak)
	}
	r = check(t, d, good)
	if r.Streak != 1 {
		t.Errorf("a different outcome: streak %d, want 1", r.Streak)
	}
}

func TestMissingInvariant(t *testing.T) {
	traffic := record(t)
	work := t.TempDir()
	spec := filepath.Join(work, "SPEC.md")
	os.WriteFile(spec, []byte("spec\n"), 0o644)
	d, err := contract.Init(filepath.Join(work, "c"), contract.InitOptions{
		Spec: spec, Traffic: traffic, FeatureMatch: []string{`$.argv[0]=="INCR"`},
		Invariants: []string{"counter-holds-integer", "never-declared"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := check(t, d, good); r.Verdict != "missing_invariant(never-declared)" {
		t.Errorf("got %s", r.Verdict)
	}
}

func TestBaselineMismatch(t *testing.T) {
	d := newContract(t)
	r, err := d.Check(contract.CheckOptions{Old: regressing, New: good})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Verdict, "baseline_mismatch(") {
		t.Errorf("got %s", r.Verdict)
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		expr, data string
		want       bool
	}{
		{`$.argv[0]=="INCR"`, `{"argv":["INCR","n"]}`, true},
		{`$.argv[0]=="INCR"`, `{"argv":["GET","n"]}`, false},
		{`$.argv[5]=="INCR"`, `{"argv":["INCR"]}`, false},
		{`$["event type"]==3`, `{"event type":3.0}`, true},
		{`$.a.b==true`, `{"a":{"b":true}}`, true},
		{`$.a==1`, `not json`, false},
	} {
		m, err := contract.ParseMatch(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := m.Matches([]byte(c.data)); got != c.want {
			t.Errorf("%s on %s: got %v", c.expr, c.data, got)
		}
	}
	for _, bad := range []string{`argv==1`, `$.a=1`, `$.a==nope`, `$[x]==1`} {
		if _, err := contract.ParseMatch(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestParseSteps(t *testing.T) {
	got, err := contract.ParseSteps("3,5-7, 10")
	if err != nil || len(got) != 3 || got[0] != (contract.StepRange{3, 3}) || got[1] != (contract.StepRange{5, 7}) {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := contract.ParseSteps("7-5"); err == nil {
		t.Error("7-5: want an error")
	}
}
