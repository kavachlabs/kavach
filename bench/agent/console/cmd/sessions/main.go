// Command sessions runs the trial's sessions (api.SessionService) as a Kavach
// service: a trial server, driven by visitors' requests and restarts.
//
//	sessions -fixtures /tmp/sessions api/testdata/kavach/sessions/traffic.jsonl
//
// feeds each line of the traffic file (an api.SessionInput) to the service
// through the flight recorder (kavach-recorder, found through
// $KAVACH_RECORDER or PATH), which writes the journal under -fixtures. With
// "kavach-host" as its last argument it serves the kavach CLI instead, from
// the console's directory:
//
//	kavach contract check --dir api/testdata/kavach/sessions/contract --old ./sessions --new ./sessions
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func main() {
	// The trial server runs a visitor's Harbor through this executable.
	if len(os.Args) > 1 && os.Args[1] == harbor.LogPipeCommand {
		os.Exit(harbor.RunLogPipe(os.Args[2:]))
	}
	tasks := flag.String("tasks", "harbor/testdata/tasks", "the tasks directory, with the hello-world tasks")
	dir := flag.String("fixtures", "", "directory for the journal; fixtures go in its fixtures/ subdirectory (default kavach)")
	flag.Parse()
	kavach.MaybeReplay(func() kavach.Handler { return &api.SessionService{Tasks: *tasks} })

	if flag.NArg() != 1 {
		log.Fatal("usage: sessions [-fixtures DIR] traffic.jsonl")
	}
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	svc := &api.SessionService{Tasks: *tasks}
	defer svc.Close()
	rec := kavach.NewRecorder(svc, kavach.Options{
		Service:   "sessions",
		Dir:       *dir,
		OnFixture: func(file, failure string) { fmt.Printf("fixture %s\n  %s\n", file, failure) },
	})
	defer rec.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		n++
		if err := rec.Step(kavach.Input{Source: "trial", Position: fmt.Sprint(n), Data: append([]byte(nil), sc.Bytes()...)}); err != nil {
			var inv *kavach.InvariantError
			if errors.As(err, &inv) {
				fmt.Printf("input %d: %v\n", n, err)
				return
			}
			log.Fatalf("input %d: %v", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d inputs\n", n)
}
