// Command benchsvc is one benchmark bug as a standalone service, so that the
// kavach CLI can replay it like any user's binary. Which bug and which build
// (buggy, fixed or narrow) are chosen at link time:
//
//	go build -ldflags "-X main.bug=nil-email -X main.mode=fixed" ./bench/cmd/benchsvc
//
// `benchsvc record <dir>` feeds the bug's events to the flight recorder until
// the last one fails, and prints the path of the fixture it wrote.
package main

import (
	"fmt"
	"os"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/bench"
)

var bug, mode = "", "buggy"

func main() {
	var b *bench.Bug
	for _, c := range bench.Bugs() {
		if c.Name == bug {
			b = &c
		}
	}
	m := map[string]bench.Mode{"buggy": bench.Buggy, "fixed": bench.Fixed, "narrow": bench.Narrow}[mode]
	if b == nil {
		fmt.Fprintf(os.Stderr, "benchsvc: unknown bug %q (set with -ldflags \"-X main.bug=NAME\")\n", bug)
		os.Exit(2)
	}
	kavach.MaybeReplay(func() kavach.Handler { return b.New(m) })

	if len(os.Args) != 3 || os.Args[1] != "record" {
		fmt.Fprintln(os.Stderr, "usage: benchsvc record <dir>")
		os.Exit(2)
	}
	path, err := bench.RecordWith(*b, os.Args[2], os.Getenv("BENCH_NOSCRUB") != "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchsvc:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}
