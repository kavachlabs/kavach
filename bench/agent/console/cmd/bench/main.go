// Command bench is the console of Kavach's agent benchmark: it runs coding
// agents on benchmark tasks with Harbor and shows how they did.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// command is one bench subcommand.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

// commands are bench's subcommands. A frontend registers here; the one named
// "" runs when bench is given no subcommand.
var commands = []command{
	{"api", "serve the console's HTTP API on 127.0.0.1 (see API.md)", cmdAPI},
	{"web", "open the web console, serving the API unless a server already runs", cmdWeb},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if name == "help" {
		fmt.Fprint(stdout, usage())
		return 0
	}
	for _, c := range commands {
		if c.name == name {
			return c.run(args, stdout, stderr)
		}
	}
	if name != "" {
		fmt.Fprintf(stderr, "bench: unknown command %q\n\n", name)
	}
	fmt.Fprint(stderr, usage())
	return 2
}

func usage() string {
	var b strings.Builder
	b.WriteString("bench runs coding agents on Kavach's benchmark tasks with Harbor.\n\nUsage:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  bench %-6s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun bench <command> -h for its flags.\n")
	return b.String()
}
