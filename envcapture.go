package kavach

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// CaptureEnv records the process environment and host facts. Variable values
// are stored verbatim, secrets included; run `kavach scrub` before sharing a
// fixture.
func CaptureEnv() *journal.Env {
	e := &journal.Env{Vars: map[string]string{}}
	for _, kv := range os.Environ() {
		if name, val, ok := strings.Cut(kv, "="); ok && name != "" {
			e.Vars[name] = val
		}
	}
	host, _ := os.Hostname()
	e.System = &journal.System{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Kernel:     kernelVersion(),
		CPUs:       runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Timezone:   time.Local.String(),
		Hostname:   host,
	}
	e.Build = &journal.Build{GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		e.Build.Module = bi.Main.Path
	}
	e.Build.Revision = buildRevision()
	return e
}

func kernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// envPlumbing are variables describing the replaying process rather than the
// recorded one; they are never re-applied.
var envPlumbing = map[string]bool{
	"PATH": true, "HOME": true, "PWD": true, "OLDPWD": true, "USER": true, "LOGNAME": true,
	"SHELL": true, "TMPDIR": true, "TERM": true, "HOSTNAME": true, "SHLVL": true, "_": true,
}

// ReplayEnv returns the environment to run a replay binary in: base, with the
// recorded variables applied over it except process plumbing and KAVACH_*.
// Variables that were not set at recording time are left as they are.
func ReplayEnv(base []string, rec *journal.Env) []string {
	if rec == nil {
		return base
	}
	out := make([]string, 0, len(base)+len(rec.Vars))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := rec.Vars[name]; ok && applicable(name) {
			continue
		}
		out = append(out, kv)
	}
	for name, val := range rec.Vars {
		if applicable(name) {
			out = append(out, name+"="+val)
		}
	}
	return out
}

func applicable(name string) bool {
	return !envPlumbing[name] && !strings.HasPrefix(name, "KAVACH_")
}

// EnvDrift lists the differences between a recorded environment and the current
// one, as human-readable lines. Values are never printed, since they may be
// secrets: only names and the facts that matter for reproduction.
func EnvDrift(rec *journal.Env, cur *journal.Env) []string {
	if rec == nil || cur == nil {
		return nil
	}
	var d []string
	if a, b := rec.System, cur.System; a != nil && b != nil {
		if a.OS != b.OS || a.Arch != b.Arch {
			d = append(d, "platform "+a.OS+"/"+a.Arch+" recorded, "+b.OS+"/"+b.Arch+" now")
		}
		if a.GOMAXPROCS != b.GOMAXPROCS {
			d = append(d, "GOMAXPROCS "+strconv.Itoa(a.GOMAXPROCS)+" recorded, "+strconv.Itoa(b.GOMAXPROCS)+" now")
		}
		if a.Timezone != b.Timezone {
			d = append(d, "timezone "+a.Timezone+" recorded, "+b.Timezone+" now")
		}
	}
	if a, b := rec.Build, cur.Build; a != nil && b != nil && a.GoVersion != b.GoVersion {
		d = append(d, "Go "+a.GoVersion+" recorded, "+b.GoVersion+" now")
	}
	for name, val := range rec.Vars {
		if !applicable(name) {
			continue
		}
		if now, ok := cur.Vars[name]; !ok {
			d = append(d, "env "+name+" was set, is unset")
		} else if now != val {
			d = append(d, "env "+name+" differs")
		}
	}
	return d
}
