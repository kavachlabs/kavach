package matcher

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeSQL(t *testing.T) {
	q1 := "SELECT id, plan_id FROM subscriptions WHERE organization_id = 'org-1' AND amount > 42 -- comment\nLIMIT $1;"
	q2 := "SELECT  id, plan_id\nFROM subscriptions\nWHERE organization_id = 'org-2' AND amount > 99.5 LIMIT $2"
	n1 := NormalizeSQL(q1)
	n2 := NormalizeSQL(q2)
	if n1 != n2 {
		t.Fatalf("normalized SQL mismatch:\n  q1: %q\n  q2: %q", n1, n2)
	}
	if strings.Contains(n1, "org-1") || strings.Contains(n1, "$1") || strings.Contains(n1, "42") {
		t.Fatalf("literals/placeholders not stripped: %q", n1)
	}
}

func TestTwoTierMatchingStrictSeqKeyedTemplateAndUnmatched(t *testing.T) {
	recorded := []GatewayCall{
		{
			Driver:   "pg",
			Op:       "SELECT id FROM charges WHERE plan_id = $1 AND pay_in_advance IS TRUE",
			KeyArgs:  []string{"plan-vertex"},
			Response: []byte(`{"rows":[{"id":"ch-1"}]}`),
		},
		{
			Driver:   "redis",
			Op:       "ZADD",
			KeyArgs:  []string{"subscription_refreshed_v2"},
			Response: []byte(`1`),
		},
	}

	// 1. StrictSeq match in exact order.
	tbl1 := NewStepTable(9, recorded, nil)
	resp, err, tier := tbl1.MatchGateway(GatewayCall{
		Driver:  "pg",
		Op:      "SELECT id FROM charges WHERE plan_id = $1 AND pay_in_advance IS TRUE",
		KeyArgs: []string{"plan-vertex"},
	})
	if err != nil || tier != TierStrictSeq || string(resp) != `{"rows":[{"id":"ch-1"}]}` {
		t.Fatalf("tier1 pg failed: resp=%q err=%v tier=%v", resp, err, tier)
	}
	resp, err, tier = tbl1.MatchGateway(GatewayCall{
		Driver:  "redis",
		Op:      "ZADD",
		KeyArgs: []string{"subscription_refreshed_v2"},
	})
	if err != nil || tier != TierStrictSeq || string(resp) != "1" {
		t.Fatalf("tier1 redis failed: resp=%q err=%v tier=%v", resp, err, tier)
	}
	if tbl1.ReorderedCount() != 0 {
		t.Fatalf("ReorderedCount = %d, want 0", tbl1.ReorderedCount())
	}

	// 2. Reordered reads (Redis before Postgres) -> KeyedTemplate fallback + telemetry.
	tbl2 := NewStepTable(9, recorded, nil)
	resp, err, tier = tbl2.MatchGateway(GatewayCall{
		Driver:  "redis",
		Op:      "zadd", // case-insensitive command normalization
		KeyArgs: []string{"subscription_refreshed_v2"},
	})
	if err != nil || tier != TierKeyedTemplate || string(resp) != "1" {
		t.Fatalf("tier2 redis failed: resp=%q err=%v tier=%v", resp, err, tier)
	}
	// Next call (Postgres) now hits StrictSeq cursor 0 cleanly.
	resp, err, tier = tbl2.MatchGateway(GatewayCall{
		Driver:  "pg",
		Op:      "SELECT   id FROM charges WHERE plan_id = 'other-literal' AND pay_in_advance IS TRUE",
		KeyArgs: []string{"plan-vertex"},
	})
	if err != nil || tier != TierStrictSeq || string(resp) != `{"rows":[{"id":"ch-1"}]}` {
		t.Fatalf("subsequent pg failed: resp=%q err=%v tier=%v", resp, err, tier)
	}
	if tbl2.ReorderedCount() != 1 {
		t.Fatalf("ReorderedCount = %d, want 1", tbl2.ReorderedCount())
	}
	notes := tbl2.Telemetry()
	if len(notes) != 1 || notes[0].Kind != "reordered_gateway" {
		t.Fatalf("expected reordered_gateway telemetry, got %+v", notes)
	}

	// 3. Brand-new unknown read -> hard fail UnmatchedGatewayError with diagnostics.
	_, err, tier = tbl2.MatchGateway(GatewayCall{
		Driver:  "pg",
		Op:      "SELECT * FROM unknown_table WHERE id = $1",
		KeyArgs: []string{"id-999"},
	})
	if tier != TierUnmatched {
		t.Fatalf("expected TierUnmatched, got %v", tier)
	}
	var uge *UnmatchedGatewayError
	if !errors.As(err, &uge) {
		t.Fatalf("expected *UnmatchedGatewayError, got %T (%v)", err, err)
	}
	if !strings.Contains(uge.Error(), "unknown_table") || !strings.Contains(uge.Error(), "id-999") {
		t.Fatalf("diagnostic missing query/keys: %s", uge.Error())
	}
}

func TestSeparateGatewayAndOutputTablesLagoL1Reorder(t *testing.T) {
	// Recorded unpatched Lago L1 order: Output(enriched) -> Gateway(pg) -> Output(advance) -> Gateway(redis)
	recordedGW := []GatewayCall{
		{Driver: "pg", Op: "SELECT id FROM charges WHERE plan_id = $1", KeyArgs: []string{"plan-1"}, Response: []byte(`true`)},
		{Driver: "redis", Op: "ZADD", KeyArgs: []string{"subscription_refreshed_v2"}, Response: []byte(`1`)},
	}
	recordedOut := []OutputRecord{
		{Sink: "kafka:events_enriched", Data: []byte(`{"transaction_id":"tx-1"}`)},
		{Sink: "kafka:events_charged_in_advance", Data: []byte(`{"transaction_id":"tx-1"}`)},
	}

	// Patched order runs BOTH Gateways BEFORE BOTH Outputs.
	tbl := NewStepTable(9, recordedGW, recordedOut)
	if _, err, _ := tbl.MatchGateway(recordedGW[0]); err != nil {
		t.Fatalf("pg gateway: %v", err)
	}
	if _, err, _ := tbl.MatchGateway(recordedGW[1]); err != nil {
		t.Fatalf("redis gateway: %v", err)
	}
	tbl.RecordOutput(recordedOut[0])
	tbl.RecordOutput(recordedOut[1])

	if err := tbl.CompareOutputs(); err != nil {
		t.Fatalf("CompareOutputs failed after gateway-before-output reorder: %v", err)
	}
}

func TestFaultInjectionAndRetry(t *testing.T) {
	recorded := []GatewayCall{
		{Driver: "redis", Op: "ZADD", KeyArgs: []string{"subscription_refreshed_v2"}, Response: []byte(`1`)},
		{Driver: "pg", Op: "SELECT balance FROM wallets WHERE id = $1", KeyArgs: []string{"w-1"}, Response: []byte(`100`)},
		{Driver: "http", Op: "POST /webhooks", KeyArgs: []string{"wh-1"}, Response: []byte(`200`)},
	}

	tbl := NewStepTable(1, recorded, nil)
	tbl.InjectFault(FaultRule{Driver: "redis", ErrString: CodeRedisLoading, Once: true})
	tbl.InjectFault(FaultRule{Driver: "pg", ErrString: CodePG40001, Once: true})
	tbl.InjectFault(FaultRule{Driver: "http", ErrString: CodeETIMEDOUT, Once: true})

	// 1st attempt on Redis hits REDIS_LOADING fault.
	_, err, tier := tbl.MatchGateway(recorded[0])
	if tier != TierFaultInjected || !IsFault(err, CodeRedisLoading) || err.Error() != FaultRedisLoading {
		t.Fatalf("expected REDIS_LOADING fault, got err=%v tier=%v", err, tier)
	}
	// Retry on Redis succeeds cleanly because Once=true and recorded slot wasn't consumed.
	resp, err, tier := tbl.MatchGateway(recorded[0])
	if err != nil || tier != TierStrictSeq || string(resp) != "1" {
		t.Fatalf("retry after fault failed: resp=%q err=%v tier=%v", resp, err, tier)
	}

	// PG_40001 and ETIMEDOUT
	_, err, _ = tbl.MatchGateway(recorded[1])
	if !IsFault(err, CodePG40001) {
		t.Fatalf("expected PG_40001, got %v", err)
	}
	_, err, _ = tbl.MatchGateway(recorded[2])
	if !IsFault(err, CodeETIMEDOUT) {
		t.Fatalf("expected ETIMEDOUT, got %v", err)
	}
}
