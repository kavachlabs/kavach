package kmap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoSpecFromDirSynthesizesValidMapAndHTML(t *testing.T) {
	dir := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", `module github.com/acme/payments-service

go 1.23

require (
	github.com/jackc/pgx/v5 v5.7.0
	github.com/redis/go-redis/v9 v9.7.0
	github.com/twmb/franz-go v1.18.0
)
`)

	write("main.go", `package main

import (
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/charge", HandleCharge)
	_ = http.ListenAndServe(":8080", mux)
}
`)

	write("handlers/charge.go", `package handlers

import (
	"context"
	"net/http"
	"github.com/twmb/franz-go/pkg/kgo"
)

type Store interface {
	Query(ctx context.Context, sql string) error
	Exec(ctx context.Context, sql string) error
}

type Redis interface {
	Get(ctx context.Context, key string) string
}

type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record)
}

func HandleCharge(w http.ResponseWriter, r *http.Request) {
	var db Store
	var rdb Redis
	var kafka Producer
	_ = db.Query(r.Context(), "SELECT balance FROM accounts")
	_ = rdb.Get(r.Context(), "idemp:1")
	_ = db.Exec(r.Context(), "INSERT INTO charges VALUES (1)")
	kafka.ProduceSync(r.Context(), &kgo.Record{Topic: "charges_settled"})
	w.WriteHeader(http.StatusOK)
}
`)

	write(".kavach/invariants.yaml", `service: payments-service
invariants:
  - name: atomic_single_charge
    kind: at_most_once_emit
    sink: kafka:charges_settled
    key: $.transaction_id
    on_error_emit_count: 0

  - name: no_silent_drop
    kind: anti_lobotomy
    sinks: [kafka:charges_settled, http:response]
    min_ratio_vs_baseline: 1.0
`)

	spec, err := AutoSpecFromDir(dir, AutoOptions{})
	if err != nil {
		t.Fatalf("AutoSpecFromDir: %v", err)
	}
	if err := spec.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate: %v", err)
	}
	if spec.Service != "payments-service" {
		t.Fatalf("service = %q, want payments-service", spec.Service)
	}

	nodeKinds := map[string]string{}
	for _, n := range spec.Nodes {
		nodeKinds[n.ID] = n.Kind
	}
	for _, wantID := range []string{"gw_postgres", "gw_redis", "gw_kafka", "sink_kafka_charges_settled", "inv_atomic_single_charge", "inv_no_silent_drop"} {
		if _, ok := nodeKinds[wantID]; !ok {
			t.Errorf("missing expected node %q in %v", wantID, nodeKinds)
		}
	}
	if len(spec.Edges) == 0 {
		t.Fatal("expected synthesized edges")
	}
	if len(spec.Flows) == 0 {
		t.Fatal("expected Day-0 guided flow")
	}

	// Write map.json and round-trip through strict LoadSpec + Build + RenderHTML.
	mapJSON := filepath.Join(dir, ".kavach", "map.json")
	if err := WriteSpecFile(mapJSON, spec); err != nil {
		t.Fatalf("WriteSpecFile: %v", err)
	}
	loaded, err := LoadSpec(mapJSON)
	if err != nil {
		t.Fatalf("LoadSpec round-trip: %v", err)
	}
	m, err := Build(Input{Spec: loaded, BaseDir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var htmlBuf bytes.Buffer
	if err := RenderHTML(&htmlBuf, m); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(htmlBuf.String(), `id="kavach-map-data"`) {
		t.Fatal("rendered HTML missing embedded kavach-map-data")
	}
	if !strings.Contains(htmlBuf.String(), "atomic_single_charge") {
		t.Fatal("rendered HTML missing declarative invariant node")
	}
}

func TestAutoSpecFromDirTypeScript(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("package.json", `{
  "name": "ts-events-processor",
  "dependencies": {
    "express": "^4.19.0",
    "pg": "^8.12.0",
    "ioredis": "^5.4.0",
    "kafkajs": "^2.2.4"
  }
}`)
	write("src/index.ts", `import express from "express";
import { Pool } from "pg";
import Redis from "ioredis";
import { Kafka } from "kafkajs";

const pool = new Pool();
const redis = new Redis();
const kafka = new Kafka({ brokers: ["localhost:9092"] });
const producer = kafka.producer();
const consumer = kafka.consumer({ groupId: "events" });

const app = express();
app.post("/v1/events", ingestEvent);

async function ingestEvent(req: any, res: any) {
  await pool.query("SELECT * FROM subscriptions");
  await pool.query("INSERT INTO events VALUES ($1)", [1]);
  await redis.zadd("subscription_refreshed_v2", 1, "sub-1");
  await fetch("https://billing.example/webhook");
  res.json({ ok: true });
}

export async function run() {
  await consumer.subscribe({ topic: "events_raw" });
  await consumer.run({
    eachMessage: async () => {
      await producer.send({
        topic: "events_charged_in_advance",
        messages: [{ value: "{}" }],
      });
    },
  });
}
`)
	write(".kavach/invariants.yaml", `service: ts-events-processor
invariants:
  - name: atomic_single_emit_on_retry
    kind: at_most_once_emit
    sink: kafka:events_charged_in_advance
    key: $.transaction_id
    on_error_emit_count: 0
`)

	spec, err := AutoSpecFromDir(dir, AutoOptions{})
	if err != nil {
		t.Fatalf("AutoSpecFromDir(TS): %v", err)
	}
	if err := spec.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate(TS): %v", err)
	}
	if spec.Service != "ts-events-processor" {
		t.Fatalf("service = %q, want ts-events-processor", spec.Service)
	}

	nodeKinds := map[string]string{}
	for _, n := range spec.Nodes {
		nodeKinds[n.ID] = n.Kind
	}
	for _, want := range []string{"gw_postgres", "gw_redis", "gw_kafka", "gw_http", "sink_kafka_events_charged_in_advance", "sink_http_response", "inv_atomic_single_emit_on_retry"} {
		if _, ok := nodeKinds[want]; !ok {
			t.Errorf("missing expected TS node %q in %v", want, nodeKinds)
		}
	}
	if len(spec.Edges) == 0 || len(spec.Flows) == 0 {
		t.Fatalf("expected non-empty TS edges & flows: edges=%d flows=%d", len(spec.Edges), len(spec.Flows))
	}

	mapJSON := filepath.Join(dir, ".kavach", "map.json")
	if err := WriteSpecFile(mapJSON, spec); err != nil {
		t.Fatalf("WriteSpecFile: %v", err)
	}
	loaded, err := LoadSpec(mapJSON)
	if err != nil {
		t.Fatalf("LoadSpec round-trip: %v", err)
	}
	m, err := Build(Input{Spec: loaded, BaseDir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var htmlBuf bytes.Buffer
	if err := RenderHTML(&htmlBuf, m); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(htmlBuf.String(), "atomic_single_emit_on_retry") {
		t.Fatal("rendered TS HTML missing invariant node")
	}
}
