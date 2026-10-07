package kavach_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
)

func TestFixtureCarriesEnvironment(t *testing.T) {
	t.Setenv("KAVACH_TEST_DB_PASSWORD", "hunter2")
	t.Setenv("KAVACH_TEST_REGION", "eu-1")
	t.Setenv("TZ", "Asia/Kolkata")
	opts := testOptions(t)
	opts.NoScrub = true
	r := kavach.NewRecorder(newWallet(), opts)
	var pe *kavach.PanicError
	if err := feed(t, r, "alice:10", "bob:null"); !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	e := j.Header.Meta.Env
	if e == nil {
		t.Fatal("fixture has no env record")
	}
	if e.Scrubbed {
		t.Error("a NoScrub recording must be marked unscrubbed")
	}
	if e.Vars["KAVACH_TEST_DB_PASSWORD"] != "hunter2" || e.Vars["TZ"] != "Asia/Kolkata" {
		t.Errorf("vars not captured verbatim: %v", e.Vars)
	}
	if e.System == nil || e.System.OS == "" || e.System.GOMAXPROCS < 1 || e.Build == nil || e.Build.GoVersion == "" {
		t.Errorf("system/build facts missing: %+v %+v", e.System, e.Build)
	}
}

func TestNoEnvOption(t *testing.T) {
	opts := testOptions(t)
	opts.NoEnv = true
	r := kavach.NewRecorder(newWallet(), opts)
	err := feed(t, r, "alice:10", "bob:null")
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a panic, got %v", err)
	}
	j, err := journal.ReadFile(pe.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Meta.Env != nil {
		t.Error("NoEnv must leave the header without an env record")
	}
}

func TestReplayEnv(t *testing.T) {
	rec := &journal.Env{Vars: map[string]string{
		"TZ": "UTC", "PATH": "/recorded/bin", "KAVACH_BIN": "/x", "APP_MODE": "prod",
	}}
	base := []string{"TZ=Asia/Tokyo", "PATH=/usr/bin", "KEEP=1"}
	got := strings.Join(kavach.ReplayEnv(base, rec), "\n")
	for _, want := range []string{"TZ=UTC", "PATH=/usr/bin", "KEEP=1", "APP_MODE=prod"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, bad := range []string{"TZ=Asia/Tokyo", "/recorded/bin", "KAVACH_BIN=/x"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in %q", bad, got)
		}
	}
	if out := kavach.ReplayEnv(base, nil); len(out) != len(base) {
		t.Errorf("nil record must leave env alone, got %v", out)
	}
}

func TestEnvDriftNeverPrintsValues(t *testing.T) {
	rec := &journal.Env{Vars: map[string]string{"DB_PASSWORD": "hunter2", "GONE": "x", "SAME": "y"}}
	cur := &journal.Env{Vars: map[string]string{"DB_PASSWORD": "other", "SAME": "y"}}
	d := strings.Join(kavach.EnvDrift(rec, cur), "\n")
	if strings.Contains(d, "hunter2") || strings.Contains(d, "other") {
		t.Fatalf("drift leaked a value: %q", d)
	}
	if !strings.Contains(d, "DB_PASSWORD differs") || !strings.Contains(d, "GONE was set, is unset") || strings.Contains(d, "SAME") {
		t.Errorf("unexpected drift: %q", d)
	}
}
