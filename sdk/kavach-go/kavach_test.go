package kavach

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kavachlabs/kavach-go/clock"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

func TestInitAutoDetection(t *testing.T) {
	t.Setenv("KAVACH_HOST", "")
	t.Setenv("KAVACH_RING_PATH", "")

	// Default -> ModeOff
	kvOff := Init(Config{Service: "svc"})
	if kvOff.Mode() != ModeOff || !kvOff.IsOff() {
		t.Fatalf("expected ModeOff, got %v", kvOff.Mode())
	}
	_ = kvOff.Close()

	// KAVACH_RING_PATH -> ModeRecord
	ringFile := filepath.Join(t.TempDir(), "auto.shm")
	t.Setenv("KAVACH_RING_PATH", ringFile)
	kvRec := Init(Config{Service: "svc", RingCapacity: ring.MinCapacity})
	if kvRec.Mode() != ModeRecord || !kvRec.IsRecord() {
		t.Fatalf("expected ModeRecord, got %v", kvRec.Mode())
	}
	if err := kvRec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// KAVACH_HOST=1 -> ModeReplay (takes precedence over KAVACH_RING_PATH)
	t.Setenv("KAVACH_HOST", "1")
	kvRep := Init(Config{Service: "svc"})
	if kvRep.Mode() != ModeReplay || !kvRep.IsReplay() {
		t.Fatalf("expected ModeReplay, got %v", kvRep.Mode())
	}
	_ = kvRep.Close()

	// detectReplayMode via argv ending in "kavach-host"
	if !detectReplayMode([]string{"./events-processor", "kavach-host"}, func(string) string { return "" }) {
		t.Fatalf("expected detectReplayMode true for trailing kavach-host arg")
	}
}

func TestRecordConcurrentStepsAndDecodeKVRING02(t *testing.T) {
	ringFile := filepath.Join(t.TempDir(), "concurrent.shm")
	kv := Init(Config{
		Service:      "payments",
		RingPath:     ringFile,
		RingCapacity: 256 * 1024,
		PII:          ScrubStandard,
		Mode:         ModeRecord,
	})
	if kv.InitError() != nil {
		t.Fatalf("InitError: %v", kv.InitError())
	}

	// Run 8 concurrent steps to verify atomic per-step ring commits never produce
	// nested inputs or data races.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, sc := kv.BeginStep(context.Background(), Input{
				Source:   "http:POST /charge",
				Position: "req",
				Data:     []byte(`{"transaction_id":"tx-1","email":"bob@example.com"}`),
			})
			_ = kv.Now(ctx)
			_ = kv.Rand(ctx).UUIDv4()
			_, _ = kv.DoGateway(ctx, GatewayCall{
				Driver:  "redis",
				Op:      "ZADD",
				KeyArgs: []string{"sub_refresh"},
			}, func() ([]byte, error) {
				return []byte(`1`), nil
			})
			kv.RecordOutput(ctx, Output{
				Sink: "kafka:events_charged_in_advance",
				Data: []byte(`{"transaction_id":"tx-1","email":"bob@example.com"}`),
			})
			if err := sc.End(nil); err != nil {
				t.Errorf("step %d End: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	if err := kv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Drain ring and verify clean KVRING02 framing + PII scrubbing.
	consumer, err := ring.Open(ringFile)
	if err != nil {
		t.Fatalf("ring.Open: %v", err)
	}
	defer consumer.Close()
	raw, err := consumer.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if strings.Contains(string(raw), "bob@example.com") {
		t.Fatalf("raw email PII leaked into KVRING02 ring")
	}

	var dec ring.FrameDecoder
	if _, err := dec.Feed(raw); err != nil {
		t.Fatalf("FrameDecoder.Feed failed on concurrent steps: %v", err)
	}
	res := dec.FinishEOF()
	if !res.CleanClose {
		t.Fatalf("expected CleanClose=true")
	}
	stepEnds := 0
	for _, f := range res.Frames {
		if f.Kind == ring.FrameStepEnd {
			stepEnds++
		}
	}
	if stepEnds != 8 {
		t.Fatalf("got %d StepEnd frames, want 8", stepEnds)
	}
}

func TestActiveStepFallbackForContextBackgroundGORM(t *testing.T) {
	kv := Init(Config{Service: "lago-events", Mode: ModeReplay})
	defer kv.Close()

	table := matcher.NewStepTable(1, []GatewayCall{
		{
			Driver:   "pg",
			Op:       "SELECT * FROM subscriptions WHERE external_id = $1",
			KeyArgs:  []string{"ext-1"},
			Response: []byte(`{"id":"sub-1"}`),
		},
	}, nil)

	var gotResp []byte
	_, err := kv.RunStepWithTable(context.Background(), Input{Source: "kafka:raw_events", Data: []byte(`{}`)}, table, StepFunc(func(_ context.Context, _ Input) error {
		// Simulate Lago GORM ApiStore calling database/sql with context.Background()!
		var err error
		gotResp, err = kv.ReplayGateway(context.Background(), GatewayCall{
			Driver:  "pg",
			Op:      "SELECT * FROM subscriptions WHERE external_id = $1",
			KeyArgs: []string{"ext-1"},
		})
		return err
	}))
	if err != nil {
		t.Fatalf("RunStepWithTable failed: %v", err)
	}
	if string(gotResp) != `{"id":"sub-1"}` {
		t.Fatalf("got %q, want {\"id\":\"sub-1\"}", gotResp)
	}
	// After step ends, ActiveStep(context.Background()) must return false.
	if _, ok := kv.ActiveStep(context.Background()); ok {
		t.Fatalf("expected no active step after EndStep")
	}
}

func TestLagoL1RetryFaultInjectionAndYAMLInvariants(t *testing.T) {
	invPath := filepath.Join(t.TempDir(), "invariants.yaml")
	yamlContent := `
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
`
	if err := os.WriteFile(invPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	in := Input{
		Source:   "kafka:raw_events",
		Position: "0:9",
		Data:     []byte(`{"transaction_id":"tx-vertex-003"}`),
	}
	recordedGW := []GatewayCall{
		{Driver: "pg", Op: "SELECT id FROM charges WHERE pay_in_advance IS TRUE", KeyArgs: []string{"plan-v"}, Response: []byte(`true`)},
		{Driver: "redis", Op: "ZADD", KeyArgs: []string{"subscription_refreshed_v2"}, Response: []byte(`1`)},
	}
	recordedOut := []matcher.OutputRecord{
		{Sink: "kafka:events_enriched", Data: []byte(`{"transaction_id":"tx-vertex-003"}`)},
		{Sink: "kafka:events_charged_in_advance", Data: []byte(`{"transaction_id":"tx-vertex-003"}`)},
	}

	// Unpatched handler: emits Kafka outputs BEFORE Redis ZADD check!
	unpatched := func(kv *Kavach) StepRunner {
		return StepFunc(func(ctx context.Context, in Input) error {
			kv.RecordOutput(ctx, Output{Sink: "kafka:events_enriched", Data: in.Data})
			if _, err := kv.ReplayGateway(ctx, recordedGW[0]); err != nil {
				return err
			}
			kv.RecordOutput(ctx, Output{Sink: "kafka:events_charged_in_advance", Data: in.Data})
			if _, err := kv.ReplayGateway(ctx, recordedGW[1]); err != nil {
				return err
			}
			return nil
		})
	}

	// Patched handler: runs PG + Redis gateways BEFORE emitting Kafka outputs!
	patched := func(kv *Kavach) StepRunner {
		return StepFunc(func(ctx context.Context, in Input) error {
			if _, err := kv.ReplayGateway(ctx, recordedGW[0]); err != nil {
				return err
			}
			if _, err := kv.ReplayGateway(ctx, recordedGW[1]); err != nil {
				return err
			}
			kv.RecordOutput(ctx, Output{Sink: "kafka:events_enriched", Data: in.Data})
			kv.RecordOutput(ctx, Output{Sink: "kafka:events_charged_in_advance", Data: in.Data})
			return nil
		})
	}

	// 1. Unpatched build MUST FAIL atomic_single_emit_on_retry on Redis LOADING fault!
	kvOld := Init(Config{Service: "events-processor", Mode: ModeReplay, InvariantsPath: invPath})
	kvOld.Invariant("code_single_advance", func(s Step) error {
		return s.AssertAtMostOnceEmit("events_charged_in_advance", "transaction_id")
	})
	tblOld1 := matcher.NewStepTable(9, recordedGW, recordedOut)
	tblOld1.InjectFault(FaultRule{Driver: "redis", ErrString: CodeRedisLoading, Once: true})
	_, errOld1 := kvOld.RunStepWithTable(context.Background(), in, tblOld1, unpatched(kvOld))
	var ie *InvariantError
	if !errors.As(errOld1, &ie) || ie.Name != "atomic_single_emit_on_retry" {
		t.Fatalf("unpatched attempt 1 expected atomic_single_emit_on_retry violation, got %v", errOld1)
	}

	// 2. Patched build: Attempt 1 fails with REDIS_LOADING and 0 emits -> Attempt 2 succeeds with 1 emit + 0 divergence!
	kvNew := Init(Config{Service: "events-processor", Mode: ModeReplay, InvariantsPath: invPath})
	kvNew.Invariant("code_single_advance", func(s Step) error {
		return s.AssertAtMostOnceEmit("events_charged_in_advance", "transaction_id")
	})

	tblNew1 := matcher.NewStepTable(9, recordedGW, recordedOut)
	tblNew1.InjectFault(FaultRule{Driver: "redis", ErrString: CodeRedisLoading, Once: true})
	sc1, errNew1 := kvNew.RunStepWithTable(context.Background(), in, tblNew1, patched(kvNew))
	if !matcher.IsFault(errNew1, CodeRedisLoading) {
		t.Fatalf("patched attempt 1 expected REDIS_LOADING error (no invariant breach), got %v", errNew1)
	}
	if len(sc1.Outputs()) != 0 {
		t.Fatalf("patched attempt 1 leaked %d outputs", len(sc1.Outputs()))
	}

	// Attempt 2 (redelivery after Redis recovers): succeeds and matches recorded outputs!
	tblNew2 := matcher.NewStepTable(10, recordedGW, recordedOut)
	sc2, errNew2 := kvNew.RunStepWithTable(context.Background(), in, tblNew2, patched(kvNew))
	if errNew2 != nil {
		t.Fatalf("patched attempt 2 failed: %v", errNew2)
	}
	if sc2.Attempt() != 2 {
		t.Fatalf("Attempt() = %d, want 2", sc2.Attempt())
	}
	if err := tblNew2.CompareOutputs(); err != nil {
		t.Fatalf("patched attempt 2 output mismatch: %v", err)
	}
}

func TestServeOrReplayHTTPHandlerOffline(t *testing.T) {
	drvToHostR, drvToHostW := io.Pipe()
	hostToDrvR, hostToDrvW := io.Pipe()

	kv := Init(Config{
		Service: "http-svc",
		Mode:    ModeReplay,
		Stdin:   drvToHostR,
		Stdout:  hostToDrvW,
	})
	defer kv.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		ts := kv.Now(r.Context()).UTC().Format(time.RFC3339)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"time":"` + ts + `"}`))
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- kv.ServeOrReplay(mux)
	}()

	reader := bufio.NewReader(hostToDrvR)
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		_, _ = drvToHostW.Write(append(b, '\n'))
	}
	recv := func() map[string]any {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m
	}

	send(map[string]any{"t": "hello", "protocol": 1, "service": "http-svc", "start": "genesis"})
	if m := recv(); m["t"] != "ready" {
		t.Fatalf("want ready, got %+v", m)
	}

	send(map[string]any{
		"t":      "step",
		"seq":    "1",
		"source": "http:GET /hello",
		"data":   base64.StdEncoding.EncodeToString([]byte("")),
	})
	if m := recv(); m["t"] != "clock" {
		t.Fatalf("want clock, got %+v", m)
	}
	send(map[string]any{"t": "clock", "unix_nanos": "1741007009000000000"})

	emit := recv()
	if emit["t"] != "emit" || emit["sink"] != "http:response" {
		t.Fatalf("want emit(http:response), got %+v", emit)
	}
	decoded, _ := base64.StdEncoding.DecodeString(emit["data"].(string))
	if !strings.Contains(string(decoded), "2025-03-03T13:03:29Z") {
		t.Fatalf("unexpected HTTP replay body: %s", decoded)
	}

	if done := recv(); done["t"] != "done" || done["outcome"] != "ok" {
		t.Fatalf("want done(ok), got %+v", done)
	}
	send(map[string]any{"t": "end"})
	if err := <-errCh; err != nil {
		t.Fatalf("ServeOrReplay returned error: %v", err)
	}
}

func TestClockInheritanceInRunStepWithTable(t *testing.T) {
	kv := Init(Config{Service: "clk-svc", Mode: ModeReplay})
	fixed := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	repClock := &clock.ReplayProvider{Clocks: []int64{fixed.UnixNano()}, Lenient: true}
	baseCtx := clock.WithProvider(context.Background(), repClock)

	var got time.Time
	_, err := kv.RunStepWithTable(baseCtx, Input{Source: "test"}, matcher.NewStepTable(1, nil, nil), StepFunc(func(ctx context.Context, _ Input) error {
		got = kv.Now(ctx)
		return nil
	}))
	if err != nil {
		t.Fatalf("RunStepWithTable: %v", err)
	}
	if !got.Equal(fixed) {
		t.Fatalf("Now = %v, want %v", got, fixed)
	}
}

func TestInjectFaultNthAndOnceAcrossSteps(t *testing.T) {
	kv := Init(Config{Service: "fault-svc", Mode: ModeReplay})
	defer kv.Close()

	kv.InjectFault(FaultRule{
		Driver:    "redis",
		Nth:       2,
		Once:      true,
		ErrString: CodeRedisLoading,
	})

	call1 := GatewayCall{Driver: "redis", Op: "GET", KeyArgs: []string{"k1"}, Response: []byte("v1")}
	call2 := GatewayCall{Driver: "redis", Op: "ZADD", KeyArgs: []string{"sub_refresh"}, Response: []byte("1")}
	in := Input{Source: "kafka:raw_events", Position: "0:1", Data: []byte(`{"id":"1"}`)}

	runner := StepFunc(func(ctx context.Context, _ Input) error {
		resp, err := kv.ReplayGateway(ctx, call1)
		if err != nil {
			return err
		}
		if string(resp) != "v1" {
			return errors.New("unexpected call1 resp")
		}
		resp, err = kv.ReplayGateway(ctx, call2)
		if err != nil {
			return err
		}
		if string(resp) != "1" {
			return errors.New("unexpected call2 resp")
		}
		return nil
	})

	// Step 1: 1st redis call succeeds, 2nd redis call (Nth: 2) triggers REDIS_LOADING.
	tbl1 := matcher.NewStepTable(1, []GatewayCall{call1, call2}, nil)
	_, err := kv.RunStepWithTable(context.Background(), in, tbl1, runner)
	if !matcher.IsFault(err, CodeRedisLoading) {
		t.Fatalf("step 1 expected REDIS_LOADING on Nth=2 call, got %v", err)
	}

	// Step 2 (retry): Once=true already fired, so both calls succeed cleanly.
	tbl2 := matcher.NewStepTable(2, []GatewayCall{call1, call2}, nil)
	if _, err := kv.RunStepWithTable(context.Background(), in, tbl2, runner); err != nil {
		t.Fatalf("step 2 (retry after Once=true fault) should succeed, got %v", err)
	}

	// After resetSession(), rule counters reset so Nth=2 fires again on a new session.
	kv.resetSession()
	tbl3 := matcher.NewStepTable(1, []GatewayCall{call1, call2}, nil)
	_, err = kv.RunStepWithTable(context.Background(), in, tbl3, runner)
	if !matcher.IsFault(err, CodeRedisLoading) {
		t.Fatalf("after resetSession expected REDIS_LOADING on Nth=2 call again, got %v", err)
	}
}
