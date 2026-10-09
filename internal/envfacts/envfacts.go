// Package envfacts collects the facts an environment record holds (SPEC.md
// §4.8): the process's environment variables and what can be learned about the
// host, OS and kernel it runs on.
//
// Collection is best effort. A fact the platform lacks, or that cannot be read,
// is left out; nothing here fails. Everything that touches the machine goes
// through a Collector, whose file system root and command runner can be
// replaced, so that parsing is tested against fixtures on any platform.
package envfacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// secretWords are the substrings that mark an environment variable as a secret
// (SPEC.md §4.8), compared case-insensitively.
var secretWords = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE", "AUTH", "DSN", "_URL"}

// IsSecret reports whether the environment variable called name must be
// recorded as a hash: its name contains a secret word, or it is in extra.
func IsSecret(name string, extra []string) bool {
	up := strings.ToUpper(name)
	for _, w := range secretWords {
		if strings.Contains(up, w) {
			return true
		}
	}
	for _, e := range extra {
		if e == name {
			return true
		}
	}
	return false
}

// Env returns an `env.` fact for every variable in environ ("NAME=value"
// strings, as os.Environ returns them), sorted by key. Variables whose names
// are secret, or are listed in secretKeys, are recorded as SHA-256 hashes.
func Env(environ []string, secretKeys []string) []journal.Fact {
	facts := make([]journal.Fact, 0, len(environ))
	seen := make(map[string]bool, len(environ))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		f := journal.Fact{Key: "env." + name, Form: journal.FactValue, Value: []byte(value)}
		if IsSecret(name, secretKeys) {
			sum := sha256.Sum256([]byte(value))
			f.Form, f.Value = journal.FactSHA256, sum[:]
		}
		facts = append(facts, f)
	}
	journal.SortFacts(facts)
	return facts
}

// Runner runs a command and returns its standard output.
type Runner func(name string, args ...string) ([]byte, error)

// ExecRunner runs commands with os/exec, giving up on each after a few seconds.
func ExecRunner(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Collector reads host facts. Use New to get one for the running machine.
type Collector struct {
	// Root is prepended to the absolute paths read from the file system, such
	// as /proc and /etc. It defaults to "/".
	Root string
	// OS names the platform whose collectors run. It defaults to runtime.GOOS.
	OS string
	// Run runs external commands (busctl, loginctl, sysctl).
	Run Runner
	// Environ returns the process's environment variables.
	Environ func() []string
	// Hostname returns the host name.
	Hostname func() (string, error)
	// UID returns the user id of the process.
	UID func() int
}

// New returns a Collector for the machine it runs on.
func New() *Collector {
	return &Collector{Root: "/", OS: runtime.GOOS, Run: ExecRunner, Environ: os.Environ, Hostname: os.Hostname, UID: os.Getuid}
}

func (c *Collector) getenv(name string) string {
	prefix := name + "="
	for _, kv := range c.Environ() {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}

// Facts returns the `env.` facts and the `host.` facts, sorted by key. It does
// not include host.runtime, which the SDK sends (SPEC.md §4.8).
func (c *Collector) Facts(secretKeys []string) []journal.Fact {
	facts := append(Env(c.Environ(), secretKeys), c.Host()...)
	journal.SortFacts(facts)
	return facts
}
