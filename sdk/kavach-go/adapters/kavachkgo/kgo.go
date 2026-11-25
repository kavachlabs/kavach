// Package kavachkgo provides Kavach v0.2 record/replay adapters for
// github.com/twmb/franz-go/pkg/kgo (Kafka consumers and producers) with ZERO
// live Kafka broker connection in Replay mode.
package kavachkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach-go"
	"github.com/twmb/franz-go/pkg/kgo"
)

// ErrRecordNotCommitted is returned by BatchConsumer.RunStep (and recorded as a
// step error in ModeRecord) when a batch handler like Lago's ProcessEvents leaves
// a record uncommitted (e.g. on a retryable downstream failure).
var ErrRecordNotCommitted = errors.New("kavachkgo: record not committed")

// Client wraps a *kgo.Client for Kafka produce/consume operations.
// In ModeReplay, raw may be nil and NO Kafka broker is ever dialed:
//   - Ping and CommitRecords succeed immediately offline.
//   - Produce and ProduceSync capture Outputs ("kafka:<topic>") into Kavach without network I/O.
type Client struct {
	kv      *kavach.Kavach
	raw     *kgo.Client
	offline bool
}

// Wrap wraps an existing *kgo.Client with Kavach capture/replay.
// In ModeReplay, client may be nil and is never used for network I/O.
func Wrap(kv *kavach.Kavach, client *kgo.Client) *Client {
	return &Client{kv: kv, raw: client}
}

// NewOfflineClient returns a *Client that never dials Kafka in any mode
// (capturing Outputs in both ModeRecord and ModeReplay). Useful for local
// `kavach dev` / test-driven capsule capture without running a Kafka broker.
func NewOfflineClient(kv *kavach.Kavach) *Client {
	return &Client{kv: kv, offline: true}
}

// NewClient constructs a wrapped *kgo.Client from opts.
// In ModeReplay it returns an offline *Client immediately WITHOUT creating a
// real *kgo.Client or dialing any Kafka seed brokers.
func NewClient(kv *kavach.Kavach, opts ...kgo.Opt) (*Client, error) {
	if kv != nil && kv.IsReplay() {
		return &Client{kv: kv, offline: true}, nil
	}
	raw, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &Client{kv: kv, raw: raw}, nil
}

func (c *Client) isOffline() bool {
	return c.offline || (c.kv != nil && c.kv.IsReplay())
}

// Raw returns the underlying *kgo.Client (nil in offline ModeReplay).
func (c *Client) Raw() *kgo.Client { return c.raw }

// Ping verifies broker connectivity. In ModeReplay (or offline client) it succeeds immediately.
func (c *Client) Ping(ctx context.Context) error {
	if c.isOffline() {
		return nil
	}
	if c.raw == nil {
		return errors.New("kavachkgo: nil underlying kgo.Client")
	}
	return c.raw.Ping(ctx)
}

// Close closes the underlying *kgo.Client if non-nil.
func (c *Client) Close() {
	if c != nil && c.raw != nil {
		c.raw.Close()
	}
}

// AllowRebalance delegates to the underlying client outside ModeReplay.
func (c *Client) AllowRebalance() {
	if c.isOffline() {
		return
	}
	if c.raw != nil {
		c.raw.AllowRebalance()
	}
}

// CommitRecords commits offsets. In ModeReplay (or offline client) it succeeds immediately.
func (c *Client) CommitRecords(ctx context.Context, rs ...*kgo.Record) error {
	if c.isOffline() {
		return nil
	}
	if c.raw == nil {
		return errors.New("kavachkgo: nil underlying kgo.Client")
	}
	return c.raw.CommitRecords(ctx, rs...)
}

// PollRecords delegates to *kgo.Client.PollRecords outside ModeReplay.
func (c *Client) PollRecords(ctx context.Context, maxPollRecords int) kgo.Fetches {
	if c.isOffline() || c.raw == nil {
		return nil
	}
	return c.raw.PollRecords(ctx, maxPollRecords)
}

// PollFetches delegates to *kgo.Client.PollFetches outside ModeReplay.
func (c *Client) PollFetches(ctx context.Context) kgo.Fetches {
	if c.isOffline() || c.raw == nil {
		return nil
	}
	return c.raw.PollFetches(ctx)
}

// Produce asynchronously produces r.
// In ModeReplay (or offline client) it NEVER touches Kafka: captures the Output ("kafka:<topic>")
// and invokes promise(r, nil) synchronously.
// In ModeRecord it produces to Kafka and captures the Output upon success.
func (c *Client) Produce(ctx context.Context, r *kgo.Record, promise func(*kgo.Record, error)) {
	if c.isOffline() {
		if r != nil && c.kv != nil && !c.kv.IsOff() {
			recordProducedOutput(ctx, c.kv, r)
		}
		if promise != nil {
			promise(r, nil)
		}
		return
	}
	if c.kv == nil || c.kv.IsOff() {
		if c.raw != nil {
			c.raw.Produce(ctx, r, promise)
		} else if promise != nil {
			promise(r, errors.New("kavachkgo: nil underlying kgo.Client"))
		}
		return
	}
	if c.raw == nil {
		if promise != nil {
			promise(r, errors.New("kavachkgo: nil underlying kgo.Client"))
		}
		return
	}
	c.raw.Produce(ctx, r, func(rec *kgo.Record, err error) {
		if err == nil && rec != nil {
			recordProducedOutput(ctx, c.kv, rec)
		}
		if promise != nil {
			promise(rec, err)
		}
	})
}

// ProduceSync synchronously produces rs.
// In ModeReplay (or offline client) it NEVER touches Kafka: captures each record as an Output
// ("kafka:<topic>") and returns nil-error ProduceResults.
// In ModeRecord it produces to Kafka and captures each succeeded record as an Output.
func (c *Client) ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	if c.isOffline() {
		out := make(kgo.ProduceResults, len(rs))
		for i, r := range rs {
			if r != nil && c.kv != nil && !c.kv.IsOff() {
				recordProducedOutput(ctx, c.kv, r)
			}
			out[i] = kgo.ProduceResult{Record: r, Err: nil}
		}
		return out
	}
	if c.kv == nil || c.kv.IsOff() {
		if c.raw == nil {
			out := make(kgo.ProduceResults, len(rs))
			err := errors.New("kavachkgo: nil underlying kgo.Client")
			for i, r := range rs {
				out[i] = kgo.ProduceResult{Record: r, Err: err}
			}
			return out
		}
		return c.raw.ProduceSync(ctx, rs...)
	}
	if c.raw == nil {
		out := make(kgo.ProduceResults, len(rs))
		err := errors.New("kavachkgo: nil underlying kgo.Client")
		for i, r := range rs {
			out[i] = kgo.ProduceResult{Record: r, Err: err}
		}
		return out
	}
	results := c.raw.ProduceSync(ctx, rs...)
	for _, pr := range results {
		if pr.Err == nil && pr.Record != nil {
			recordProducedOutput(ctx, c.kv, pr.Record)
		}
	}
	return results
}

// RecordToInput converts a consumed *kgo.Record into a Kavach step Input.
// Source is "kafka:<topic>", Position is "<partition>:<offset>", and Data is record.Value.
func RecordToInput(rec *kgo.Record) kavach.Input {
	if rec == nil {
		return kavach.Input{Source: "kafka:"}
	}
	var hdrs map[string]string
	if len(rec.Headers) > 0 {
		hdrs = make(map[string]string, len(rec.Headers))
		for _, h := range rec.Headers {
			hdrs[h.Key] = string(h.Value)
		}
	}
	return kavach.Input{
		Source:   "kafka:" + rec.Topic,
		Position: fmt.Sprintf("%d:%d", rec.Partition, rec.Offset),
		Key:      cloneBytes(rec.Key),
		Headers:  hdrs,
		Data:     cloneBytes(rec.Value),
	}
}

// InputToRecord reconstructs a *kgo.Record from a Kavach Input during Replay.
func InputToRecord(in kavach.Input, defaultTopic string) *kgo.Record {
	topic := defaultTopic
	if t, ok := strings.CutPrefix(in.Source, "kafka:"); ok && t != "" {
		topic = t
	}
	var part int32
	var offset int64
	if in.Position != "" {
		if pStr, oStr, ok := strings.Cut(in.Position, ":"); ok {
			if p, err := strconv.ParseInt(pStr, 10, 32); err == nil {
				part = int32(p)
			}
			if o, err := strconv.ParseInt(oStr, 10, 64); err == nil {
				offset = o
			}
		}
	}
	var hdrs []kgo.RecordHeader
	if len(in.Headers) > 0 {
		hdrs = make([]kgo.RecordHeader, 0, len(in.Headers))
		for k, v := range in.Headers {
			hdrs = append(hdrs, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
	}
	return &kgo.Record{
		Topic:     topic,
		Partition: part,
		Offset:    offset,
		Key:       cloneBytes(in.Key),
		Value:     cloneBytes(in.Data),
		Headers:   hdrs,
	}
}

// BatchConsumer adapts batch-style Kafka consumers with signature
// func(context.Context, []*kgo.Record) []*kgo.Record (matching Lago's ConsumerGroupConfig.ProcessRecords).
// It implements both kavach.StepRunner (for deterministic Replay via kv.ServeOrReplay)
// and kavach.Starter (for live Start(ctx) outside Replay).
type BatchConsumer struct {
	kv        *kavach.Kavach
	topic     string
	process   func(context.Context, []*kgo.Record) []*kgo.Record
	liveStart func(context.Context)
}

var (
	_ kavach.StepRunner = (*BatchConsumer)(nil)
	_ kavach.Starter    = (*BatchConsumer)(nil)
)

// WrapBatchHandler creates a BatchConsumer for replay and step-per-record recording.
func WrapBatchHandler(
	kv *kavach.Kavach,
	topic string,
	process func(context.Context, []*kgo.Record) []*kgo.Record,
) *BatchConsumer {
	return &BatchConsumer{
		kv:      kv,
		topic:   topic,
		process: process,
	}
}

// WrapBatchConsumer creates a BatchConsumer that also delegates Start(ctx) to liveStart
// in ModeOff / ModeRecord so kv.ServeOrReplay(consumer) works with 1 line in main.go.
func WrapBatchConsumer(
	kv *kavach.Kavach,
	topic string,
	process func(context.Context, []*kgo.Record) []*kgo.Record,
	liveStart func(context.Context),
) *BatchConsumer {
	return &BatchConsumer{
		kv:        kv,
		topic:     topic,
		process:   process,
		liveStart: liveStart,
	}
}

// ProcessBatch can be plugged directly into a ConsumerGroupConfig.ProcessRecords callback.
// In ModeRecord it wraps each consumed record in a Kavach BeginStep/EndStep lifecycle
// (marking uncommitted records with ErrRecordNotCommitted).
func (b *BatchConsumer) ProcessBatch(ctx context.Context, records []*kgo.Record) []*kgo.Record {
	if b.process == nil {
		return nil
	}
	if b.kv == nil || !b.kv.IsRecord() {
		return b.process(ctx, records)
	}
	committed := make([]*kgo.Record, 0, len(records))
	for _, rec := range records {
		stepCtx, sc := b.kv.BeginStep(ctx, RecordToInput(rec))
		done := b.process(stepCtx, []*kgo.Record{rec})
		if len(done) == 0 {
			_ = sc.End(ErrRecordNotCommitted)
			continue
		}
		_ = sc.End(nil)
		committed = append(committed, done...)
	}
	return committed
}

// RunStep implements kavach.StepRunner for deterministic Replay mode.
// Feeds one recorded/mutated *kgo.Record into process and returns ErrRecordNotCommitted
// when the handler declines to commit it (retryable error).
func (b *BatchConsumer) RunStep(ctx context.Context, in kavach.Input) error {
	if b.process == nil {
		return nil
	}
	rec := InputToRecord(in, b.topic)
	processed := b.process(ctx, []*kgo.Record{rec})
	if len(processed) < 1 {
		return ErrRecordNotCommitted
	}
	return nil
}

// Start implements kavach.Starter for normal ModeOff / ModeRecord execution.
func (b *BatchConsumer) Start(ctx context.Context) {
	if b.liveStart != nil {
		b.liveStart(ctx)
	}
}

// Run starts the consumer with ctx in ModeOff/ModeRecord, or runs the
// deterministic kavach-host loop in ModeReplay (preserving caller's shutdown ctx).
func (b *BatchConsumer) Run(ctx context.Context) error {
	if b.kv != nil && b.kv.IsReplay() {
		return b.kv.ServeHost(b)
	}
	b.Start(ctx)
	return nil
}

// RecordConsumer adapts single-record Kafka handlers func(context.Context, *kgo.Record) error.
// Implements both kavach.StepRunner and kavach.Server.
type RecordConsumer struct {
	kv        *kavach.Kavach
	topic     string
	handle    func(context.Context, *kgo.Record) error
	liveServe func(context.Context) error
}

var (
	_ kavach.StepRunner = (*RecordConsumer)(nil)
	_ kavach.Server     = (*RecordConsumer)(nil)
)

// WrapHandler creates a single-record Kafka consumer runner compatible with kv.ServeOrReplay.
func WrapHandler(
	kv *kavach.Kavach,
	topic string,
	handle func(context.Context, *kgo.Record) error,
	liveServe func(context.Context) error,
) *RecordConsumer {
	return &RecordConsumer{
		kv:        kv,
		topic:     topic,
		handle:    handle,
		liveServe: liveServe,
	}
}

// HandleRecord wraps a single live consumed record in BeginStep/EndStep in ModeRecord.
func (c *RecordConsumer) HandleRecord(ctx context.Context, rec *kgo.Record) error {
	if c.handle == nil {
		return nil
	}
	if c.kv == nil || !c.kv.IsRecord() {
		return c.handle(ctx, rec)
	}
	stepCtx, sc := c.kv.BeginStep(ctx, RecordToInput(rec))
	err := c.handle(stepCtx, rec)
	_ = sc.End(err)
	return err
}

// RunStep implements kavach.StepRunner for deterministic Replay mode.
func (c *RecordConsumer) RunStep(ctx context.Context, in kavach.Input) error {
	if c.handle == nil {
		return nil
	}
	return c.handle(ctx, InputToRecord(in, c.topic))
}

// Serve implements kavach.Server for normal ModeOff / ModeRecord execution.
func (c *RecordConsumer) Serve(ctx context.Context) error {
	if c.liveServe != nil {
		return c.liveServe(ctx)
	}
	return nil
}

// NewRecordHook returns a lightweight kgo.Hook (HookProduceRecordUnbuffered)
// for recording produced Kafka records when passing kgo.WithHooks(...) to a raw *kgo.Client.
func NewRecordHook(kv *kavach.Kavach) kgo.Hook {
	return &produceRecordHook{kv: kv}
}

type produceRecordHook struct {
	kv *kavach.Kavach
}

var _ kgo.HookProduceRecordUnbuffered = (*produceRecordHook)(nil)

func (h *produceRecordHook) OnProduceRecordUnbuffered(r *kgo.Record, err error) {
	if h.kv == nil || !h.kv.IsRecord() || err != nil || r == nil {
		return
	}
	recordProducedOutput(context.Background(), h.kv, r)
}

func recordProducedOutput(ctx context.Context, kv *kavach.Kavach, r *kgo.Record) {
	if kv == nil || r == nil {
		return
	}
	kv.RecordOutput(ctx, kavach.Output{
		Sink: "kafka:" + r.Topic,
		Key:  string(r.Key),
		Data: encodeOutputData(r),
	})
}

// encodeOutputData returns record.Value directly when it is valid JSON (so
// declarative invariants like $.transaction_id match out of the box), or wraps
// non-JSON/empty payloads in a structured JSON envelope.
func encodeOutputData(r *kgo.Record) []byte {
	trimmed := bytes.TrimSpace(r.Value)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		var compact bytes.Buffer
		if json.Compact(&compact, trimmed) == nil {
			return compact.Bytes()
		}
		return cloneBytes(trimmed)
	}
	env := struct {
		Topic   string            `json:"topic"`
		Key     string            `json:"key,omitempty"`
		Value   []byte            `json:"value,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}{
		Topic: r.Topic,
		Key:   string(r.Key),
		Value: r.Value,
	}
	if len(r.Headers) > 0 {
		env.Headers = make(map[string]string, len(r.Headers))
		for _, h := range r.Headers {
			env.Headers[h.Key] = string(h.Value)
		}
	}
	b, _ := json.Marshal(env)
	return b
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
