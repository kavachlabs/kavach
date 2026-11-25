package invariants

import (
	"errors"
	"testing"
)

func TestParseYAMLAllKinds(t *testing.T) {
	raw := `
# Kavach declarative invariants
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

  - name: no_negative_balance
    kind: non_negative
    sink: ledger:balance
    field: $.balance

  - name: transfer_conserves_money
    kind: conservation
    sinks:
      - ledger:posting
    field: $.entries[*].delta
`
	f, err := ParseYAML([]byte(raw))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if f.Service != "events-processor" {
		t.Fatalf("service = %q, want events-processor", f.Service)
	}
	if len(f.Invariants) != 4 {
		t.Fatalf("got %d invariants, want 4", len(f.Invariants))
	}
	if f.Invariants[0].OnErrorEmitCount == nil || *f.Invariants[0].OnErrorEmitCount != 0 {
		t.Fatalf("expected on_error_emit_count=0, got %v", f.Invariants[0].OnErrorEmitCount)
	}
	if len(f.Invariants[1].Sinks) != 2 || f.Invariants[1].Sinks[1] != "kafka:events_charged_in_advance" {
		t.Fatalf("inline sinks mismatch: %v", f.Invariants[1].Sinks)
	}
	if len(f.Invariants[3].Sinks) != 1 || f.Invariants[3].Sinks[0] != "ledger:posting" {
		t.Fatalf("block sinks mismatch: %v", f.Invariants[3].Sinks)
	}
}

func TestEvaluateAtMostOnceEmitOnErrorAndRetry(t *testing.T) {
	zero := 0
	eval := NewEvaluator([]Rule{{
		Name:             "atomic_single_emit_on_retry",
		Kind:             KindAtMostOnceEmit,
		Sink:             "kafka:events_charged_in_advance",
		Key:              "$.transaction_id",
		OnErrorEmitCount: &zero,
	}})

	// Unpatched attempt 1: emits before failing on Redis LOADING -> violated!
	v := eval.CheckStep(StepSnapshot{
		Seq:     9,
		Attempt: 1,
		Err:     errors.New("LOADING Redis is loading the dataset in memory"),
		Outputs: []Output{{
			Sink: "kafka:events_charged_in_advance",
			Data: []byte(`{"transaction_id":"tx-vertex-003"}`),
		}},
	})
	if v == nil || v.Name != "atomic_single_emit_on_retry" {
		t.Fatalf("expected on_error_emit_count violation, got %+v", v)
	}

	// Attempt 2 (retry) emits same transaction_id -> ALSO caught as duplicate emit!
	v2 := eval.CheckStep(StepSnapshot{
		Seq:     10,
		Attempt: 2,
		Outputs: []Output{{
			Sink: "kafka:events_charged_in_advance",
			Data: []byte(`{"transaction_id":"tx-vertex-003"}`),
		}},
	})
	if v2 == nil || v2.Name != "atomic_single_emit_on_retry" {
		t.Fatalf("expected duplicate emit violation on retry, got %+v", v2)
	}

	// Reset and test patched sequence: Attempt 1 fails with 0 emits, Attempt 2 succeeds with 1 emit.
	eval.Reset()
	if v := eval.CheckStep(StepSnapshot{Seq: 9, Attempt: 1, Err: errors.New("LOADING")}); v != nil {
		t.Fatalf("patched attempt 1 should pass, got %v", v)
	}
	if v := eval.CheckStep(StepSnapshot{
		Seq:     10,
		Attempt: 2,
		Outputs: []Output{{Sink: "kafka:events_charged_in_advance", Data: []byte(`{"transaction_id":"tx-vertex-003"}`)}},
	}); v != nil {
		t.Fatalf("patched attempt 2 should pass, got %v", v)
	}
}

func TestEvaluateAntiLobotomyNonNegativeConservation(t *testing.T) {
	ratio := 1.0
	eval := NewEvaluator([]Rule{
		{Name: "no_drop", Kind: KindAntiLobotomy, Sinks: []string{"kafka:events_enriched"}, MinRatioVsBaseline: &ratio},
		{Name: "non_neg", Kind: KindNonNegative, Sink: "ledger:balance", Field: "$.balance"},
		{Name: "conserve", Kind: KindConservation, Sink: "ledger:transfer", Field: "$.entries[*].delta"},
	})
	eval.SetBaselineCounts(map[string]int{"kafka:events_enriched": 2})

	// Valid step
	if v := eval.CheckStep(StepSnapshot{
		Seq: 1,
		Outputs: []Output{
			{Sink: "kafka:events_enriched", Data: []byte(`{"id":"1"}`)},
			{Sink: "ledger:balance", Data: []byte(`{"balance":25.50}`)},
			{Sink: "ledger:transfer", Data: []byte(`{"entries":[{"delta":-10},{"delta":10}]}`)},
		},
	}); v != nil {
		t.Fatalf("valid step failed: %v", v)
	}

	// Session anti-lobotomy fails because only 1 of 2 baseline emits happened.
	if v := eval.CheckSession(); v == nil || v.Name != "no_drop" {
		t.Fatalf("expected anti_lobotomy session violation, got %v", v)
	}

	// Negative balance fails non_negative.
	if v := eval.CheckStep(StepSnapshot{
		Seq:     2,
		Outputs: []Output{{Sink: "ledger:balance", Data: []byte(`{"balance":-0.01}`)}},
	}); v == nil || v.Name != "non_neg" {
		t.Fatalf("expected non_negative violation, got %v", v)
	}

	// Unbalanced transfer fails conservation.
	if v := eval.CheckStep(StepSnapshot{
		Seq:     3,
		Outputs: []Output{{Sink: "ledger:transfer", Data: []byte(`{"entries":[{"delta":-10},{"delta":12}]}`)}},
	}); v == nil || v.Name != "conserve" {
		t.Fatalf("expected conservation violation, got %v", v)
	}
}
