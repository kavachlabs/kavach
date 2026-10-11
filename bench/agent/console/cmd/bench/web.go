package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
)

// openBrowser opens url in the user's browser; tests replace it.
var openBrowser = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}

// cmdWeb opens the web console. A bench server already on the port (another
// `bench web`, or the `bench api` a terminal console uses) is reused, so every
// console sees the same runs; otherwise it serves one itself.
func cmdWeb(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(o.port)) + "/"
	if answers(url + "api/status") {
		fmt.Fprintf(stdout, "bench web: using the server already on %s\n", url)
		launch(url, stdout, stderr)
		return 0
	}
	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "bench web: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "bench: jobs %s, tasks %s\n", cfg.JobsDir, cfg.TasksDir)
	err = serve(api.New(cfg), o.port, stderr, func(url string) { launch(url, stdout, stderr) })
	if err != nil {
		fmt.Fprintf(stderr, "bench web: %v\n", err)
		return 1
	}
	return 0
}

// answers reports whether a bench server answers at statusURL. The first
// status call runs docker and harbor, which can take seconds.
func answers(statusURL string) bool {
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(statusURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func launch(url string, stdout, stderr io.Writer) {
	fmt.Fprintf(stdout, "bench web: open %s\n", url)
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(stderr, "bench web: opening a browser: %v\n", err)
	}
}
