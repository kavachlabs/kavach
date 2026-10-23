package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
)

// trialRetry is bench trial retry: it asks the running trial server, over
// its admin socket, to retry attempts of an invited run.
func trialRetry(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench trial retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", defaultAdminSocket, "the server's -admin-socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprint(stderr, "usage: bench trial retry [-socket PATH] <run> <attempt>...\n")
		return 2
	}
	body, _ := json.Marshal(api.AdminRetry{Job: fs.Arg(0), Trials: fs.Args()[1:]})
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", *socket)
	}}}
	resp, err := c.Post("http://admin/retry", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "bench trial retry: %v (is bench api running with -admin-socket %s?)\n", err, *socket)
		return 1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		fmt.Fprintf(stderr, "bench trial retry: %s\n", e.Error)
		return 1
	}
	var out []api.AdminRetried
	if err := json.Unmarshal(b, &out); err != nil {
		fmt.Fprintf(stderr, "bench trial retry: %v\n", err)
		return 1
	}
	code := 0
	for _, r := range out {
		switch {
		case r.Error != "":
			fmt.Fprintf(stdout, "%s  failed: %s\n", r.Trial, r.Error)
			code = 1
		case r.Queued:
			fmt.Fprintf(stdout, "%s  queued: starts when the run ends\n", r.Trial)
		default:
			fmt.Fprintf(stdout, "%s  started\n", r.Trial)
		}
	}
	return code
}
