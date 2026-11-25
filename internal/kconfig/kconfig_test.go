package kconfig_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/kconfig"
)

func TestParseAndMarshalConfigRoundTrip(t *testing.T) {
	raw := `
# Kavach service config
---
service: events-processor
mode: driver # zero-memrepo driver wrapping
pii: standard
ring_path: /dev/shm/kavach/events.shm
vault_path: .kavach/capsules
variants: 25
drivers: [pgx, go-redis, franz-go, net/http]
`
	cfg, err := kconfig.ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	want := &kconfig.Config{
		Service:   "events-processor",
		Mode:      kconfig.ModeDriver,
		PII:       kconfig.PIIScrubStandard,
		RingPath:  "/dev/shm/kavach/events.shm",
		VaultPath: ".kavach/capsules",
		Variants:  25,
		Drivers:   []string{"pgx", "go-redis", "franz-go", "net/http"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}

	out, err := kconfig.MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig failed: %v", err)
	}
	back, err := kconfig.ParseConfig(out)
	if err != nil {
		t.Fatalf("ParseConfig(MarshalConfig) failed: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("round-trip mismatch:\ngot  %+v\nwant %+v\nyaml:\n%s", back, want, out)
	}
}

func TestParseConfigBlockDriversAndFileIO(t *testing.T) {
	raw := "service: \"ledger:v2 #main\"\r\nmode: scoped\r\npii: strict\r\ndrivers:\r\n  - pgx/v5\r\n  - 'go-redis/v9'\r\n"
	cfg, err := kconfig.ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg.Service != "ledger:v2 #main" || cfg.Mode != "scoped" || cfg.PII != "strict" ||
		!reflect.DeepEqual(cfg.Drivers, []string{"pgx/v5", "go-redis/v9"}) {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, kconfig.DefaultConfigFile)
	if err := kconfig.WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig failed: %v", err)
	}
	loaded, err := kconfig.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !reflect.DeepEqual(loaded, cfg) {
		t.Fatalf("LoadConfig mismatch:\ngot  %+v\nwant %+v", loaded, cfg)
	}
}

func TestParseAndMarshalSpecInvariants(t *testing.T) {
	// Exact example from frictionless-onboarding-spec.md §4 + non_negative & conservation.
	raw := `
service: events-processor
invariants:
  - name: atomic_single_emit_on_retry
    kind: at_most_once_emit
    sink: kafka:events_charged_in_advance
    key: $.transaction_id
    on_error_emit_count: 0

  - name: no_silent_billing_drop
    kind: anti_lobotomy
    sinks: [kafka:events_enriched, kafka:events_charged_in_advance]
    min_ratio_vs_baseline: 1.0

  - name: balance_never_negative
    kind: non_negative
    sinks:
      - pg:exec
      - kafka:balances
    field: $.balance

  - name: double_entry_zero_sum
    kind: conservation
    sink: ledger:entries
    key: $.tx_id
    field: $.delta
    expr: "sum(delta) == 0"
`
	inv, err := kconfig.ParseInvariants([]byte(raw))
	if err != nil {
		t.Fatalf("ParseInvariants failed: %v", err)
	}
	want := &kconfig.InvariantsFile{
		Service: "events-processor",
		Invariants: []kconfig.InvariantRule{
			{
				Name:             "atomic_single_emit_on_retry",
				Kind:             kconfig.KindAtMostOnceEmit,
				Sink:             "kafka:events_charged_in_advance",
				Key:              "$.transaction_id",
				OnErrorEmitCount: kconfig.IntPtr(0),
			},
			{
				Name:               "no_silent_billing_drop",
				Kind:               kconfig.KindAntiLobotomy,
				Sinks:              []string{"kafka:events_enriched", "kafka:events_charged_in_advance"},
				MinRatioVsBaseline: 1.0,
			},
			{
				Name:  "balance_never_negative",
				Kind:  kconfig.KindNonNegative,
				Sinks: []string{"pg:exec", "kafka:balances"},
				Field: "$.balance",
			},
			{
				Name:  "double_entry_zero_sum",
				Kind:  kconfig.KindConservation,
				Sink:  "ledger:entries",
				Key:   "$.tx_id",
				Field: "$.delta",
				Expr:  "sum(delta) == 0",
			},
		},
	}
	if !reflect.DeepEqual(inv, want) {
		t.Fatalf("parsed mismatch:\ngot  %+v\nwant %+v", inv, want)
	}

	// Ensure on_error_emit_count: 0 is preserved distinctly from nil!
	if inv.Invariants[0].OnErrorEmitCount == nil || *inv.Invariants[0].OnErrorEmitCount != 0 {
		t.Fatalf("expected OnErrorEmitCount=0, got %v", inv.Invariants[0].OnErrorEmitCount)
	}
	if inv.Invariants[1].OnErrorEmitCount != nil {
		t.Fatalf("expected nil OnErrorEmitCount on rule 1, got %v", inv.Invariants[1].OnErrorEmitCount)
	}

	b, err := kconfig.MarshalInvariants(inv)
	if err != nil {
		t.Fatalf("MarshalInvariants failed: %v", err)
	}
	if !strings.Contains(string(b), "on_error_emit_count: 0\n") {
		t.Errorf("expected explicit on_error_emit_count: 0 in YAML:\n%s", b)
	}
	if !strings.Contains(string(b), "min_ratio_vs_baseline: 1.0\n") {
		t.Errorf("expected min_ratio_vs_baseline: 1.0 in YAML:\n%s", b)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, kconfig.DefaultInvariantsFile)
	if err := kconfig.WriteInvariants(path, inv); err != nil {
		t.Fatalf("WriteInvariants failed: %v", err)
	}
	if found := kconfig.FindInvariantsFile(dir); found != path {
		t.Fatalf("FindInvariantsFile = %q, want %q", found, path)
	}
	back, err := kconfig.LoadInvariants(path)
	if err != nil {
		t.Fatalf("LoadInvariants failed: %v", err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("round-trip mismatch:\ngot  %+v\nwant %+v\nyaml:\n%s", back, want, b)
	}
}

func TestParseEmptyInvariantsAndValidationErrors(t *testing.T) {
	inv, err := kconfig.ParseInvariants([]byte("service: demo\ninvariants: []\n"))
	if err != nil || inv.Service != "demo" || len(inv.Invariants) != 0 {
		t.Fatalf("empty invariants failed: %+v, %v", inv, err)
	}
	if found := kconfig.FindInvariantsFile(t.TempDir()); found != "" {
		t.Fatalf("expected empty FindInvariantsFile, got %q", found)
	}

	badConfigs := []string{
		"mode: bogus\n",
		"pii: unknown\n",
		"variants: -3\n",
		"variants: notanumber\n",
		"\tservice: tabbed\n",
		"drivers: [unclosed\n",
	}
	for _, s := range badConfigs {
		if _, err := kconfig.ParseConfig([]byte(s)); err == nil {
			t.Errorf("expected error for config %q", s)
		}
	}

	badInvs := []string{
		"invariants:\n  - kind: at_most_once_emit\n", // missing name
		"invariants:\n  - name: a\n    kind: bogus\n",
		"invariants:\n  - name: a\n    kind: at_most_once_emit\n    on_error_emit_count: -1\n",
		"invariants:\n  - name: a\n    kind: anti_lobotomy\n    min_ratio_vs_baseline: -0.5\n",
		"invariants:\n  - name: a\n    kind: non_negative\n", // missing field
		"invariants:\n  - name: a\n    kind: conservation\n", // missing field
		"invariants:\n  - name: dup\n    kind: at_most_once_emit\n  - name: dup\n    kind: anti_lobotomy\n",
	}
	for _, s := range badInvs {
		if _, err := kconfig.ParseInvariants([]byte(s)); err == nil {
			t.Errorf("expected error for invariants %q", s)
		}
	}
	// Verify os.Stat missing file returns error.
	if _, err := kconfig.LoadInvariants(filepath.Join(t.TempDir(), "missing.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected not-exist error, got %v", err)
	}
}
