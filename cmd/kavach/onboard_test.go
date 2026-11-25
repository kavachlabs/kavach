package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/internal/kmap"
	"github.com/kavachlabs/kavach/internal/onboard"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

func makeSampleGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", `module github.com/acme/events-processor

go 1.23

require (
	github.com/jackc/pgx/v5 v5.7.0
	github.com/redis/go-redis/v9 v9.7.0
	github.com/twmb/franz-go v1.18.0
)
`)
	write("main.go", `package main

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

func main() {
	ctx := context.Background()
	_, _ = pgxpool.New(ctx, "postgres://localhost/db")
	_ = redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	_, _ = kgo.NewClient(kgo.ConsumeTopics("events_in"), kgo.DefaultProduceTopic("events_charged_in_advance"))
	mux := http.NewServeMux()
	mux.HandleFunc("/health", Health)
	_ = http.ListenAndServe(":8080", mux)
}

func Health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
`)
	write("processors/events.go", `package processors

import (
	"context"
	"math/rand"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

func ProcessEvents(ctx context.Context, rec *kgo.Record) error {
	_ = time.Now()
	_ = rand.Intn(10)
	_ = context.Background()
	_ = &kgo.Record{Topic: "events_charged_in_advance"}
	return nil
}
`)
	return dir
}

func TestCmdInitAndDoctorCLI(t *testing.T) {
	dir := makeSampleGoRepo(t)

	// 1. kavach doctor --json
	code, out, errb := runCLI(t, "doctor", "--dir", dir, "--json")
	if code != exitPass {
		t.Fatalf("doctor --json exit %d: %s", code, errb)
	}
	var rep onboard.DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("decode doctor json: %v\n%s", err, out)
	}
	if rep.Service != "events-processor" || rep.Language != "go" {
		t.Fatalf("unexpected doctor report: service=%q lang=%q", rep.Service, rep.Language)
	}
	if len(rep.ClockCalls) == 0 || len(rep.RandCalls) == 0 || len(rep.DetachedContexts) == 0 {
		t.Fatalf("expected clock/rand/context findings: clock=%d rand=%d ctx=%d",
			len(rep.ClockCalls), len(rep.RandCalls), len(rep.DetachedContexts))
	}

	// Human output smoke check.
	code, out, errb = runCLI(t, "doctor", "--dir", dir)
	if code != exitPass || !strings.Contains(out, "Suggested 1-line kavach-go wrap snippet") {
		t.Fatalf("doctor human exit %d stderr=%s\n%s", code, errb, out)
	}

	// 2. kavach init --dir <dir> --json
	code, out, errb = runCLI(t, "init", "--dir", dir, "--json")
	if code != exitPass {
		t.Fatalf("init --json exit %d: %s", code, errb)
	}
	var initRes onboard.InitResult
	if err := json.Unmarshal([]byte(out), &initRes); err != nil {
		t.Fatalf("decode init json: %v\n%s", err, out)
	}
	if initRes.MapNodes == 0 || initRes.MapEdges == 0 {
		t.Fatalf("expected Day-0 map nodes & edges, got %+v", initRes)
	}

	// Verify kconfig parses generated YAML files cleanly.
	cfg, err := kconfig.LoadConfig(filepath.Join(dir, ".kavach", "config.yaml"))
	if err != nil {
		t.Fatalf("kconfig.LoadConfig: %v", err)
	}
	if cfg.Service != "events-processor" || cfg.Mode != kconfig.ModeDriver || cfg.Variants != 25 {
		t.Fatalf("unexpected parsed config: %+v", cfg)
	}
	inv, err := kconfig.LoadInvariants(filepath.Join(dir, ".kavach", "invariants.yaml"))
	if err != nil {
		t.Fatalf("kconfig.LoadInvariants: %v", err)
	}
	if len(inv.Invariants) < 2 {
		t.Fatalf("expected >=2 default invariants, got %+v", inv.Invariants)
	}
	if _, err := kmap.LoadSpec(filepath.Join(dir, ".kavach", "map.json")); err != nil {
		t.Fatalf("kmap.LoadSpec: %v", err)
	}
}

func TestCmdMapAutoAndZeroArgDefaultAndOpen(t *testing.T) {
	dir := makeSampleGoRepo(t)

	// 1. kavach map --auto --dir <dir> --open with KAVACH_NO_OPEN=1 must NOT open browser.
	opened := ""
	origOpen := openBrowserFn
	openBrowserFn = func(target string) error {
		opened = target
		return nil
	}
	defer func() { openBrowserFn = origOpen }()

	t.Setenv("KAVACH_NO_OPEN", "1")
	code, out, errb := runCLI(t, "map", "--auto", "--dir", dir, "--open")
	if code != exitPass {
		t.Fatalf("map --auto exit %d stderr=%s stdout=%s", code, errb, out)
	}
	if opened != "" {
		t.Fatalf("KAVACH_NO_OPEN=1 ignored, opened %q", opened)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kavach", "map.json")); err != nil {
		t.Fatalf("expected .kavach/map.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kavach", "map.html")); err != nil {
		t.Fatalf("expected .kavach/map.html: %v", err)
	}

	// 2. Zero-arg `kavach map --open` in an initialized dir uses .kavach/map.json -> .kavach/map.html.
	t.Setenv("KAVACH_NO_OPEN", "")
	code, out, errb = runCLI(t, "map", "--dir", dir, "--open")
	if code != exitPass {
		t.Fatalf("zero-arg map exit %d stderr=%s stdout=%s", code, errb, out)
	}
	if !strings.HasSuffix(opened, filepath.Join(".kavach", "map.html")) {
		t.Fatalf("opened = %q, want suffix .kavach/map.html", opened)
	}
}

func TestCmdSkillsAndInstallMCP(t *testing.T) {
	// Dry-run skills for all agents.
	code, out, errb := runCLI(t, "skills", "--agent", "all", "--dry-run", "--json")
	if code != exitPass {
		t.Fatalf("skills --dry-run exit %d: %s", code, errb)
	}
	var skResp struct {
		DryRun  bool                 `json:"dry_run"`
		Actions []skillInstallAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &skResp); err != nil {
		t.Fatalf("decode skills json: %v\n%s", err, out)
	}
	if !skResp.DryRun || len(skResp.Actions) != 4 {
		t.Fatalf("unexpected skills dry-run response: %+v", skResp)
	}

	// Real install into a temp dir.
	dst := filepath.Join(t.TempDir(), "skills")
	code, _, errb = runCLI(t, "skills", "--dir", dst)
	if code != exitPass {
		t.Fatalf("skills --dir exit %d: %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dst, "kavach-setup", "SKILL.md")); err != nil {
		t.Fatalf("expected installed kavach-setup/SKILL.md: %v", err)
	}

	// install-mcp --dry-run does not create file.
	mcpPath := filepath.Join(t.TempDir(), ".mcp.json")
	code, out, errb = runCLI(t, "install-mcp", "--target", mcpPath, "--dry-run")
	if code != exitPass {
		t.Fatalf("install-mcp --dry-run exit %d: %s", code, errb)
	}
	if _, err := os.Stat(mcpPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not write %s", mcpPath)
	}
	if !strings.Contains(out, `"kavach"`) {
		t.Fatalf("dry-run output missing kavach config:\n%s", out)
	}

	// install-mcp merges with existing mcpServers cleanly.
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"other":{"command":"foo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb = runCLI(t, "install-mcp", "--target", mcpPath)
	if code != exitPass {
		t.Fatalf("install-mcp exit %d: %s", code, errb)
	}
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse merged .mcp.json: %v", err)
	}
	if parsed["mcpServers"]["other"] == nil || parsed["mcpServers"]["kavach"] == nil {
		t.Fatalf("merged mcpServers lost entries: %+v", parsed)
	}
}

func TestDevRingConsumerAndCollector(t *testing.T) {
	tmp := t.TempDir()
	ringPath := filepath.Join(tmp, "test.shm")
	outDir := filepath.Join(tmp, "capsules")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	consumer, err := createDevRing(ringPath, 65536)
	if err != nil {
		t.Fatalf("createDevRing: %v", err)
	}
	defer consumer.Close()

	// Build a realistic KVRING02 stream using internal/recstream Encoder into a buffer,
	// then publish it into the mmap ring as an external process (like sdk/kavach-go/ring) would.
	var payload bytes.Buffer
	enc := recstream.NewEncoder(&payload)
	if err := enc.Open(recstream.Open{
		Service: "orders-svc",
		Start:   journal.StartGenesis,
	}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Facts([]journal.Fact{{Key: "env.region", Form: journal.FactValue, Value: []byte("us-east-1")}}); err != nil {
		t.Fatal(err)
	}
	recs := []journal.Record{
		{Type: journal.TypeInput, Source: "http:POST /orders", Data: []byte(`{"id":"ord-1"}`)},
		{Type: journal.TypeClock, UnixNanos: 1700000000000000000},
		{Type: journal.TypeGateway, Flags: journal.FlagCritical, Gateway: "redis:get", Request: []byte(`["ord-1"]`), Response: []byte(`"ok"`)},
		{Type: journal.TypeOutput, Sink: "kafka:orders_created", Data: []byte(`{"id":"ord-1"}`)},
	}
	if err := enc.Records(recs, true); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	publishIntoRingFile(t, ringPath, payload.Bytes())

	drained, err := consumer.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(drained) != payload.Len() {
		t.Fatalf("drained %d bytes, want %d", len(drained), payload.Len())
	}

	col := newDevStreamCollector("", outDir)
	if err := col.Feed(drained); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if err := col.FinishEOF(); err != nil {
		t.Fatalf("FinishEOF: %v", err)
	}
	if len(col.capsules) != 1 {
		t.Fatalf("expected 1 capsule, got %v", col.capsules)
	}

	j, err := journal.ReadFile(col.capsules[0])
	if err != nil {
		t.Fatalf("journal.ReadFile(%s): %v", col.capsules[0], err)
	}
	if j.Header.Meta.Service != "orders-svc" {
		t.Fatalf("meta.Service = %q, want orders-svc", j.Header.Meta.Service)
	}
	if len(j.Records) != 5 { // env + input + clock + gateway + output
		t.Fatalf("records = %d, want 5: %+v", len(j.Records), j.Records)
	}
}

func TestCmdDevEndToEndSubprocess(t *testing.T) {
	if os.Getenv("KAVACH_DEV_HELPER") == "1" {
		// Helper subprocess path: publish valid KVRING02 frames to KAVACH_RING_PATH and exit.
		ringPath := os.Getenv("KAVACH_RING_PATH")
		var buf bytes.Buffer
		enc := recstream.NewEncoder(&buf)
		_ = enc.Open(recstream.Open{Service: "helper-svc", Start: journal.StartGenesis})
		_ = enc.Records([]journal.Record{
			{Type: journal.TypeInput, Source: "kafka:events", Data: []byte(`{"tx":"tx-1"}`)},
			{Type: journal.TypeOutput, Sink: "kafka:events_out", Data: []byte(`{"tx":"tx-1"}`)},
		}, true)
		_ = enc.Close()
		publishIntoRingFile(t, ringPath, buf.Bytes())
		return
	}

	outDir := filepath.Join(t.TempDir(), "capsules")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmdLine := "KAVACH_DEV_HELPER=1 " + exe + " -test.run=^TestCmdDevEndToEndSubprocess$"

	var out, errb bytes.Buffer
	u := newUI(&out, true)
	code := cmdDevWithContext(context.Background(), u, []string{
		"--cmd", cmdLine,
		"--out", outDir,
		"--service", "helper-svc",
		"--pii", "standard",
		"--json",
	}, &out, &errb)
	if code != exitPass {
		t.Fatalf("kavach dev exit %d\nstdout=%s\nstderr=%s", code, out.String(), errb.String())
	}

	var res devResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode dev json: %v\n%s", err, out.String())
	}
	if res.Steps != 1 || len(res.Capsules) != 1 {
		t.Fatalf("unexpected dev result: %+v", res)
	}
	j, err := journal.ReadFile(res.Capsules[0])
	if err != nil {
		t.Fatalf("journal.ReadFile(%s): %v", res.Capsules[0], err)
	}
	if j.Header.Meta.Service != "helper-svc" || len(j.Records) < 3 {
		t.Fatalf("unexpected journal: service=%q records=%d", j.Header.Meta.Service, len(j.Records))
	}
}

func publishIntoRingFile(t *testing.T, ringPath string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(ringPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open ring %s: %v", ringPath, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap ring: %v", err)
	}
	defer syscall.Munmap(mem)

	capVal := binary.LittleEndian.Uint64(mem[devOffCapacity:])
	writePtr := (*atomic.Uint64)(unsafe.Pointer(&mem[devOffWrite]))
	w := writePtr.Load()
	pos := w & (capVal - 1)
	ringData := mem[devRingHeader : devRingHeader+capVal]
	n := uint64(len(data))
	first := capVal - pos
	if first >= n {
		copy(ringData[pos:pos+n], data)
	} else {
		copy(ringData[pos:], data[:first])
		copy(ringData[:n-first], data[first:])
	}
	writePtr.Store(w + n)
}

func TestDevCollectorSynthesizesCrashMidStep(t *testing.T) {
	outDir := t.TempDir()
	var buf bytes.Buffer
	enc := recstream.NewEncoder(&buf)
	if err := enc.Open(recstream.Open{Service: "crash-svc", Start: journal.StartGenesis}); err != nil {
		t.Fatal(err)
	}
	// Input started without StepEnd or Close — simulates SIGKILL / panic exit mid-step.
	if err := enc.Record(journal.Record{Type: journal.TypeInput, Source: "http:POST /pay", Data: []byte(`{"id":"1"}`)}); err != nil {
		t.Fatal(err)
	}
	col := newDevStreamCollector("", outDir)
	if err := col.Feed(buf.Bytes()); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if err := col.FinishEOF(); err != nil {
		t.Fatalf("FinishEOF: %v", err)
	}
	if len(col.capsules) != 1 || !strings.Contains(filepath.Base(col.capsules[0]), "incident") {
		t.Fatalf("expected 1 incident capsule, got %v", col.capsules)
	}
	j, err := journal.ReadFile(col.capsules[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	last := j.Records[len(j.Records)-1]
	if last.Type != journal.TypeMarker || last.Kind != journal.MarkerCrash {
		t.Fatalf("expected synthesized crash marker, got %+v", last)
	}
}

func TestReplayAndDiffInvariantsFlagAndMultiFixture(t *testing.T) {
	host, work := storeContract(t)
	traffic := filepath.Join(work, "traffic.kavach")

	// Declarative anti_lobotomy rule that passes against the matching baseline store.
	invPass := filepath.Join(work, "pass.yaml")
	if err := os.WriteFile(invPass, []byte(`service: store
invariants:
  - name: replies_not_dropped
    kind: anti_lobotomy
    sinks: [reply]
    min_ratio_vs_baseline: 1.0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCLI(t, "replay", traffic, "--bin", host+" base", "--invariants", invPass, "--json")
	if code != exitPass {
		t.Fatalf("replay with passing invariants exit %d\nstdout=%s\nstderr=%s", code, out, errb)
	}

	// Declarative at_most_once_emit with duplicate key on sink "reply" (traffic emits OK twice).
	invFail := filepath.Join(work, "fail.yaml")
	if err := os.WriteFile(invFail, []byte(`service: store
invariants:
  - name: unique_reply
    kind: at_most_once_emit
    sink: reply
`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb = runCLI(t, "replay", traffic, "--bin", host+" base", "--invariants", invFail, "--json")
	if code != exitFail || !strings.Contains(out, "invariant_violated(unique_reply)") {
		t.Fatalf("expected invariant_violated(unique_reply), got exit %d\nstdout=%s\nstderr=%s", code, out, errb)
	}

	// Multi-fixture kavach diff with --variants 0 --regressions 0 --json.
	trafficCopy := filepath.Join(work, "traffic2.kavach")
	raw, err := os.ReadFile(traffic)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trafficCopy, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb = runCLI(t, "diff", traffic, trafficCopy,
		"--old", host+" base", "--new", host+" base",
		"--invariants", invPass, "--variants", "0", "--regressions", "0", "--json")
	if code != exitPass {
		t.Fatalf("multi-fixture diff exit %d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	var multi struct {
		Passed   bool             `json:"passed"`
		Fixtures []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal([]byte(out), &multi); err != nil {
		t.Fatalf("decode multi-fixture diff json: %v\n%s", err, out)
	}
	if !multi.Passed || len(multi.Fixtures) != 2 {
		t.Fatalf("unexpected multi-fixture result: %+v", multi)
	}
}

func TestCmdInitDoctorMapAutoTypeScript(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{
  "name": "l1-billing",
  "main": "dist/index.js",
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
const consumer = kafka.consumer({ groupId: "l1" });
const app = express();
app.post("/billing/events", handleEvent);

async function handleEvent(req: any, res: any) {
  const now = Date.now();
  const rnd = Math.random();
  await pool.query("SELECT id FROM charges");
  await redis.zadd("subscription_refreshed_v2", now, "sub");
  await producer.send({ topic: "events_charged_in_advance", messages: [{ value: String(rnd) }] });
  res.json({ ok: true });
}
`)

	// 1. kavach doctor --dir <dir> --json
	code, out, errb := runCLI(t, "doctor", "--dir", dir, "--json")
	if code != exitPass {
		t.Fatalf("doctor TS exit %d: %s", code, errb)
	}
	var rep onboard.DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("decode doctor TS json: %v\n%s", err, out)
	}
	if rep.Language != onboard.LangTypeScript || len(rep.ClockCalls) == 0 || len(rep.RandCalls) == 0 {
		t.Fatalf("unexpected TS doctor report: lang=%q clock=%d rand=%d", rep.Language, len(rep.ClockCalls), len(rep.RandCalls))
	}

	// 2. kavach init --dir <dir> --json
	code, out, errb = runCLI(t, "init", "--dir", dir, "--json")
	if code != exitPass {
		t.Fatalf("init TS exit %d: %s", code, errb)
	}
	var initRes onboard.InitResult
	if err := json.Unmarshal([]byte(out), &initRes); err != nil {
		t.Fatalf("decode init TS json: %v\n%s", err, out)
	}
	if initRes.Language != onboard.LangTypeScript || initRes.MapNodes == 0 {
		t.Fatalf("unexpected TS init result: %+v", initRes)
	}

	// 3. kavach map --auto --dir <dir>
	t.Setenv("KAVACH_NO_OPEN", "1")
	code, _, errb = runCLI(t, "map", "--auto", "--open", "--dir", dir)
	if code != exitPass {
		t.Fatalf("map --auto TS exit %d: %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kavach", "map.html")); err != nil {
		t.Fatalf("expected TS .kavach/map.html: %v", err)
	}
}
