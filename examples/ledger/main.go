// Command ledger is Kavach's demo service: a single-writer wallet ledger that
// folds a stream of JSON events into balances. It reads events from a file or
// from a Kafka topic:
//
//	ledger -in testdata/events.jsonl
//	ledger -kafka localhost:9092 -seed testdata/events.jsonl   # load the topic once
//	ledger -kafka localhost:9092
//
// One upstream event has "amount": null, which crashes the handler. The flight
// recorder (kavach-recorder, found through $KAVACH_RECORDER or PATH) writes a
// fixture before the process dies.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func main() {
	// Lets the kavach CLI use this binary to replay fixtures.
	kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })

	in := flag.String("in", "", "JSON-lines file of wallet events")
	brokers := flag.String("kafka", "", "comma-separated Kafka seed brokers; consume -topic instead of -in")
	topic := flag.String("topic", "wallet-events", "Kafka topic holding the ledger's events")
	seed := flag.String("seed", "", "produce this JSON-lines file to -topic and exit")
	dir := flag.String("fixtures", "", "directory for the journal; crash fixtures go in its fixtures/ subdirectory (default kavach)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *seed != "" {
		if *brokers == "" {
			log.Fatal("ledger: -seed needs -kafka")
		}
		n, err := seedKafka(ctx, strings.Split(*brokers, ","), *topic, *seed)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("ledger: produced %d events to %s", n, *topic)
		return
	}

	rec := kavach.NewRecorder(NewLedger(), kavach.Options{
		Service: "ledger",
		Dir:     *dir,
		Deliver: func(outs []kavach.Output) error {
			for _, o := range outs {
				fmt.Printf("%-18s %s\n", o.Sink, o.Data)
			}
			return nil
		},
	})

	var err error
	switch {
	case *brokers != "":
		cl, cerr := kafkaClient(strings.Split(*brokers, ","), *topic)
		if cerr != nil {
			log.Fatal(cerr)
		}
		defer cl.Close()
		log.Printf("ledger: folding kafka topic %s from the start", *topic)
		err = consumeKafka(ctx, cl, rec)
	case *in != "":
		err = consumeFile(*in, rec)
	default:
		log.Fatal("ledger: pass -in FILE or -kafka BROKERS")
	}
	rec.Close()
	if err != nil {
		log.Fatal(err)
	}
}
