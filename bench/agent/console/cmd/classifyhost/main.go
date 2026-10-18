// Command classifyhost serves the trial error classifier as a Kavach host:
//
//	classifyhost kavach-host                            # driven by the kavach CLI
//	classifyhost -record inputs.jsonl -fixtures DIR     # record the lines as inputs
//
// Each line of the recorded file is a classifyhost.Request. A line that breaks
// the invariant ends the run and the recorder writes a fixture.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kavachlabs/kavach/bench/agent/console/classifyhost"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func main() {
	kavach.MaybeReplay(func() kavach.Handler { return &classifyhost.Classifier{} })

	in := flag.String("record", "", "JSON-lines file of requests to record")
	dir := flag.String("fixtures", "", "directory for the journal; fixtures go in its fixtures/ subdirectory")
	flag.Parse()
	if *in == "" {
		log.Fatal("classifyhost: pass -record FILE, or run as a kavach host")
	}
	f, err := os.Open(*in)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	rec := kavach.NewRecorder(&classifyhost.Classifier{}, kavach.Options{
		Service: "classifyhost",
		Dir:     *dir,
		Deliver: func(outs []kavach.Output) error {
			for _, o := range outs {
				fmt.Printf("%s: %s\n", o.Sink, o.Data)
			}
			return nil
		},
	})
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for n := 1; sc.Scan(); n++ {
		if err := rec.Step(kavach.Input{Source: "file:" + *in, Position: fmt.Sprint(n), Data: append([]byte(nil), sc.Bytes()...)}); err != nil {
			fmt.Fprintln(os.Stderr, "classifyhost:", err)
			break
		}
	}
	rec.Close()
}
