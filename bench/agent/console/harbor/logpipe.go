package harbor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// LogPipeCommand is the hidden bench subcommand that runs Harbor for a run
// with its own keys.
const LogPipeCommand = "_logpipe"

// redactVar names, in the helper's environment, the variables whose values
// are secrets. The secrets travel in the environment, never in argv, which
// any local user can read with ps.
const redactVar = "KAVACH_LOGPIPE_REDACT"

// RunLogPipe is `bench _logpipe -log FILE -home DIR -- harbor args...`. It
// runs Harbor with its own environment, writes Harbor's output to FILE with
// the secrets replaced by [redacted], and exits with Harbor's exit code. It
// forwards SIGINT and SIGTERM to Harbor alone, as Stop does, and once Harbor
// has exited writes what the redactor held back, scrubs the secrets from the
// files under -scrub DIR and removes -home DIR.
//
// It is a process of its own, started like any Harbor with a group of its
// own, so a console that exits or restarts leaves it, and the Harbor under
// it, running; Stop reaches Harbor through it by the pid recorded for the
// run.
func RunLogPipe(args []string) int {
	var logFile, home, scrub string
	for len(args) > 0 && args[0] != "--" {
		if len(args) < 2 {
			return 2
		}
		switch args[0] {
		case "-log":
			logFile = args[1]
		case "-home":
			home = args[1]
		case "-scrub":
			scrub = args[1]
		default:
			return 2
		}
		args = args[2:]
	}
	if logFile == "" || len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: _logpipe -log FILE [-home DIR] [-scrub DIR] -- harbor args...")
		return 2
	}
	cmdline := args[1:]
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Close()

	var secrets []string
	for _, name := range strings.Split(os.Getenv(redactVar), ",") {
		if v := os.Getenv(name); name != "" && v != "" {
			secrets = append(secrets, v)
		}
	}
	redact := newRedactWriter(log, secrets)
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, redactVar+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Stdout, cmd.Stderr = redact, redact
	// Harbor's children may keep the pipe open after Harbor exits.
	cmd.WaitDelay = 2 * time.Second
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	code := 0
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(redact, "harbor: %v\n", err)
		code = 127
	} else {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
	wait:
		for {
			select {
			case s := <-sigs:
				cmd.Process.Signal(s)
			case err := <-done:
				code = exitCode(err)
				break wait
			}
		}
	}
	redact.Close()
	if scrub != "" {
		// Harbor and its agent wrote files into the job's folder; a provider's
		// error in a trajectory may quote a key.
		if err := ScrubTree(scrub, secrets); err != nil {
			fmt.Fprintf(log, "\nharbor: scrubbing %s: %v\n", scrub, err)
		}
	}
	if home != "" {
		os.RemoveAll(home)
	}
	return code
}

// exitCode is err as a shell reports it: a process killed by a signal exits
// 128 plus the signal.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		if c := ee.ExitCode(); c > 0 {
			return c
		}
	}
	return 1
}
