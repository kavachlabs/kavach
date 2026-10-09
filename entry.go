package kavach

import (
	"fmt"
	"os"
)

// MaybeReplay lets a service binary act as a host for the kavach CLI
// (SPEC.md §9). Call it first thing in main:
//
//	func main() {
//		kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })
//		...
//	}
//
// When the last argument is "kavach-host", it serves the host protocol on
// standard input and output against fresh handlers from newHandler, and exits:
// 0 after a session that ended with `end`, 1 otherwise. Standard output is
// taken for the protocol; whatever the handler prints goes to standard error.
// Otherwise it returns.
func MaybeReplay(newHandler func() Handler) {
	if n := len(os.Args); n < 2 || os.Args[n-1] != HostCommand {
		return
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	if err := ServeHost(newHandler, os.Stdin, out); err != nil {
		fmt.Fprintln(os.Stderr, "kavach:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
