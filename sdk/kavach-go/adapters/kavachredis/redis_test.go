package kavachredis_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachredis"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
	"github.com/redis/go-redis/v9"
)

// stubHook simulates a live Redis server inside go-redis's hook chain so we can
// test Record mode end-to-end without requiring a local Redis daemon.
type stubRedisServerHook struct {
	kvStore map[string]string
	zsets   map[string][]redis.Z
	hashes  map[string]map[string]string
	ints    map[string]int64
}

func (s *stubRedisServerHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("stub should not dial")
	}
}

func (s *stubRedisServerHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return s.exec(cmd)
	}
}

func (s *stubRedisServerHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		var first error
		for _, c := range cmds {
			if err := s.exec(c); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
}

func (s *stubRedisServerHook) exec(cmd redis.Cmder) error {
	args := cmd.Args()
	name := strings.ToLower(cmd.Name())
	switch name {
	case "ping":
		cmd.(*redis.StatusCmd).SetVal("PONG")
	case "set":
		s.kvStore[fmt.Sprint(args[1])] = fmt.Sprint(args[2])
		cmd.(*redis.StatusCmd).SetVal("OK")
	case "get":
		v, ok := s.kvStore[fmt.Sprint(args[1])]
		if !ok {
			cmd.SetErr(redis.Nil)
			return redis.Nil
		}
		cmd.(*redis.StringCmd).SetVal(v)
	case "incr":
		k := fmt.Sprint(args[1])
		s.ints[k]++
		cmd.(*redis.IntCmd).SetVal(s.ints[k])
	case "expire":
		cmd.(*redis.BoolCmd).SetVal(true)
	case "zadd":
		k := fmt.Sprint(args[1])
		s.zsets[k] = append(s.zsets[k], redis.Z{Score: 1700000000, Member: fmt.Sprint(args[3])})
		cmd.(*redis.IntCmd).SetVal(1)
	case "zrange":
		k := fmt.Sprint(args[1])
		if zs, ok := cmd.(*redis.ZSliceCmd); ok {
			zs.SetVal(s.zsets[k])
		} else if ss, ok := cmd.(*redis.StringSliceCmd); ok {
			var vals []string
			for _, z := range s.zsets[k] {
				vals = append(vals, fmt.Sprint(z.Member))
			}
			ss.SetVal(vals)
		}
	case "hgetall":
		cmd.(*redis.MapStringStringCmd).SetVal(s.hashes[fmt.Sprint(args[1])])
	case "mget":
		vals := make([]any, len(args)-1)
		for i, a := range args[1:] {
			if v, ok := s.kvStore[fmt.Sprint(a)]; ok {
				vals[i] = v
			}
		}
		cmd.(*redis.SliceCmd).SetVal(vals)
	case "incrbyfloat":
		cmd.(*redis.FloatCmd).SetVal(42.5)
	case "echo":
		cmd.(*redis.Cmd).SetVal("hello-raw")
	default:
		cmd.SetErr(nil)
	}
	return nil
}

func decodeRecords(t *testing.T, path string) []ring.Record {
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
		t.Fatalf("feed: %v", err)
	}
	var out []ring.Record
	for _, f := range dec.FinishEOF().Frames {
		if f.Kind == ring.FrameRecord {
			r, err := ring.DecodeRecord(f.Payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			out = append(out, r)
		}
	}
	return out
}

func TestRedisRecordAndOfflineReplayAllCmdTypes(t *testing.T) {
	streamPath := filepath.Join(t.TempDir(), "redis.stream")
	recKV := kavach.Init(kavach.Config{
		Service:  "events-processor",
		RingPath: streamPath,
		Mode:     kavach.ModeRecord,
	})

	// Build a *redis.Client pointing to an unreachable address; stubRedisServerHook
	// simulates the live Redis backend below kavachredis.Wrap.
	liveClient := redis.NewClient(&redis.Options{Addr: "192.0.2.1:6379", MaxRetries: 0})
	defer liveClient.Close()
	// Wrap first (outer hook) so kavachredis sees the result produced by inner stub.
	wrapped := kavachredis.Wrap(recKV, liveClient)
	wrapped.AddHook(&stubRedisServerHook{
		kvStore: map[string]string{"plan:1": "enterprise"},
		zsets:   map[string][]redis.Z{},
		hashes:  map[string]map[string]string{"meta:1": {"tier": "gold"}},
		ints:    map[string]int64{},
	})

	// Type preservation check: Wrap(*redis.Client) returns *redis.Client!
	var _ *redis.Client = wrapped

	ctx, sc := recKV.BeginStep(context.Background(), kavach.Input{
		Source: "kafka:events_raw",
		Data:   []byte(`{"transaction_id":"tx-redis-1"}`),
	})

	if v, err := wrapped.Get(ctx, "plan:1").Result(); err != nil || v != "enterprise" {
		t.Fatalf("GET plan:1 = %q, %v", v, err)
	}
	if _, err := wrapped.Get(ctx, "missing:key").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected redis.Nil, got %v", err)
	}
	if ok, err := wrapped.Expire(ctx, "plan:1", 10*time.Second).Result(); err != nil || !ok {
		t.Fatalf("EXPIRE = %v, %v", ok, err)
	}
	// Lago FlagStore.Flag pattern: ZADD with wall-clock timestamp score & bucket!
	now1 := int64(1700000100)
	if n, err := wrapped.ZAdd(ctx, "subscription_refreshed_v2", redis.Z{
		Score:  float64(now1),
		Member: fmt.Sprintf("org1:sub1|%d", now1),
	}).Result(); err != nil || n != 1 {
		t.Fatalf("ZADD = %d, %v", n, err)
	}
	if m, err := wrapped.HGetAll(ctx, "meta:1").Result(); err != nil || m["tier"] != "gold" {
		t.Fatalf("HGETALL = %v, %v", m, err)
	}
	if sl, err := wrapped.MGet(ctx, "plan:1", "missing:key").Result(); err != nil || len(sl) != 2 || sl[0] != "enterprise" || sl[1] != nil {
		t.Fatalf("MGET = %v, %v", sl, err)
	}
	if f, err := wrapped.IncrByFloat(ctx, "usage:1", 42.5).Result(); err != nil || f != 42.5 {
		t.Fatalf("INCRBYFLOAT = %v, %v", f, err)
	}
	if zs, err := wrapped.ZRangeWithScores(ctx, "subscription_refreshed_v2", 0, -1).Result(); err != nil || len(zs) != 1 {
		t.Fatalf("ZRANGE WITHSCORES = %v, %v", zs, err)
	}
	if ss, err := wrapped.ZRange(ctx, "subscription_refreshed_v2", 0, -1).Result(); err != nil || len(ss) != 1 {
		t.Fatalf("ZRANGE = %v, %v", ss, err)
	}
	if rawVal, err := wrapped.Do(ctx, "ECHO", "hello-raw").Result(); err != nil || rawVal != "hello-raw" {
		t.Fatalf("DO ECHO = %v, %v", rawVal, err)
	}
	// Pipeline test
	pipe := wrapped.Pipeline()
	setCmd := pipe.Set(ctx, "k:pipe", "v1", 0)
	incrCmd := pipe.Incr(ctx, "cnt:pipe")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("Pipeline Exec: %v", err)
	}
	if setCmd.Val() != "OK" || incrCmd.Val() != 1 {
		t.Fatalf("pipeline results: set=%q incr=%d", setCmd.Val(), incrCmd.Val())
	}

	if err := sc.End(nil); err != nil {
		t.Fatalf("end step: %v", err)
	}
	_ = recKV.Close()

	// Collect recorded Gateway calls.
	recs := decodeRecords(t, streamPath)
	var gws []kavach.GatewayCall
	for _, r := range recs {
		if r.Type == ring.RecordGateway {
			gc := matcher.ParseGatewayRequest(r.Gateway, r.Request)
			gc.Response = r.Response
			gc.Error = r.Error
			gws = append(gws, gc)
		}
	}
	if len(gws) != 12 {
		t.Fatalf("expected 12 recorded Redis gateway calls, got %d", len(gws))
	}

	// Replay mode: unreachable Redis address, ZERO stub server, ZERO network!
	replayKV := kavach.Init(kavach.Config{Service: "events-processor", Mode: kavach.ModeReplay})
	replayClient := kavachredis.Wrap(replayKV, redis.NewClient(&redis.Options{
		Addr:        "192.0.2.1:6379",
		DialTimeout: 10 * time.Millisecond,
	}))
	defer replayClient.Close()

	// Startup Ping before any step MUST succeed offline (Lago config/redis/redis.go:50)!
	if pong, err := replayClient.Ping(context.Background()).Result(); err != nil || pong != "PONG" {
		t.Fatalf("startup Ping in replay = %q, %v (want PONG, nil)", pong, err)
	}

	tbl := matcher.NewStepTable(1, gws, nil)
	_, err := replayKV.RunStepWithTable(context.Background(), kavach.Input{
		Source: "kafka:events_raw",
		Data:   []byte(`{"transaction_id":"tx-redis-1"}`),
	}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		if v, err := replayClient.Get(ctx, "plan:1").Result(); err != nil || v != "enterprise" {
			return fmt.Errorf("replay GET plan:1 = %q, %v", v, err)
		}
		if _, err := replayClient.Get(ctx, "missing:key").Result(); !errors.Is(err, redis.Nil) {
			return fmt.Errorf("replay missing:key expected redis.Nil, got %v", err)
		}
		if ok, err := replayClient.Expire(ctx, "plan:1", 10*time.Second).Result(); err != nil || !ok {
			return fmt.Errorf("replay EXPIRE = %v, %v", ok, err)
		}
		// Crucial Lago check: ZADD with a COMPLETELY DIFFERENT wall-clock timestamp MUST STILL MATCH!
		now2 := int64(1899999990)
		if n, err := replayClient.ZAdd(ctx, "subscription_refreshed_v2", redis.Z{
			Score:  float64(now2),
			Member: fmt.Sprintf("org1:sub1|%d", now2),
		}).Result(); err != nil || n != 1 {
			return fmt.Errorf("replay ZADD with new timestamp failed: %d, %v", n, err)
		}
		if m, err := replayClient.HGetAll(ctx, "meta:1").Result(); err != nil || m["tier"] != "gold" {
			return fmt.Errorf("replay HGETALL = %v, %v", m, err)
		}
		if sl, err := replayClient.MGet(ctx, "plan:1", "missing:key").Result(); err != nil || len(sl) != 2 || sl[0] != "enterprise" {
			return fmt.Errorf("replay MGET = %v, %v", sl, err)
		}
		if f, err := replayClient.IncrByFloat(ctx, "usage:1", 42.5).Result(); err != nil || f != 42.5 {
			return fmt.Errorf("replay INCRBYFLOAT = %v, %v", f, err)
		}
		if zs, err := replayClient.ZRangeWithScores(ctx, "subscription_refreshed_v2", 0, -1).Result(); err != nil || len(zs) != 1 {
			return fmt.Errorf("replay ZRANGE WITHSCORES = %v, %v", zs, err)
		}
		if ss, err := replayClient.ZRange(ctx, "subscription_refreshed_v2", 0, -1).Result(); err != nil || len(ss) != 1 {
			return fmt.Errorf("replay ZRANGE = %v, %v", ss, err)
		}
		if rawVal, err := replayClient.Do(ctx, "ECHO", "hello-raw").Result(); err != nil || rawVal != "hello-raw" {
			return fmt.Errorf("replay DO ECHO = %v, %v", rawVal, err)
		}
		pipe := replayClient.Pipeline()
		scmd := pipe.Set(ctx, "k:pipe", "v1", 0)
		icmd := pipe.Incr(ctx, "cnt:pipe")
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("replay pipeline: %v", err)
		}
		if scmd.Val() != "OK" || icmd.Val() != 1 {
			return fmt.Errorf("replay pipeline vals: %q, %d", scmd.Val(), icmd.Val())
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("offline Redis replay failed: %v", err)
	}
}

func TestRedisFaultInjectionLagoL1Pattern(t *testing.T) {
	kv := kavach.Init(kavach.Config{Service: "lago-events", Mode: kavach.ModeReplay})
	kv.Invariant("atomic_single_emit_on_retry", func(s kavach.Step) error {
		return s.AssertAtMostOnceEmit("kafka:events_charged_in_advance", "$.transaction_id")
	})

	rdb := kavachredis.NewReplayClient(kv)
	defer rdb.Close()

	zaddResp, _ := json.Marshal(map[string]any{"kind": "int", "int": 1})
	recordedGWs := []kavach.GatewayCall{{
		Driver:       "redis",
		Gateway:      "redis:zadd",
		Op:           "ZADD",
		NormalizedOp: "ZADD",
		KeyArgs:      []string{"subscription_refreshed_v2"},
		Response:     zaddResp,
	}}
	txPayload := []byte(`{"transaction_id":"tx-vertex-003","amount":2550}`)
	recordedOuts := []matcher.OutputRecord{{Sink: "kafka:events_charged_in_advance", Data: txPayload}}

	in := kavach.Input{Source: "kafka:events_raw", Data: txPayload}

	// Patched Lago L1 order: Redis ZADD Gateway BEFORE Kafka Output!
	patchedHandler := kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		now := time.Now().Unix()
		if err := rdb.ZAdd(ctx, "subscription_refreshed_v2", redis.Z{
			Score:  float64(now),
			Member: fmt.Sprintf("org:sub|%d", now),
		}).Err(); err != nil {
			return err // abort before emitting Kafka output!
		}
		kv.RecordOutput(ctx, kavach.Output{
			Sink: "kafka:events_charged_in_advance",
			Data: txPayload,
		})
		return nil
	})

	// Attempt 1: inject REDIS_LOADING -> fails cleanly with 0 emitted outputs!
	tbl1 := matcher.NewStepTable(1, recordedGWs, recordedOuts)
	tbl1.InjectFault(kavach.FaultRule{
		Driver:    "redis",
		Gateway:   "redis:zadd",
		ErrString: kavach.CodeRedisLoading,
		Once:      true,
	})
	sc1, err1 := kv.RunStepWithTable(context.Background(), in, tbl1, patchedHandler)
	if err1 == nil || !strings.Contains(err1.Error(), "LOADING Redis is loading") {
		t.Fatalf("attempt 1 expected LOADING error, got %v", err1)
	}
	if len(sc1.Outputs()) != 0 {
		t.Fatalf("attempt 1 should emit 0 outputs on LOADING fault, got %d", len(sc1.Outputs()))
	}

	// Attempt 2: redelivery succeeds cleanly with Gateway reordered ahead of Output!
	tbl2 := matcher.NewStepTable(2, recordedGWs, recordedOuts)
	sc2, err2 := kv.RunStepWithTable(context.Background(), in, tbl2, patchedHandler)
	if err2 != nil {
		t.Fatalf("attempt 2 retry failed: %v", err2)
	}
	if len(sc2.Outputs()) != 1 {
		t.Fatalf("attempt 2 expected 1 output, got %d", len(sc2.Outputs()))
	}
	if err := tbl2.CompareOutputs(); err != nil {
		t.Fatalf("output comparison failed: %v", err)
	}
}
