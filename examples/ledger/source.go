package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/kavachlabs/kavach"
)

// step runs one event through the recorder. A panic stops the service (the
// recorder has written a fixture by then); handler errors and invariant
// failures are logged, captured as fixtures, and the event is skipped.
func step(rec *kavach.Recorder, in kavach.Input) error {
	err := rec.Step(in)
	var pe *kavach.PanicError
	if errors.As(err, &pe) {
		return err
	}
	if err != nil {
		log.Printf("ledger: %s @ %s: %v", in.Source, in.Position, err)
	}
	return nil
}

// consumeFile folds a JSON-lines file of events.
func consumeFile(path string, rec *kavach.Recorder) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		in := kavach.Input{Source: "file:" + filepath.Base(path), Position: strconv.Itoa(line), Data: sc.Bytes()}
		if err := step(rec, in); err != nil {
			return err
		}
	}
	return sc.Err()
}

// kafkaClient returns a client that reads topic from its first offset.
//
// The topic is the ledger's journal: the service keeps no other state, so on
// every start it folds the topic from the beginning. That is also why it does
// not use a consumer group or commit offsets. Use a single-partition topic: the
// ledger is a single writer, and only one partition has a total order.
func kafkaClient(brokers []string, topic string, opts ...kgo.Opt) (*kgo.Client, error) {
	return kgo.NewClient(append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	}, opts...)...)
}

// consumeKafka folds records from cl until ctx is cancelled or a step panics.
func consumeKafka(ctx context.Context, cl *kgo.Client, rec *kavach.Recorder) error {
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			e := errs[0]
			return fmt.Errorf("kafka: fetching %s[%d]: %w", e.Topic, e.Partition, e.Err)
		}
		for iter := fetches.RecordIter(); !iter.Done(); {
			r := iter.Next()
			in := kavach.Input{
				Source:   "kafka:" + r.Topic,
				Position: fmt.Sprintf("%d:%d", r.Partition, r.Offset),
				Data:     r.Value,
			}
			if err := step(rec, in); err != nil {
				return err
			}
		}
	}
}

// seedKafka produces every non-empty line of path to topic, in order.
func seedKafka(ctx context.Context, brokers []string, topic, path string) (int, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		kgo.AllowAutoTopicCreation(),
		kgo.MaxBufferedRecords(1), // keep file order without relying on batching
	)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		value := append([]byte(nil), sc.Bytes()...)
		if err := cl.ProduceSync(ctx, &kgo.Record{Value: value}).FirstErr(); err != nil {
			return n, fmt.Errorf("kafka: producing line %d: %w", n+1, err)
		}
		n++
	}
	return n, sc.Err()
}
