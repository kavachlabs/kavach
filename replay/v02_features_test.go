package replay_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// TestGatewayAndOutputStreamSeparationAndTwoTierMatching verifies:
//   - Moving Gateway reads ahead of Output emits (Lago L1 patch) never causes
//     false diverged or nondeterministic errors.
//   - Concurrent multi-sink emits (e.g. errgroup) in swapped sink order pass,
//     while swapped order within the same sink still reports diverged.
//   - Two-tier Gateway matching: StrictSeq primary, KeyedTemplate fallback
//     (recording ReorderedGateways telemetry and normalizing SQL/Redis templates),
//     and UnmatchedGateway diagnostic for unknown calls on non-lenient steps.
func TestGatewayAndOutputStreamSeparationAndTwoTierMatching(t *testing.T) {
	// Record an unpatched Lago-L1-shaped step:
	//   Output(kafka:events_enriched) -> Gateway(pg:select) -> Output(kafka:events_charged_in_advance) -> Gateway(redis:zadd)
	j := hostJournal(nil,
		journal.Record{Type: journal.TypeInput, Source: "kafka:events", Position: "0:1", Data: []byte(`{"transaction_id":"tx_1","subscription_id":"sub_1"}`)},
		journal.Record{Type: journal.TypeClock, UnixNanos: 1_700_000_000_000_000_000},
		journal.Record{Type: journal.TypeOutput, Sink: "kafka:events_enriched", Data: []byte(`{"transaction_id":"tx_1","kind":"enriched"}`)},
		journal.Record{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: "pg:select", Request: []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["sub_1"]}`), Response: []byte(`[{"id":"ch_1"}]`)},
		journal.Record{Type: journal.TypeOutput, Sink: "kafka:events_charged_in_advance", Data: []byte(`{"transaction_id":"tx_1","charge_id":"ch_1"}`)},
		journal.Record{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: "redis:zadd", Request: []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000000,"sub_1|100"]}`), Response: []byte(`1`)},
	)

	t.Run("lago-l1-reorder-gateway-before-output-and-errgroup-sinks", func(t *testing.T) {
		patched := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				// Read gateways BEFORE clock & outputs, with normalized SQL whitespace
				// and a different Redis ZADD wall-clock score/member.
				if _, err := env.Query("pg:select", []byte(`{"args":["sub_1"],"sql":"select  id  from  charges\nwhere subscription_id = $1 and pay_in_advance is true"}`)); err != nil {
					return err
				}
				if _, err := env.Query("redis:zadd", []byte(`{"cmd":"zadd","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700009999,"sub_1|200"]}`)); err != nil {
					return err
				}
				_ = env.Now()
				// Emit independent sinks in reversed order (simulating errgroup).
				env.Emit("kafka:events_charged_in_advance", []byte(`{"transaction_id":"tx_1","charge_id":"ch_1"}`))
				env.Emit("kafka:events_enriched", []byte(`{"transaction_id":"tx_1","kind":"enriched"}`))
				return nil
			})
		}
		res, err := replay.Run(j, patched)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusOK {
			t.Fatalf("got %s (%s), want ok", res, res.Detail)
		}
		if res.Steps[0].ReorderedGateways != 0 {
			t.Fatalf("in-order gateways reported ReorderedGateways=%d, want 0", res.Steps[0].ReorderedGateways)
		}
	})

	t.Run("reordered-gateways-tier2-telemetry", func(t *testing.T) {
		swapped := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				_ = env.Now()
				// Call redis:zadd BEFORE pg:select (swapped order vs recording).
				if _, err := env.Query("redis:zadd", []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000000,"sub_1|100"]}`)); err != nil {
					return err
				}
				if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["sub_1"]}`)); err != nil {
					return err
				}
				env.Emit("kafka:events_enriched", []byte(`{"transaction_id":"tx_1","kind":"enriched"}`))
				env.Emit("kafka:events_charged_in_advance", []byte(`{"transaction_id":"tx_1","charge_id":"ch_1"}`))
				return nil
			})
		}
		res, err := replay.Run(j, swapped)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusOK {
			t.Fatalf("reordered gateways: got %s (%s), want ok", res, res.Detail)
		}
		if res.Steps[0].ReorderedGateways != 1 {
			t.Fatalf("got ReorderedGateways=%d, want 1", res.Steps[0].ReorderedGateways)
		}
	})

	t.Run("unmatched-gateway-hard-fails-non-lenient", func(t *testing.T) {
		// Different SQL bind argument ("sub_999" != "sub_1") MUST fail with UnmatchedGateway.
		wrongArg := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				_ = env.Now()
				_, err := env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["sub_999"]}`))
				return err
			})
		}
		res, err := replay.Run(j, wrongArg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "UnmatchedGateway") {
			t.Fatalf("wrong arg: got %s (%s), want nondeterministic with UnmatchedGateway", res, res.Detail)
		}

		// Totally unknown extra gateway call after consuming recorded gateways.
		extraCall := func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				_ = env.Now()
				_, _ = env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["sub_1"]}`))
				_, _ = env.Query("redis:zadd", []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000000,"sub_1|100"]}`))
				_, _ = env.Query("http:GET", []byte(`{"url":"https://unknown.example/api"}`))
				return nil
			})
		}
		res, err = replay.Run(j, extraCall)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusNondeterministic || !strings.Contains(res.Detail, "UnmatchedGateway") {
			t.Fatalf("extra call: got %s (%s), want nondeterministic with UnmatchedGateway", res, res.Detail)
		}
	})

	t.Run("same-sink-reordered-outputs-still-diverge", func(t *testing.T) {
		sameSinkJ := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "s", Data: []byte("go")},
			journal.Record{Type: journal.TypeOutput, Sink: "kafka:events", Data: []byte("first")},
			journal.Record{Type: journal.TypeOutput, Sink: "kafka:events", Data: []byte("second")},
		)
		res, err := replay.Run(sameSinkJ, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("kafka:events", []byte("second"))
				env.Emit("kafka:events", []byte("first"))
				return nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusDiverged {
			t.Fatalf("reordered same-sink outputs: got %s, want diverged", res)
		}
	})

	t.Run("lenient-sql-fallback-matches-same-normalized-op-never-cross-schema", func(t *testing.T) {
		// Reproduces Hatchet H1 pattern:
		//   1. ListQueueItemsForQueue (18 cols, minId=0)
		//   2. GetMinUnprocessedQueueItemId (1 col, returns minId=101)
		//   3. ListQueueItemsForQueue (18 cols, minId=101)
		// In lenient mode (variant), if the 2nd ListQueueItemsForQueue fires
		// with shifted key_args ["0"] BEFORE GetMinUnprocessedQueueItemId, it
		// must match unread #3 (18-col ListQueueItemsForQueue), NOT unread #2
		// (1-col GetMinUnprocessedQueueItemId), and an unknown SQL query must
		// NOT steal #2's 1-col schema either.
		listSQL := "SELECT id, tenant_id, step_run_id FROM v1_queue_item WHERE id >= ?"
		minSQL := "SELECT MIN(id) FROM v1_queue_item WHERE tenant_id = ?"
		unknownSQL := "SELECT count(*) FROM v1_workers WHERE tenant_id = ?"

		vj := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "hatchet", Data: []byte(`{"op":"schedule"}`)},
			journal.Record{
				Type:     journal.TypeGateway,
				Flags:    journal.FlagCritical,
				Gateway:  "pg:select",
				Request:  []byte(`{"driver":"pg","normalized_op":"` + listSQL + `","key_args":["0"]}`),
				Response: []byte(`{"cols":18,"rows":["r1"]}`),
			},
			journal.Record{
				Type:     journal.TypeGateway,
				Flags:    journal.FlagCritical,
				Gateway:  "pg:select",
				Request:  []byte(`{"driver":"pg","normalized_op":"` + minSQL + `","key_args":["t1"]}`),
				Response: []byte(`{"cols":1,"min_id":101}`),
			},
			journal.Record{
				Type:     journal.TypeGateway,
				Flags:    journal.FlagCritical,
				Gateway:  "pg:select",
				Request:  []byte(`{"driver":"pg","normalized_op":"` + listSQL + `","key_args":["101"]}`),
				Response: []byte(`{"cols":18,"rows":["r2"]}`),
			},
		)
		vj.Header.Meta.Variant = &journal.Variant{ID: 1, Mutation: "shifted min_id", Incident: 1}

		res, err := replay.Run(vj, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				// Call 1: exact match for #1 (minId=0).
				r1, err := env.Query("pg:select", []byte(`{"driver":"pg","normalized_op":"`+listSQL+`","key_args":["0"]}`))
				if err != nil || string(r1) != `{"cols":18,"rows":["r1"]}` {
					return fmt.Errorf("call 1 got %q, %v", r1, err)
				}
				// Call 2: shifted key_args ["0"] BEFORE GetMinUnprocessedQueueItemId.
				// Must match unread #3 (18-col), NOT unread #2 (1-col)!
				r2, err := env.Query("pg:select", []byte(`{"driver":"pg","normalized_op":"`+listSQL+`","key_args":["0"]}`))
				if err != nil || string(r2) != `{"cols":18,"rows":["r2"]}` {
					return fmt.Errorf("call 2 got %q, %v; expected 18-col #3", r2, err)
				}
				// Call 3: unknown SQL must NOT steal unread #2 (1-col minSQL)!
				if _, err := env.Query("pg:select", []byte(`{"driver":"pg","normalized_op":"`+unknownSQL+`","key_args":["t1"]}`)); err == nil || !strings.Contains(err.Error(), "no recorded response") {
					return fmt.Errorf("call 3 unknown SQL expected no recorded response, got %v", err)
				}
				// Call 4: extra ListQueueItemsForQueue reuses same-SQL 18-col schema without stealing #2!
				r4, err := env.Query("pg:select", []byte(`{"driver":"pg","normalized_op":"`+listSQL+`","key_args":["202"]}`))
				if err != nil || string(r4) != `{"cols":18,"rows":["r2"]}` {
					return fmt.Errorf("call 4 extra listSQL got %q, %v", r4, err)
				}
				// Call 5: GetMinUnprocessedQueueItemId still gets unread #2 (1-col) cleanly!
				r5, err := env.Query("pg:select", []byte(`{"driver":"pg","normalized_op":"`+minSQL+`","key_args":["t1"]}`))
				if err != nil || string(r5) != `{"cols":1,"min_id":101}` {
					return fmt.Errorf("call 5 minSQL got %q, %v", r5, err)
				}
				return nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusOK {
			t.Fatalf("lenient SQL fallback: got %s (%s), want ok", res, res.Detail)
		}
	})
}

// lagoSim models Lago events-processor's exact L1 bug, its fix, and a lobotomized fix.
type lagoMode int

const (
	lagoOld lagoMode = iota
	lagoFixed
	lagoLobotomized
)

func newLagoSim(mode lagoMode) func() kavach.Handler {
	return func() kavach.Handler {
		return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
			var ev struct {
				TransactionID  string `json:"transaction_id"`
				SubscriptionID string `json:"subscription_id"`
				Code           string `json:"code"`
				Units          int    `json:"units"`
			}
			if err := json.Unmarshal(in.Data, &ev); err != nil {
				return err
			}
			if ev.TransactionID == "" || ev.Code == "" || ev.Units <= 0 {
				return fmt.Errorf("invalid event payload")
			}
			_ = env.Now()
			// Pre-emit Postgres lookups (billable_metrics + subscriptions).
			if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT * FROM billable_metrics WHERE code = $1","args":["`+ev.Code+`"]}`)); err != nil {
				return err
			}
			if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT * FROM subscriptions WHERE id = $1","args":["`+ev.SubscriptionID+`"]}`)); err != nil {
				return err
			}

			enriched, _ := json.Marshal(map[string]any{
				"transaction_id":  ev.TransactionID,
				"subscription_id": ev.SubscriptionID,
				"units":           ev.Units,
			})
			charged, _ := json.Marshal(map[string]any{
				"transaction_id": ev.TransactionID,
				"charge_id":      "ch_" + ev.SubscriptionID,
			})

			switch mode {
			case lagoOld:
				// BUG (Lago L1): emits enriched BEFORE charges query, and emits
				// charged_in_advance BEFORE Redis FlagSubscriptionRefresh!
				env.Emit("kafka:events_enriched", enriched)
				if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
				env.Emit("kafka:events_charged_in_advance", charged)
				if _, err := env.Query("redis:zadd", []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000000,"`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
			case lagoFixed:
				// FIX: run all Postgres/Redis gateways BEFORE emitting to Kafka!
				if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
				if _, err := env.Query("redis:zadd", []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000001,"`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
				env.Emit("kafka:events_charged_in_advance", charged)
				env.Emit("kafka:events_enriched", enriched)
			case lagoLobotomized:
				// Bad "fix": checks gateways, silently drops Kafka emits!
				if _, err := env.Query("pg:select", []byte(`{"sql":"SELECT id FROM charges WHERE subscription_id = $1 AND pay_in_advance IS TRUE","args":["`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
				if _, err := env.Query("redis:zadd", []byte(`{"cmd":"ZADD","key":"subscription_refreshed_v2","args":["subscription_refreshed_v2",1700000001,"`+ev.SubscriptionID+`"]}`)); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

func recordHealthyLagoBaseline(t *testing.T) *journal.Journal {
	t.Helper()
	opts := testOptions(t)
	opts.Gateway = func(gateway string, request []byte) ([]byte, error) {
		if strings.HasPrefix(gateway, "redis") {
			return []byte("1"), nil
		}
		return []byte(`[{"id":"ok"}]`), nil
	}
	r := newRecorder(t, newLagoSim(lagoOld)(), opts)
	if err := feed(t, r,
		`{"transaction_id":"tx_100","subscription_id":"sub_1","code":"api_calls","units":10}`,
		`{"transaction_id":"tx_101","subscription_id":"sub_2","code":"storage_gb","units":25}`,
	); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	if err := r.Flush(true); err != nil {
		t.Fatal(err)
	}
	j, err := journal.ReadFile(r.File())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func writeLagoInvariants(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".kavach", "invariants.yaml")
	inv := &kconfig.InvariantsFile{
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
		},
	}
	if err := kconfig.WriteInvariants(p, inv); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHealthyBaselineDriverFaultInjectionLagoL1(t *testing.T) {
	j := recordHealthyLagoBaseline(t)
	invPath := writeLagoInvariants(t)

	// Verify driver-specific fault messages are generated.
	fvs, err := replay.BaselineFaultVariants(j, 32)
	if err != nil {
		t.Fatal(err)
	}
	var sawRedis, sawPG bool
	for _, fv := range fvs {
		m := fv.Header.Meta.Variant.Mutation
		if strings.Contains(m, "LOADING Redis is loading the dataset in memory") {
			sawRedis = true
		}
		if strings.Contains(m, "SQLSTATE 40001") {
			sawPG = true
		}
	}
	if !sawRedis || !sawPG {
		t.Fatalf("expected both Redis LOADING and PG 40001 fault variants (sawRedis=%v, sawPG=%v)", sawRedis, sawPG)
	}

	// 1. Old (buggy L1) vs New (fixed L1) on healthy baseline with --variants 25 -> FIXED!
	vFixed, err := replay.Verify(j, newLagoSim(lagoOld), newLagoSim(lagoFixed), replay.VerifyOptions{
		MinVariants:    25,
		Parallel:       4,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vFixed.Status != replay.StatusFixed || vFixed.Verdict != "fixed" {
		t.Fatalf("Lago L1 fix: got %s (%s), reproducing=%d/%d passing=%d", vFixed, vFixed.Detail, vFixed.Reproducing, vFixed.Candidates, vFixed.Passing)
	}
	if vFixed.Reproducing < 25 || vFixed.Passing != vFixed.Reproducing {
		t.Fatalf("expected >= 25 reproducing & all passing, got reproducing=%d passing=%d", vFixed.Reproducing, vFixed.Passing)
	}
	if vFixed.ReproducingByKind[replay.KindGatewayFault] < 25 {
		t.Fatalf("expected >= 25 gateway_fault variants, got %+v", vFixed.ReproducingByKind)
	}

	// 2. Same check via VerifyWith when ReplayFunc doesn't receive Options.Invariants directly
	//    (simulating cmdDiff passing oldPool.Run / newPool.Run directly!).
	vPostEval, err := replay.VerifyWith(j,
		func(vj *journal.Journal) (*replay.Result, error) { return replay.Run(vj, newLagoSim(lagoOld)) },
		func(vj *journal.Journal) (*replay.Result, error) { return replay.Run(vj, newLagoSim(lagoFixed)) },
		replay.VerifyOptions{MinVariants: 25, Parallel: 4, InvariantsPath: invPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	if vPostEval.Status != replay.StatusFixed || vPostEval.Reproducing < 25 {
		t.Fatalf("VerifyWith post-eval: got %s (%s), reproducing=%d", vPostEval, vPostEval.Detail, vPostEval.Reproducing)
	}

	// 3. Old vs Old (unfixed!) MUST fail with variant_failed!
	vUnfixed, err := replay.Verify(j, newLagoSim(lagoOld), newLagoSim(lagoOld), replay.VerifyOptions{
		MinVariants:    25,
		Parallel:       4,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vUnfixed.Status != replay.StatusVariantFailed || !strings.Contains(vUnfixed.Detail, "atomic_single_emit_on_retry") {
		t.Fatalf("unfixed build: got %s (%s), want variant_failed(atomic_single_emit_on_retry)", vUnfixed, vUnfixed.Detail)
	}

	// 4. Saved failing fault-injection variant round-trips on disk and reproduces identically!
	var failedJ *journal.Journal
	for _, c := range vUnfixed.Variants {
		if c.Reproduces && !c.Passed {
			failedJ = c.Journal
			break
		}
	}
	if failedJ == nil {
		t.Fatal("expected a saved failing fault variant")
	}
	vPath := filepath.Join(t.TempDir(), "fault.kavach")
	if err := journal.WriteFile(vPath, failedJ.Header.Meta, failedJ.Records); err != nil {
		t.Fatal(err)
	}
	reloaded, err := journal.ReadFile(vPath)
	if err != nil {
		t.Fatal(err)
	}
	rOld, err := replay.RunWith(reloaded, newLagoSim(lagoOld), replay.Options{InvariantsPath: invPath})
	if err != nil {
		t.Fatal(err)
	}
	if rOld.Status != replay.StatusInvariantViolated || rOld.Invariant != "atomic_single_emit_on_retry" {
		t.Fatalf("reloaded fault variant on old: got %s (%s)", rOld, rOld.Detail)
	}
	rNew, err := replay.RunWith(reloaded, newLagoSim(lagoFixed), replay.Options{InvariantsPath: invPath})
	if err != nil {
		t.Fatal(err)
	}
	if rNew.Status != replay.StatusOK {
		t.Fatalf("reloaded fault variant on new: got %s (%s), want ok", rNew, rNew.Detail)
	}

	// 5. Lobotomized fix (drops Kafka emits) is caught by anti_lobotomy!
	vLobo, err := replay.Verify(j, newLagoSim(lagoOld), newLagoSim(lagoLobotomized), replay.VerifyOptions{
		MinVariants:    5,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vLobo.Passed() {
		t.Fatalf("lobotomized fix should fail, got %s", vLobo)
	}

	// 6. Regression PR: Fixed (old) vs Old-buggy (new) MUST be caught as regressed!
	vReg, err := replay.Verify(j, newLagoSim(lagoFixed), newLagoSim(lagoOld), replay.VerifyOptions{
		MinVariants:    5,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vReg.Status != replay.StatusRegressed || !strings.Contains(vReg.Detail, "atomic_single_emit_on_retry") {
		t.Fatalf("regression PR: got %s (%s), want regressed(atomic_single_emit_on_retry)", vReg, vReg.Detail)
	}

	// 7. Clean PR: Fixed vs Fixed -> OK with 0 regressions!
	vClean, err := replay.Verify(j, newLagoSim(lagoFixed), newLagoSim(lagoFixed), replay.VerifyOptions{
		MinVariants:    5,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vClean.Status != replay.StatusOK || vClean.Verdict != "ok" {
		t.Fatalf("clean PR: got %s (%s), want ok", vClean, vClean.Detail)
	}

	// 8. Unparseable field mutation (e.g. naive timestamp mutated to invalid
	//    format -> handler skips emitting & returns nil without reaching any
	//    gateway -> anti_lobotomy fires with FaultsInjected == 0 on BOTH old
	//    and new) must NOT count as reproducing a gateway fault.
	makeParserSim := func(mode lagoMode) func() kavach.Handler {
		return func() kavach.Handler {
			inner := newLagoSim(mode)()
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				var ts struct {
					IngestedAt string `json:"ingested_at"`
				}
				if err := json.Unmarshal(in.Data, &ts); err != nil {
					return nil
				}
				// Naive ISO-8601 timestamp (no timezone suffix, like Lago).
				if ts.IngestedAt != "" && strings.HasSuffix(ts.IngestedAt, "-1") {
					// Silently drop unparseable event before calling any DB/Redis gateway.
					return nil
				}
				return inner.Handle(env, in)
			})
		}
	}
	opts := testOptions(t)
	opts.Gateway = func(gateway string, request []byte) ([]byte, error) {
		if strings.HasPrefix(gateway, "redis") {
			return []byte("1"), nil
		}
		return []byte(`[{"id":"ok"}]`), nil
	}
	rec := newRecorder(t, makeParserSim(lagoOld)(), opts)
	if err := feed(t, rec,
		`{"transaction_id":"tx_200","subscription_id":"sub_1","code":"api_calls","units":10,"ingested_at":"2026-11-25T11:55:22"}`,
		`{"transaction_id":"tx_201","subscription_id":"sub_2","code":"storage_gb","units":25,"ingested_at":"2026-11-25T11:55:23"}`,
	); err != nil {
		t.Fatalf("record parser baseline: %v", err)
	}
	if err := rec.Flush(true); err != nil {
		t.Fatal(err)
	}
	jTS, err := journal.ReadFile(rec.File())
	if err != nil {
		t.Fatal(err)
	}
	vTS, err := replay.Verify(jTS, makeParserSim(lagoOld), makeParserSim(lagoFixed), replay.VerifyOptions{
		MinVariants:    25,
		Parallel:       4,
		InvariantsPath: invPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vTS.Status != replay.StatusFixed || vTS.Reproducing < 25 {
		t.Fatalf("naive-timestamp baseline: got %s (%s), reproducing=%d/%d", vTS, vTS.Detail, vTS.Reproducing, vTS.Candidates)
	}
}

func TestDeclarativeInvariantsAllFourKinds(t *testing.T) {
	t.Run("at_most_once_emit_across_retries_without_on_error_count", func(t *testing.T) {
		j := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte(`{"tx":"tx_1"}`)},
			journal.Record{Type: journal.TypeOutput, Sink: "kafka:billed", Data: []byte(`{"transaction_id":"tx_1"}`)},
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte(`{"tx":"tx_1"}`)},
			journal.Record{Type: journal.TypeOutput, Sink: "kafka:billed", Data: []byte(`{"transaction_id":"tx_1"}`)},
		)
		res, err := replay.RunWith(j, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("kafka:billed", []byte(`{"transaction_id":"tx_1"}`))
				return nil
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name: "once_per_tx",
				Kind: kconfig.KindAtMostOnceEmit,
				Sink: "kafka:billed",
				Key:  "$.transaction_id",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "once_per_tx" || !strings.Contains(res.Detail, "duplicate emit") {
			t.Fatalf("got %s (%s), want invariant_violated(once_per_tx)", res, res.Detail)
		}
	})

	t.Run("at_most_once_emit_on_panic_enforces_on_error_emit_count", func(t *testing.T) {
		j := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("boom")},
			marker(journal.MarkerPanic, "nil ptr"),
		)
		res, err := replay.RunWith(j, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("kafka:billed", []byte(`{"transaction_id":"tx_p"}`))
				panic("nil ptr")
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name:             "no_emit_on_crash",
				Kind:             kconfig.KindAtMostOnceEmit,
				Sink:             "kafka:billed",
				Key:              "$.transaction_id",
				OnErrorEmitCount: kconfig.IntPtr(0),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "no_emit_on_crash" {
			t.Fatalf("got %s (%s), want invariant_violated(no_emit_on_crash)", res, res.Detail)
		}
	})

	t.Run("anti_lobotomy_on_variant_with_explicit_baseline", func(t *testing.T) {
		j := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("ev1")},
			journal.Record{Type: journal.TypeOutput, Sink: "kafka:enriched", Data: []byte(`{"ok":true}`)},
		)
		j.Header.Meta.Variant = &journal.Variant{ID: 1, Mutation: "test", Incident: 1}
		res, err := replay.RunWith(j, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				return nil // silently dropped emit!
			})
		}, replay.Options{
			BaselineOutputs: map[string]int{"kafka:enriched": 1},
			Invariants: []kconfig.InvariantRule{{
				Name:               "no_drop",
				Kind:               kconfig.KindAntiLobotomy,
				Sinks:              []string{"kafka:enriched"},
				MinRatioVsBaseline: 1.0,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "no_drop" {
			t.Fatalf("got %s (%s), want invariant_violated(no_drop)", res, res.Detail)
		}
	})

	t.Run("non_negative_nested_and_wildcard", func(t *testing.T) {
		j := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("debit")},
			journal.Record{Type: journal.TypeOutput, Sink: "ledger:balances", Data: []byte(`{"entries":[{"acct":"a","balance":10},{"acct":"b","balance":-5}]}`)},
		)
		res, err := replay.RunWith(j, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("ledger:balances", []byte(`{"entries":[{"acct":"a","balance":10},{"acct":"b","balance":-5}]}`))
				return nil
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name:  "bal_non_neg",
				Kind:  kconfig.KindNonNegative,
				Sink:  "ledger:balances",
				Field: "$.entries[*].balance",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "bal_non_neg" || !strings.Contains(res.Detail, "-5") {
			t.Fatalf("got %s (%s), want invariant_violated(bal_non_neg)", res, res.Detail)
		}
	})

	t.Run("conservation_per_step_and_keyed", func(t *testing.T) {
		// Balanced per-step passes.
		jOK := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("xfer")},
			journal.Record{Type: journal.TypeOutput, Sink: "ledger:postings", Data: []byte(`{"entries":[{"delta":-100},{"delta":100}]}`)},
		)
		res, err := replay.RunWith(jOK, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("ledger:postings", []byte(`{"entries":[{"delta":-100},{"delta":100}]}`))
				return nil
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name:  "zero_sum",
				Kind:  kconfig.KindConservation,
				Sink:  "ledger:postings",
				Field: "$.entries[*].delta",
			}},
		})
		if err != nil || res.Status != replay.StatusOK {
			t.Fatalf("balanced conservation: got %v, %v; want ok", res, err)
		}

		// Imbalanced per-step fails.
		jBad := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("xfer")},
			journal.Record{Type: journal.TypeOutput, Sink: "ledger:postings", Data: []byte(`{"entries":[{"delta":-100},{"delta":105}]}`)},
		)
		res, err = replay.RunWith(jBad, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				env.Emit("ledger:postings", []byte(`{"entries":[{"delta":-100},{"delta":105}]}`))
				return nil
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name:  "zero_sum",
				Kind:  kconfig.KindConservation,
				Sink:  "ledger:postings",
				Field: "$.entries[*].delta",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "zero_sum" {
			t.Fatalf("imbalanced conservation: got %s (%s), want invariant_violated(zero_sum)", res, res.Detail)
		}

		// Keyed cross-step conservation: balanced passes, unbalanced fails.
		jKeyed := hostJournal(nil,
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("leg1")},
			journal.Record{Type: journal.TypeOutput, Sink: "ledger:leg", Data: []byte(`{"tx_id":"t1","delta":-50}`)},
			journal.Record{Type: journal.TypeInput, Source: "q", Data: []byte("leg2")},
			journal.Record{Type: journal.TypeOutput, Sink: "ledger:leg", Data: []byte(`{"tx_id":"t1","delta":40}`)},
		)
		res, err = replay.RunWith(jKeyed, func() kavach.Handler {
			return kavach.HandlerFunc(func(env kavach.Env, in kavach.Input) error {
				if string(in.Data) == "leg1" {
					env.Emit("ledger:leg", []byte(`{"tx_id":"t1","delta":-50}`))
				} else {
					env.Emit("ledger:leg", []byte(`{"tx_id":"t1","delta":40}`))
				}
				return nil
			})
		}, replay.Options{
			Invariants: []kconfig.InvariantRule{{
				Name:  "tx_conserved",
				Kind:  kconfig.KindConservation,
				Sink:  "ledger:leg",
				Key:   "$.tx_id",
				Field: "$.delta",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != replay.StatusInvariantViolated || res.Invariant != "tx_conserved" {
			t.Fatalf("keyed conservation: got %s (%s), want invariant_violated(tx_conserved)", res, res.Detail)
		}
	})
}

func TestIncidentVariantsIncludeGatewayFaults(t *testing.T) {
	j := hostJournal(nil,
		journal.Record{Type: journal.TypeInput, Source: "http", Data: []byte(`{"op":"charge","amount":50}`)},
		journal.Record{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: "http:POST stripe.com/v1/charges", Request: []byte(`{"amount":50}`), Response: []byte(`{"id":"ch_1"}`)},
		journal.Record{Type: journal.TypeOutput, Sink: "http:response", Data: []byte(`{"ok":true}`)},
		marker(journal.MarkerError, "post-charge failure"),
	)
	vs, err := replay.Variants(j)
	if err != nil {
		t.Fatal(err)
	}
	foundHTTPFault := false
	for _, v := range vs {
		info := v.Header.Meta.Variant
		if info.Kind == replay.KindGatewayFault && strings.Contains(info.Mutation, "context deadline exceeded") {
			foundHTTPFault = true
			break
		}
	}
	if !foundHTTPFault {
		var muts []string
		for _, v := range vs {
			muts = append(muts, fmt.Sprintf("%s:%s", v.Header.Meta.Variant.Kind, v.Header.Meta.Variant.Mutation))
		}
		t.Fatalf("expected http gateway_fault variant with context deadline exceeded, got %v", muts)
	}
}

func TestHostDriverSetsKavachHostEnvWithoutDrift(t *testing.T) {
	j := hostJournal(nil,
		opsInput(`[{"op":"getenv","name":"KAVACH_HOST"}]`),
		trace("1"),
	)
	res, err := replay.RunHost(j, conformanceHost(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != replay.StatusOK {
		t.Fatalf("KAVACH_HOST=1 check: got %s (%s), want ok", res, res.Detail)
	}
	for _, d := range res.Drift {
		if d.Key == "env.KAVACH_HOST" {
			t.Fatalf("env.KAVACH_HOST leaked into Drift: %+v", d)
		}
	}
}
