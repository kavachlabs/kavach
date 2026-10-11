package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/tui"
)

// cmdConsole opens the terminal console. It connects to the API on the
// port if one answers there; otherwise it serves the API itself, with the
// same flags as bench api, for as long as the console is open.
func cmdConsole(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	base := "http://127.0.0.1:" + strconv.Itoa(o.port)
	owned := !tui.Answering(base)
	if owned {
		cfg, err := o.config()
		if err != nil {
			fmt.Fprintf(stderr, "bench: %v\n", err)
			return 2
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			fmt.Fprintf(stderr, "bench: port %d is taken by something that isn't the bench API; pass --port\n", o.port)
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "bench: %v\n", err)
			return 1
		}
		srv := &http.Server{Handler: api.New(cfg)}
		go srv.Serve(ln)
		defer srv.Close()
	}
	keep, err := tui.Run(base, owned)
	if err != nil {
		fmt.Fprintf(stderr, "bench: %v\n", err)
		return 1
	}
	if keep {
		tui.Wait(base, stderr)
	}
	return 0
}
