package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/internal/kmap"
)

func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRepoGoTSRust(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFile(t, dir, "go.mod", `module github.com/acme/events-processor

go 1.23

require (
	github.com/jackc/pgx/v5 v5.7.1
	gorm.io/gorm v1.25.12
	github.com/redis/go-redis/v9 v9.7.0
	github.com/twmb/franz-go v1.18.0
	github.com/go-chi/chi/v5 v5.1.0
)

require (
	google.golang.org/grpc v1.67.0 // indirect
)
`)
		det, err := DetectRepo(dir)
		if err != nil {
			t.Fatalf("DetectRepo: %v", err)
		}
		if det.Language != LangGo {
			t.Fatalf("language = %q, want go", det.Language)
		}
		if det.Service != "events-processor" {
			t.Fatalf("service = %q, want events-processor", det.Service)
		}
		got := map[string]bool{}
		for _, d := range det.Drivers {
			got[d.Name] = true
		}
		for _, want := range []string{"pgx/v5", "gorm", "go-redis/v9", "franz-go", "chi"} {
			if !got[want] {
				t.Errorf("missing expected Go driver %q in %v", want, got)
			}
		}
		if got["grpc"] {
			t.Errorf("indirect grpc dependency should not be reported as a direct driver")
		}
	})

	t.Run("typescript", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFile(t, dir, "package.json", `{
  "name": "@acme/billing-api",
  "dependencies": {
    "express": "^4.19.0",
    "pg": "^8.12.0",
    "@prisma/client": "^5.20.0",
    "drizzle-orm": "^0.33.0",
    "ioredis": "^5.4.0",
    "kafkajs": "^2.2.4",
    "fastify": "^4.28.0",
    "hono": "^4.5.0"
  }
}`)
		det, err := DetectRepo(dir)
		if err != nil {
			t.Fatalf("DetectRepo: %v", err)
		}
		if det.Language != LangTypeScript {
			t.Fatalf("language = %q, want typescript", det.Language)
		}
		if det.Service != "billing-api" {
			t.Fatalf("service = %q, want billing-api", det.Service)
		}
		got := map[string]bool{}
		for _, d := range det.Drivers {
			got[d.Name] = true
		}
		for _, want := range []string{"pg", "prisma", "drizzle-orm", "ioredis", "kafkajs", "express", "fastify", "hono"} {
			if !got[want] {
				t.Errorf("missing expected TS driver %q in %v", want, got)
			}
		}
	})

	t.Run("rust", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFile(t, dir, "Cargo.toml", `[package]
name = "ledger_engine"
version = "0.1.0"

[dependencies]
axum = "0.7"
sqlx = { version = "0.8", features = ["postgres"] }
redis = "0.27"
rdkafka = "0.36"
`)
		det, err := DetectRepo(dir)
		if err != nil {
			t.Fatalf("DetectRepo: %v", err)
		}
		if det.Language != LangRust {
			t.Fatalf("language = %q, want rust", det.Language)
		}
		if det.Service != "ledger_engine" {
			t.Fatalf("service = %q, want ledger_engine", det.Service)
		}
		got := map[string]bool{}
		for _, d := range det.Drivers {
			got[d.Name] = true
		}
		for _, want := range []string{"axum", "sqlx", "redis", "rdkafka"} {
			if !got[want] {
				t.Errorf("missing expected Rust driver %q in %v", want, got)
			}
		}
	})
}

func TestScanRepoGoASTDoctor(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "go.mod", `module github.com/acme/checkout

go 1.23

require (
	github.com/jackc/pgx/v5 v5.7.0
	github.com/redis/go-redis/v9 v9.7.0
	github.com/twmb/franz-go v1.18.0
	github.com/google/uuid v1.6.0
)
`)
	writeFixtureFile(t, dir, "main.go", `package main

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

func main() {
	ctx := context.Background() // ok in main
	_, _ = pgxpool.New(ctx, "postgres://localhost/db")
	_, _ = sql.Open("postgres", "postgres://localhost/db")
	_ = redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	_, _ = kgo.NewClient(kgo.ConsumeTopics("orders_in"), kgo.DefaultProduceTopic("orders_billed"))
	mux := http.NewServeMux()
	mux.HandleFunc("/orders", HandleOrder)
	_ = http.ListenAndServe(":8080", mux)
}
`)
	writeFixtureFile(t, dir, "handlers/order.go", `package handlers

import (
	"context"
	crand "crypto/rand"
	"math/rand"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func HandleOrder(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	_ = time.Since(now)
	_ = time.Until(now)
	_ = rand.Intn(100)
	buf := make([]byte, 8)
	_, _ = crand.Read(buf)
	_ = uuid.NewString()

	_ = context.Background()
	go func() {
		_ = context.TODO()
	}()
	w.WriteHeader(http.StatusOK)
}
`)
	// Test files must be skipped.
	writeFixtureFile(t, dir, "handlers/order_test.go", `package handlers

import "time"

func TestHelper() { _ = time.Now() }
`)

	rep, err := ScanRepo(dir)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	if rep.FilesScanned != 2 {
		t.Fatalf("FilesScanned = %d, want 2 (test files skipped)", rep.FilesScanned)
	}
	if rep.DurationMS >= 2000 {
		t.Fatalf("ScanRepo took %.1fms, want <2000ms", rep.DurationMS)
	}
	if rep.RecommendedMode != "driver" {
		t.Fatalf("RecommendedMode = %q, want driver", rep.RecommendedMode)
	}
	if len(rep.InitSites) < 5 {
		t.Fatalf("expected >=5 driver init sites, got %+v", rep.InitSites)
	}
	if len(rep.ClockCalls) != 3 {
		t.Fatalf("ClockCalls = %d (%+v), want 3 (Now, Since, Until)", len(rep.ClockCalls), rep.ClockCalls)
	}
	if len(rep.RandCalls) != 3 {
		t.Fatalf("RandCalls = %d (%+v), want 3 (math/rand, crypto/rand, uuid)", len(rep.RandCalls), rep.RandCalls)
	}
	if len(rep.DetachedContexts) != 2 {
		t.Fatalf("DetachedContexts = %d (%+v), want 2 (handler + goroutine; not main)", len(rep.DetachedContexts), rep.DetachedContexts)
	}
	if len(rep.Handlers) == 0 {
		t.Fatal("expected detected handlers")
	}
	for _, want := range []string{"kavachpgx", "kavachredis", "kavachkgo", "kavachhttp", "kavach.Init"} {
		if !strings.Contains(rep.SuggestedSnippet, want) {
			t.Errorf("SuggestedSnippet missing %q:\n%s", want, rep.SuggestedSnippet)
		}
	}
}

func TestInitScaffoldsAndRespectsForce(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "go.mod", `module github.com/acme/billing

go 1.23

require (
	github.com/twmb/franz-go v1.18.0
	github.com/redis/go-redis/v9 v9.7.0
)
`)
	writeFixtureFile(t, dir, "processor.go", `package main

import (
	"context"
	"github.com/twmb/franz-go/pkg/kgo"
)

func ProcessEvent(ctx context.Context, rec *kgo.Record) error {
	_ = &kgo.Record{Topic: "events_charged_in_advance"}
	return nil
}
`)

	res, err := Init(InitOptions{Dir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Service != "billing" || res.Mode != "driver" {
		t.Fatalf("Init result = %+v", res)
	}
	if res.MapNodes == 0 || res.MapEdges == 0 || res.MapFlows == 0 {
		t.Fatalf("expected non-empty Day-0 map: nodes=%d edges=%d flows=%d", res.MapNodes, res.MapEdges, res.MapFlows)
	}

	for _, rel := range []string{
		".kavach/config.yaml",
		".kavach/invariants.yaml",
		".kavach/map.json",
		".kavach/map.html",
		".github/workflows/kavach.yml",
	} {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
		if len(data) == 0 {
			t.Fatalf("expected %s to be non-empty", rel)
		}
	}

	// Verify config.yaml, invariants.yaml, and map.json pass strict parsers, and map.html embeds map data.
	if _, err := kconfig.LoadConfig(filepath.Join(dir, ".kavach", "config.yaml")); err != nil {
		t.Fatalf("kconfig.LoadConfig(.kavach/config.yaml): %v", err)
	}
	if _, err := kconfig.LoadInvariants(filepath.Join(dir, ".kavach", "invariants.yaml")); err != nil {
		t.Fatalf("kconfig.LoadInvariants(.kavach/invariants.yaml): %v", err)
	}
	if _, err := kmap.LoadSpec(filepath.Join(dir, ".kavach", "map.json")); err != nil {
		t.Fatalf("LoadSpec(.kavach/map.json): %v", err)
	}
	htmlBytes, _ := os.ReadFile(filepath.Join(dir, ".kavach", "map.html"))
	if !strings.Contains(string(htmlBytes), `id="kavach-map-data"`) {
		t.Fatal(".kavach/map.html missing kavach-map-data")
	}
	wfBytes, _ := os.ReadFile(filepath.Join(dir, ".github", "workflows", "kavach.yml"))
	if !strings.Contains(string(wfBytes), "kavach diff .kavach/capsules/*.kavach") || !strings.Contains(string(wfBytes), "--variants 25") {
		t.Fatalf("workflow missing kavach diff command:\n%s", wfBytes)
	}

	// Re-run without --force: all files should be skipped.
	res2, err := Init(InitOptions{Dir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	for _, f := range res2.Files {
		if f.Status != "skipped" {
			t.Errorf("second Init without --force: %s status = %q, want skipped", f.Path, f.Status)
		}
	}

	// Re-run with --force: all files should be overwritten.
	res3, err := Init(InitOptions{Dir: dir, Force: true, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("force Init: %v", err)
	}
	for _, f := range res3.Files {
		if f.Status != "overwritten" {
			t.Errorf("force Init: %s status = %q, want overwritten", f.Path, f.Status)
		}
	}
}

func TestScanRepoAndInitTypeScript(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "package.json", `{
  "name": "@acme/ts-billing",
  "main": "dist/server.js",
  "dependencies": {
    "express": "^4.19.0",
    "pg": "^8.12.0",
    "@prisma/client": "^5.20.0",
    "drizzle-orm": "^0.33.0",
    "ioredis": "^5.4.0",
    "kafkajs": "^2.2.4",
    "next": "^15.0.0"
  }
}`)
	writeFixtureFile(t, dir, "src/server.ts", `import express from "express";
import { Pool } from "pg";
import { PrismaClient } from "@prisma/client";
import { drizzle } from "drizzle-orm/node-postgres";
import Redis from "ioredis";
import { Kafka } from "kafkajs";
import crypto from "node:crypto";

// Commented out Date.now() and Math.random() must not trigger:
/* new Date(); Math.random(); */

const pool = new Pool({ connectionString: process.env.DATABASE_URL });
const prisma = new PrismaClient();
const db = drizzle(pool);
const redis = new Redis(process.env.REDIS_URL!);
const kafka = new Kafka({ brokers: ["localhost:9092"] });
const producer = kafka.producer();
const consumer = kafka.consumer({ groupId: "billing" });

const app = express();
app.post("/v1/charge", handleCharge);

async function handleCharge(req: any, res: any) {
  const ts = Date.now();
  const createdAt = new Date();
  const deterministic = new Date(ts); // not empty-arg new Date(), must not trigger
  const jitter = Math.random();
  const id = crypto.randomUUID();
  await pool.query("SELECT balance FROM accounts WHERE id = $1", [id]);
  await redis.zadd("subscription_refreshed_v2", ts, id);
  await producer.send({
    topic: "events_charged_in_advance",
    messages: [{ value: JSON.stringify({ transaction_id: id, createdAt, deterministic, jitter }) }],
  });
  res.json({ ok: true });
}

export async function startConsumer() {
  await consumer.subscribe({ topic: "events_in" });
  await consumer.run({
    eachMessage: async ({ message }) => {
      await prisma.event.findFirst({ where: { id: "1" } });
      await db.select().from("charges" as any);
    },
  });
}
`)
	writeFixtureFile(t, dir, "src/app/api/invoices/route.ts", `export async function POST(req: Request) {
  return Response.json({ created: true });
}
`)
	// Test files and node_modules must be skipped.
	writeFixtureFile(t, dir, "src/server.test.ts", `const x = Date.now(); const y = Math.random();`)
	writeFixtureFile(t, dir, "node_modules/fake/index.js", `const z = Date.now();`)

	rep, err := ScanRepo(dir)
	if err != nil {
		t.Fatalf("ScanRepo(TS): %v", err)
	}
	if rep.Language != LangTypeScript || rep.Service != "ts-billing" {
		t.Fatalf("unexpected TS detection: lang=%q service=%q", rep.Language, rep.Service)
	}
	if rep.FilesScanned != 2 {
		t.Fatalf("FilesScanned = %d, want 2 (test & node_modules skipped)", rep.FilesScanned)
	}
	if len(rep.InitSites) < 7 {
		t.Fatalf("expected >=7 TS driver init sites, got %+v", rep.InitSites)
	}
	if len(rep.ClockCalls) != 2 {
		t.Fatalf("ClockCalls = %d (%+v), want 2 (Date.now + empty new Date)", len(rep.ClockCalls), rep.ClockCalls)
	}
	if len(rep.RandCalls) != 2 {
		t.Fatalf("RandCalls = %d (%+v), want 2 (Math.random + crypto.randomUUID)", len(rep.RandCalls), rep.RandCalls)
	}
	if len(rep.Routes) < 2 {
		t.Fatalf("expected >=2 routes (Express + Next.js App Router), got %v", rep.Routes)
	}
	if len(rep.Topics) < 2 {
		t.Fatalf("expected >=2 Kafka topics, got %v", rep.Topics)
	}
	for _, want := range []string{`"@kavachlabs/core"`, `"@kavachlabs/pg"`, `"@kavachlabs/ioredis"`, `"@kavachlabs/kafkajs"`, `"@kavachlabs/http"`, "serveOrReplay"} {
		if !strings.Contains(rep.SuggestedSnippet, want) {
			t.Errorf("TS SuggestedSnippet missing %q:\n%s", want, rep.SuggestedSnippet)
		}
	}

	res, err := Init(InitOptions{Dir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("Init(TS): %v", err)
	}
	if res.Language != LangTypeScript || res.Mode != "driver" {
		t.Fatalf("unexpected TS Init result: %+v", res)
	}
	if res.MapNodes == 0 || res.MapEdges == 0 || res.MapFlows == 0 {
		t.Fatalf("expected non-empty TS Day-0 map: nodes=%d edges=%d flows=%d", res.MapNodes, res.MapEdges, res.MapFlows)
	}
	if _, err := kconfig.LoadConfig(filepath.Join(dir, ".kavach", "config.yaml")); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, err := kconfig.LoadInvariants(filepath.Join(dir, ".kavach", "invariants.yaml")); err != nil {
		t.Fatalf("LoadInvariants: %v", err)
	}
	if _, err := kmap.LoadSpec(filepath.Join(dir, ".kavach", "map.json")); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	wfBytes, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "kavach.yml"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	for _, want := range []string{"actions/setup-node@v4", "node-version: '22'", "npm ci", "node dist/server.js", "kavach diff .kavach/capsules/*.kavach", "--variants 25"} {
		if !strings.Contains(string(wfBytes), want) {
			t.Errorf("TS workflow missing %q:\n%s", want, wfBytes)
		}
	}
}

func TestScanRepoAndInitTypeScriptPeerAndKavachWrapped(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "package.json", `{
  "name": "@kavachlabs/example-l1-billing",
  "dependencies": {
    "@kavachlabs/core": "0.3.0",
    "@kavachlabs/pg": "0.3.0",
    "@kavachlabs/ioredis": "0.3.0",
    "@kavachlabs/kafkajs": "0.3.0"
  },
  "peerDependencies": {
    "pg": "^8.13.0",
    "ioredis": "^5.4.0",
    "kafkajs": "^2.2.4"
  },
  "optionalDependencies": {
    "express": "^4.19.0"
  }
}`)
	writeFixtureFile(t, dir, "old.ts", `import { init } from "@kavachlabs/core";
import { createReplayRedis } from "@kavachlabs/ioredis";
import { createReplayKafka } from "@kavachlabs/kafkajs";
import { createReplayPool } from "@kavachlabs/pg";

const kv = init({ service: "l1-billing-ts", pii: "standard" });
const pool = createReplayPool(kv);
const redis = createReplayRedis(kv);
const kafka = createReplayKafka(kv);
const producer = kafka.producer();
const consumer = kafka.consumer({ groupId: "l1" });
await consumer.subscribe({ topic: "events_raw" });
await consumer.run({
  eachMessage: async () => {
    await pool.query("SELECT id FROM charges");
    await redis.zadd("subscription_refreshed_v2", 1, "sub-1");
    await producer.send({ topic: "events_charged_in_advance", messages: [{ value: "{}" }] });
  },
});
`)

	res, err := Init(InitOptions{Dir: dir, Version: "0.2.0"})
	if err != nil {
		t.Fatalf("Init(peer+@kavach): %v", err)
	}
	if res.Mode != "driver" {
		t.Fatalf("mode = %q, want driver", res.Mode)
	}
	gotDrivers := map[string]bool{}
	for _, d := range res.Drivers {
		gotDrivers[d.Name] = true
	}
	for _, want := range []string{"pg", "ioredis", "kafkajs", "express"} {
		if !gotDrivers[want] {
			t.Errorf("missing driver %q from peer/optional/@kavach deps: %v", want, gotDrivers)
		}
	}
	inv, err := kconfig.LoadInvariants(filepath.Join(dir, ".kavach", "invariants.yaml"))
	if err != nil {
		t.Fatalf("LoadInvariants: %v", err)
	}
	foundKafkaSink := false
	for _, r := range inv.Invariants {
		for _, s := range r.TargetSinks() {
			if strings.HasPrefix(s, "kafka:") {
				foundKafkaSink = true
			}
		}
	}
	if !foundKafkaSink {
		t.Fatalf("expected kafka:* sink in generated invariants.yaml, got %+v", inv.Invariants)
	}
	spec, err := kmap.LoadSpec(filepath.Join(dir, ".kavach", "map.json"))
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	nodes := map[string]bool{}
	for _, n := range spec.Nodes {
		nodes[n.ID] = true
	}
	for _, want := range []string{"gw_postgres", "gw_redis", "gw_kafka"} {
		if !nodes[want] {
			t.Errorf("missing expected gateway node %q in map.json (%v)", want, nodes)
		}
	}
}
