// Command ledger is Kavach's demo service: a single-writer wallet ledger that
// folds a stream of JSON events into balances.
//
//	go run ./examples/ledger -in examples/ledger/testdata/events.jsonl
//
// One upstream event has "amount": null, which crashes the handler. The flight
// recorder writes a fixture before the process dies.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/kavachlabs/kavach"
)

func main() {
	// Lets the kavach CLI use this binary to replay fixtures.
	kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })

	in := flag.String("in", "", "JSON-lines file of wallet events")
	dir := flag.String("fixtures", "fixtures", "directory for crash fixtures")
	flag.Parse()
	if *in == "" {
		log.Fatal("ledger: -in is required")
	}
	f, err := os.Open(*in)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	rec := kavach.NewRecorder(NewLedger(), kavach.Options{
		Service: "ledger",
		Dir:     *dir,
		Deliver: func(outs []kavach.Output) error {
			for _, o := range outs {
				fmt.Printf("%-18s %s\n", o.Sink, o.Data)
			}
			return nil
		},
		OnFlush: func(path string, err error) {
			if err != nil {
				log.Printf("kavach: could not write fixture: %v", err)
				return
			}
			log.Printf("kavach: wrote fixture %s", path)
		},
	})

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		in := kavach.Input{Source: "file:" + filepath.Base(*in), Position: strconv.Itoa(line), Data: sc.Bytes()}
		if err := rec.Step(in); err != nil {
			log.Printf("ledger: %v", err)
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
}
