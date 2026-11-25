package kavachkgo_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachkgo"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
	"github.com/twmb/franz-go/pkg/kgo"
)

func decodeStream(t *testing.T, path string) []ring.Record {
	t.Helper()
	var raw []byte
	if r, err := ring.Open(path); err == nil {
		defer r.Close()
		raw, err = r.PollOnce()
		if err != nil {
			t.Fatalf("poll ring: %v", err)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
	}
	var dec ring.FrameDecoder
	if _, err := dec.Feed(raw); err != nil {
		t.Fatalf("feed stream: %v", err)
	}
	var out []ring.Record
	for _, f := range dec.FinishEOF().Frames {
		if f.Kind == ring.FrameRecord {
			r, err := ring.DecodeRecord(f.Payload)
			if err != nil {
				t.Fatalf("decode record: %v", err)
			}
			out = append(out, r)
		}
	}
	return out
}

func TestKafkaOfflineClientAndBatchConsumerServeOrReplay(t *testing.T) {
	// 1. NewClient in ModeReplay with unreachable brokers MUST succeed offline!
	var inBuf bytes.Buffer
	var outBuf bytes.Buffer
	replayKV := kavach.Init(kavach.Config{
		Service: "events-processor",
		Mode:    kavach.ModeReplay,
		Stdin:   &inBuf,
		Stdout:  &outBuf,
	})

	producer, err := kavachkgo.NewClient(replayKV, kgo.SeedBrokers("192.0.2.1:9092"))
	if err != nil {
		t.Fatalf("NewClient in ModeReplay failed: %v", err)
	}
	defer producer.Close()

	// Startup Ping & CommitRecords must succeed offline!
	if err := producer.Ping(context.Background()); err != nil {
		t.Fatalf("Ping in replay failed: %v", err)
	}
	if err := producer.CommitRecords(context.Background(), &kgo.Record{Topic: "events_raw", Offset: 1}); err != nil {
		t.Fatalf("CommitRecords in replay failed: %v", err)
	}

	// Lago-style batch processor: produces to events_enriched + events_charged_in_advance.
	processRecords := func(ctx context.Context, records []*kgo.Record) []*kgo.Record {
		var committed []*kgo.Record
		for _, rec := range records {
			var ev struct {
				OrgID         string `json:"organization_id"`
				TransactionID string `json:"transaction_id"`
				PayInAdvance  bool   `json:"pay_in_advance"`
			}
			if err := json.Unmarshal(rec.Value, &ev); err != nil {
				continue
			}
			outBytes, _ := json.Marshal(map[string]any{
				"organization_id": ev.OrgID,
				"transaction_id":  ev.TransactionID,
				"enriched":        true,
			})
			res := producer.ProduceSync(ctx, &kgo.Record{
				Topic: "events_enriched",
				Key:   []byte(ev.OrgID + "-" + ev.TransactionID),
				Value: outBytes,
			})
			if res.FirstErr() != nil {
				continue
			}
			if ev.PayInAdvance {
				var wg sync.WaitGroup
				wg.Add(1)
				producer.Produce(ctx, &kgo.Record{
					Topic: "events_charged_in_advance",
					Key:   []byte(ev.OrgID + "-" + ev.TransactionID),
					Value: outBytes,
				}, func(_ *kgo.Record, pErr error) {
					if pErr != nil {
						t.Errorf("async Produce failed: %v", pErr)
					}
					wg.Done()
				})
				wg.Wait()
			}
			committed = append(committed, rec)
		}
		return committed
	}

	consumer := kavachkgo.WrapBatchConsumer(replayKV, "events_raw", processRecords, func(context.Context) {
		t.Fatal("liveStart MUST NOT run in ModeReplay")
	})

	// Drive kv.ServeOrReplay(consumer) via SPEC §9 stdio protocol!
	writeMsg := func(m map[string]any) {
		b, _ := json.Marshal(m)
		inBuf.Write(append(b, '\n'))
	}
	rawEvent := []byte(`{"organization_id":"org-1","transaction_id":"tx-100","pay_in_advance":true}`)
	writeMsg(map[string]any{"t": "hello", "protocol": 1, "service": "events-processor", "start": "genesis"})
	writeMsg(map[string]any{
		"t":        "step",
		"seq":      1,
		"source":   "kafka:events_raw",
		"position": "0:42",
		"data":     base64.StdEncoding.EncodeToString(rawEvent),
	})
	writeMsg(map[string]any{"t": "end"})

	if err := replayKV.ServeOrReplay(consumer); err != nil {
		t.Fatalf("ServeOrReplay failed: %v", err)
	}

	// Verify both kafka:* emits were sent to the replay driver with top-level transaction_id!
	sinks := map[string]string{}
	sawDoneOK := false
	for _, line := range strings.Split(strings.TrimSpace(outBuf.String()), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("invalid host json %q: %v", line, err)
		}
		if msg["t"] == "emit" {
			data, _ := base64.StdEncoding.DecodeString(msg["data"].(string))
			sinks[msg["sink"].(string)] = string(data)
		}
		if msg["t"] == "done" && msg["outcome"] == "ok" {
			sawDoneOK = true
		}
	}
	if !sawDoneOK {
		t.Fatalf("expected done ok, got:\n%s", outBuf.String())
	}
	for _, wantSink := range []string{"kafka:events_enriched", "kafka:events_charged_in_advance"} {
		body, ok := sinks[wantSink]
		if !ok {
			t.Fatalf("missing emit for sink %q; got sinks=%v", wantSink, sinks)
		}
		if !strings.Contains(body, `"transaction_id":"tx-100"`) {
			t.Fatalf("sink %q payload missing transaction_id: %s", wantSink, body)
		}
	}
}

func TestKafkaBatchConsumerRecordModeAndUncommittedError(t *testing.T) {
	streamPath := filepath.Join(t.TempDir(), "kgo.stream")
	recKV := kavach.Init(kavach.Config{
		Service:  "events-processor",
		RingPath: streamPath,
		Mode:     kavach.ModeRecord,
	})

	hook := kavachkgo.NewRecordHook(recKV).(kgo.HookProduceRecordUnbuffered)

	// Simulate a batch of 2 records: first succeeds and produces an output; second fails (uncommitted).
	bc := kavachkgo.WrapBatchHandler(recKV, "events_raw", func(ctx context.Context, recs []*kgo.Record) []*kgo.Record {
		rec := recs[0]
		if string(rec.Key) == "fail-key" {
			return nil // uncommitted -> ErrRecordNotCommitted
		}
		// Simulate hook firing on produced record during the active step.
		hook.OnProduceRecordUnbuffered(&kgo.Record{
			Topic: "events_enriched",
			Key:   rec.Key,
			Value: []byte(`{"transaction_id":"tx-ok"}`),
		}, nil)
		return recs
	})

	batch := []*kgo.Record{
		{Topic: "events_raw", Partition: 1, Offset: 10, Key: []byte("ok-key"), Value: []byte(`{"transaction_id":"tx-ok"}`)},
		{Topic: "events_raw", Partition: 1, Offset: 11, Key: []byte("fail-key"), Value: []byte(`{"transaction_id":"tx-fail"}`)},
	}
	committed := bc.ProcessBatch(context.Background(), batch)
	if len(committed) != 1 || string(committed[0].Key) != "ok-key" {
		t.Fatalf("expected only ok-key committed, got %+v", committed)
	}
	_ = recKV.Close()

	recs := decodeStream(t, streamPath)
	inputs, outputs, markers := 0, 0, 0
	for _, r := range recs {
		switch r.Type {
		case ring.RecordInput:
			inputs++
			if r.Source != "kafka:events_raw" {
				t.Fatalf("unexpected Input source %q", r.Source)
			}
		case ring.RecordOutput:
			outputs++
			if r.Sink != "kafka:events_enriched" {
				t.Fatalf("unexpected Output sink %q", r.Sink)
			}
		case ring.RecordMarker:
			markers++
			if r.Kind != ring.MarkerError || !strings.Contains(r.Message, "not committed") {
				t.Fatalf("unexpected failure marker: %+v", r)
			}
		}
	}
	if inputs != 2 || outputs != 1 || markers != 1 {
		t.Fatalf("expected 2 inputs, 1 output, 1 error marker; got %d, %d, %d", inputs, outputs, markers)
	}
}

func TestKafkaLagoL1ReorderedGatewayAndRetryIdempotency(t *testing.T) {
	// Proves the complete Lago L1 scenario end-to-end with kavachkgo + Gateway fault injection:
	// - Unpatched handler emits Kafka Output BEFORE Redis ZADD -> fails invariant on retry!
	// - Patched handler runs Redis ZADD Gateway BEFORE Kafka Output -> Attempt 1 fails cleanly
	//   with 0 emits, Attempt 2 succeeds cleanly even with Gateway reordered ahead of Output!
	runScenario := func(patched bool) (attempt1Err, attempt2Err error, attempt1Emits, attempt2Emits int) {
		kv := kavach.Init(kavach.Config{Service: "lago-events", Mode: kavach.ModeReplay})
		kv.Invariant("atomic_single_emit_on_retry", func(s kavach.Step) error {
			return s.AssertAtMostOnceEmit("kafka:events_charged_in_advance", "$.transaction_id")
		})
		prod := kavachkgo.Wrap(kv, nil)

		redisCall := kavach.GatewayCall{
			Driver:       "redis",
			Gateway:      "redis:zadd",
			Op:           "ZADD",
			NormalizedOp: "ZADD",
			KeyArgs:      []string{"subscription_refreshed_v2"},
			Response:     []byte(`{"kind":"int","int":1}`),
		}
		payload := []byte(`{"organization_id":"org-v","transaction_id":"tx-vertex-003"}`)

		handler := kavachkgo.WrapBatchHandler(kv, "events_raw", func(ctx context.Context, recs []*kgo.Record) []*kgo.Record {
			if !patched {
				// BUGGY ORDER: produce BEFORE retryable Redis check.
				prod.ProduceSync(ctx, &kgo.Record{Topic: "events_charged_in_advance", Value: payload})
				if _, err := kv.ReplayGateway(ctx, redisCall); err != nil {
					return nil // uncommitted!
				}
				return recs
			}
			// PATCHED ORDER: check Redis Gateway BEFORE producing to Kafka.
			if _, err := kv.ReplayGateway(ctx, redisCall); err != nil {
				return nil // uncommitted, 0 emits leaked!
			}
			prod.ProduceSync(ctx, &kgo.Record{Topic: "events_charged_in_advance", Value: payload})
			return recs
		})

		in := kavach.Input{Source: "kafka:events_raw", Position: "0:9", Data: payload}
		recordedOuts := []matcher.OutputRecord{{Sink: "kafka:events_charged_in_advance", Data: payload}}

		// Attempt 1: transient REDIS_LOADING fault.
		tbl1 := matcher.NewStepTable(1, []kavach.GatewayCall{redisCall}, recordedOuts)
		tbl1.InjectFault(kavach.FaultRule{
			Driver:    "redis",
			Gateway:   "redis:zadd",
			ErrString: kavach.CodeRedisLoading,
			Once:      true,
		})
		sc1, err1 := kv.RunStepWithTable(context.Background(), in, tbl1, handler)

		// Attempt 2: redelivery after Redis recovers.
		tbl2 := matcher.NewStepTable(2, []kavach.GatewayCall{redisCall}, recordedOuts)
		sc2, err2 := kv.RunStepWithTable(context.Background(), in, tbl2, handler)
		return err1, err2, len(sc1.Outputs()), len(sc2.Outputs())
	}

	// Unpatched: leaks emit on Attempt 1 AND violates atomic_single_emit_on_retry!
	uErr1, uErr2, uEmits1, uEmits2 := runScenario(false)
	if uEmits1 != 1 || uEmits2 != 1 {
		t.Fatalf("unpatched expected 1+1 double emits, got %d+%d", uEmits1, uEmits2)
	}
	var invErr *kavach.InvariantError
	if !errors.As(uErr1, &invErr) && !errors.As(uErr2, &invErr) {
		t.Fatalf("expected unpatched build to trigger InvariantError, got err1=%v err2=%v", uErr1, uErr2)
	}

	// Patched: Attempt 1 returns ErrRecordNotCommitted with 0 emits; Attempt 2 succeeds cleanly!
	pErr1, pErr2, pEmits1, pEmits2 := runScenario(true)
	if !errors.Is(pErr1, kavachkgo.ErrRecordNotCommitted) {
		t.Fatalf("patched attempt 1 expected ErrRecordNotCommitted, got %v", pErr1)
	}
	if pEmits1 != 0 {
		t.Fatalf("patched attempt 1 expected 0 emits, got %d", pEmits1)
	}
	if pErr2 != nil || pEmits2 != 1 {
		t.Fatalf("patched attempt 2 expected nil err & 1 emit, got err=%v emits=%d", pErr2, pEmits2)
	}
}

func TestKafkaOfflineRecordClientAndRecordConsumer(t *testing.T) {
	ringPath := filepath.Join(t.TempDir(), "offline-rec.shm")
	recKV := kavach.Init(kavach.Config{
		Service:  "events-processor",
		RingPath: ringPath,
		Mode:     kavach.ModeRecord,
	})
	prod := kavachkgo.NewOfflineClient(recKV)
	if err := prod.Ping(context.Background()); err != nil {
		t.Fatalf("offline Ping: %v", err)
	}

	rc := kavachkgo.WrapHandler(recKV, "events_raw", func(ctx context.Context, rec *kgo.Record) error {
		res := prod.ProduceSync(ctx, &kgo.Record{
			Topic: "events_enriched",
			Key:   rec.Key,
			Value: rec.Value,
		})
		return res.FirstErr()
	}, nil)

	if err := rc.HandleRecord(context.Background(), &kgo.Record{
		Topic:     "events_raw",
		Partition: 0,
		Offset:    7,
		Key:       []byte("k-7"),
		Value:     []byte(`{"transaction_id":"tx-7"}`),
	}); err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	_ = recKV.Close()

	recs := decodeStream(t, ringPath)
	var sawIn, sawOut bool
	for _, r := range recs {
		if r.Type == ring.RecordInput && r.Source == "kafka:events_raw" && r.Position == "0:7" {
			sawIn = true
		}
		if r.Type == ring.RecordOutput && r.Sink == "kafka:events_enriched" && strings.Contains(string(r.Data), `"transaction_id":"tx-7"`) {
			sawOut = true
		}
	}
	if !sawIn || !sawOut {
		t.Fatalf("missing expected offline recorded input/output: sawIn=%v sawOut=%v", sawIn, sawOut)
	}
}
