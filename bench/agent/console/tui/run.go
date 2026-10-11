package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// Answering reports whether a bench API serves base.
func Answering(base string) bool {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(base + "/api/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Run shows the console for the API at base until the user quits. owned
// says this process serves that API. keep reports that the user chose to
// leave the server running for runs that are still going.
func Run(base string, owned bool) (keep bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, base, owned, settingsPath())
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		return false, err
	}
	return m.keep, nil
}

// Wait keeps the process, and so the API it serves, alive until the runs
// it started have ended or the user presses Ctrl-C.
func Wait(base string, w io.Writer) {
	fmt.Fprintf(w, "bench: still serving on %s while the runs go on.\n", base)
	fmt.Fprintf(w, "bench: reconnect with `bench` in another terminal; Ctrl-C here stops the server (the runs go on, but can no longer be stopped).\n")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(base)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		jobs, err := c.jobs(ctx)
		if err != nil {
			continue
		}
		going := false
		for _, j := range jobs {
			going = going || (j.Stoppable && j.Status == harbor.JobRunning)
		}
		if !going {
			fmt.Fprintln(w, "bench: the runs have ended; stopping the server.")
			return
		}
	}
}
