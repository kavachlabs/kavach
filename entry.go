package kavach

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ReplayCommand is the first argument that makes MaybeReplay take over a binary.
const ReplayCommand = "kavach-replay"

// MaybeReplay lets a service binary act as its own replayer for the kavach CLI.
// Call it first thing in main:
//
//	func main() {
//		kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })
//		...
//	}
//
// When the binary is run as `<bin> kavach-replay <fixture> <out.json>`, it
// replays the fixture against a fresh handler, writes the Result as JSON to
// out.json ("-" for stdout), and exits: 0 if the replay ran (whatever its
// status), 3 if the fixture could not be replayed. Otherwise it returns.
func MaybeReplay(newHandler func() Handler) {
	if len(os.Args) < 2 || os.Args[1] != ReplayCommand {
		return
	}
	os.Exit(replayMain(os.Args[2:], newHandler, os.Stdout, os.Stderr))
}

func replayMain(args []string, newHandler func() Handler, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintf(stderr, "usage: %s %s <fixture> <out.json|->\n", os.Args[0], ReplayCommand)
		return 2
	}
	res, err := ReplayFile(args[0], newHandler)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	b = append(b, '\n')
	if args[1] == "-" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(args[1], b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	return 0
}
