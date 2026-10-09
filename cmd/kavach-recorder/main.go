// Command kavach-recorder is the flight recorder's second half (SPEC.md §10).
// An SDK starts it as a child process and writes the record stream into its
// standard input; it numbers the records, batches them into compressed blocks
// and writes journal segments and fixtures, and answers on standard output with
// a JSON Lines control stream.
//
//	kavach-recorder [--test-facts facts.json]
//	kavach-recorder facts [--test-facts facts.json] [--secret-key NAME]...
//	kavach-recorder version
//
// The facts command prints the env. and host. facts a genesis environment
// would hold, as one JSON object from key to fact, for SDK hosts that fill
// `ready.environment` (SPEC.md §9.2).
//
// Files are written under the open object's dir: segments as
// <dir>/<service>-<run>-<segment>.kavach and fixtures as
// <dir>/fixtures/<service>-<run>-<seq>.kavach, where seq is that of the
// failing step's input.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/internal/recorder"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "facts":
			return runFacts(args[1:], stdout, stderr)
		case "version", "--version", "-version":
			fmt.Fprintln(stdout, recorder.Name)
			return 0
		}
	}

	fs := flag.NewFlagSet("kavach-recorder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	testFacts := fs.String("test-facts", "", "take env. and host. facts, the run and recorded_at from this JSON file, and do not watch the host (for conformance tests)")
	watch := fs.Duration("watch-interval", recorder.DefaultWatchInterval, "how often to poll host facts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "kavach-recorder: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	cfg := recorder.Config{In: stdin, Out: stdout, Log: stderr, WatchInterval: *watch, Ring: os.NewFile(3, "ring")}
	if *testFacts != "" {
		tf, err := loadTestFacts(*testFacts)
		if err != nil {
			fmt.Fprintf(stderr, "kavach-recorder: %v\n", err)
			return 2
		}
		cfg.Test = tf
	}

	// A signal sent to the service's process group must not stop the recorder
	// before it has read the pipe to the end (SPEC.md §10.1).
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	detach()
	return recorder.Run(cfg)
}

func loadTestFacts(path string) (*recorder.TestFacts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tf recorder.TestFacts
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := envfacts.FromJSON(tf.Facts); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &tf, nil
}

func runFacts(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kavach-recorder facts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	testFacts := fs.String("test-facts", "", "print the facts of this test-facts file instead of collecting them")
	var secrets stringList
	fs.Var(&secrets, "secret-key", "name of an environment variable to hash besides the ones the spec requires (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var out []byte
	if *testFacts != "" {
		tf, err := loadTestFacts(*testFacts)
		if err != nil {
			fmt.Fprintf(stderr, "kavach-recorder: %v\n", err)
			return 2
		}
		facts, _ := envfacts.FromJSON(tf.Facts)
		out, _ = envfacts.MarshalFacts(facts)
	} else {
		var err error
		if out, err = envfacts.MarshalFacts(envfacts.New().Facts(secrets)); err != nil {
			fmt.Fprintf(stderr, "kavach-recorder: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}
