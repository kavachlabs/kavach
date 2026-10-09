package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

const topic = "wallet-events"

// fakeKafka starts an in-process Kafka cluster seeded with the demo events.
func fakeKafka(t *testing.T) []string {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := seedKafka(ctx, c.ListenAddrs(), topic, "testdata/events.jsonl")
	if err != nil || n != 9 {
		t.Fatalf("seeded %d events: %v", n, err)
	}
	return c.ListenAddrs()
}

func TestKafkaCrashFixture(t *testing.T) {
	brokers := fakeKafka(t)
	cl, err := kafkaClient(brokers, topic)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	dir := t.TempDir()
	rec := kavach.NewRecorder(NewLedger(), kavach.Options{Service: "ledger", Dir: dir, RecoverPanics: true})
	defer rec.Close()
	// The buggy build stops at the crash; the fixed one consumes until the
	// deadline, as a service would until shut down.
	timeout := 10 * time.Second
	if fixNullAmount {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = consumeKafka(ctx, cl, rec)

	if fixNullAmount {
		if err != nil {
			t.Fatalf("fixed build: %v", err)
		}
		if err := rec.Flush(true); err != nil {
			t.Fatal(err)
		}
		if res, _ := replay.RunFile(rec.File(), newHandler); res.Status != replay.StatusOK || len(res.Steps) != 9 {
			t.Fatalf("fixed build consumed %v", res)
		}
		return
	}
	var pe *kavach.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected the null amount to crash the ledger, got %v", err)
	}
	if err := rec.Flush(true); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "fixtures", "*.kavach"))
	if len(paths) != 1 {
		t.Fatalf("fixtures: %v", paths)
	}
	j, err := journal.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var positions []string
	for _, r := range j.Records {
		if r.Type == journal.TypeInput {
			if r.Source != "kafka:"+topic {
				t.Fatalf("input source = %q", r.Source)
			}
			positions = append(positions, r.Position)
		}
	}
	if got := strings.Join(positions, " "); got != "0:0 0:1 0:2 0:3 0:4 0:5 0:6 0:7" {
		t.Fatalf("input positions = %s", got)
	}

	// The fixture replays with no Kafka at all.
	res, err := replay.Run(j, newHandler)
	if err != nil || res.String() != "still_failing@32" {
		t.Fatalf("replay: %v, %v", res, err)
	}
}

// TestKafkaBinary runs the real ledger binary against the in-process cluster.
func TestKafkaBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	brokers := strings.Join(fakeKafka(t), ",")
	dir := t.TempDir()
	bin := filepath.Join(dir, "ledger")
	if b, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}

	fx := filepath.Join(dir, "journal")
	cmd := exec.Command(bin, "-kafka", brokers, "-topic", topic, "-fixtures", fx)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the ledger to crash:\n%s", out)
	}
	if !strings.Contains(string(out), "kavach: wrote fixture") {
		t.Fatalf("no fixture reported:\n%s", out)
	}
	paths, _ := filepath.Glob(filepath.Join(fx, "fixtures", "*.kavach"))
	if len(paths) != 1 {
		t.Fatalf("fixtures: %v", paths)
	}
	// The binary has the bug planted; this test's handler may be the fixed one.
	want := replay.StatusStillFailing
	if fixNullAmount {
		want = replay.StatusFixed
	}
	res, err := replay.RunFile(paths[0], newHandler)
	if err != nil || res.Status != want || res.Recorded == nil {
		t.Fatalf("replay: %v, %v", res, err)
	}

	// -seed needs -kafka.
	if out, err := exec.Command(bin, "-seed", "testdata/events.jsonl").CombinedOutput(); err == nil ||
		!strings.Contains(string(out), "-seed needs -kafka") {
		t.Fatalf("seed without kafka: %v\n%s", err, out)
	}
}
