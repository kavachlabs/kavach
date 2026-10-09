package envfacts

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnvSecrets(t *testing.T) {
	facts := Env([]string{
		"MAX_TRANSFER=1000",
		"api_key=abc",
		"DB_DSN=postgres://x",
		"SERVICE_URL=http://x",
		"MY_PRIVATE_THING=1",
		"OAUTH=1",
		"GITHUB_TOKEN=t",
		"DB_PASSWD=p",
		"CUSTOM=hide-me",
		"EMPTY=",
		"MAX_TRANSFER=ignored duplicate",
		"=bad",
		"novalue",
	}, []string{"CUSTOM"})
	got := map[string]journal.Fact{}
	for _, f := range facts {
		got[f.Key] = f
	}
	if len(facts) != len(got) || len(facts) != 10 {
		t.Fatalf("facts = %v", facts)
	}
	if f := got["env.MAX_TRANSFER"]; f.Form != journal.FactValue || string(f.Value) != "1000" {
		t.Fatalf("MAX_TRANSFER = %+v", f)
	}
	if f := got["env.EMPTY"]; f.Form != journal.FactValue || len(f.Value) != 0 {
		t.Fatalf("EMPTY = %+v", f)
	}
	for _, name := range []string{"api_key", "DB_DSN", "SERVICE_URL", "MY_PRIVATE_THING", "OAUTH", "GITHUB_TOKEN", "DB_PASSWD", "CUSTOM"} {
		f := got["env."+name]
		if f.Form != journal.FactSHA256 || len(f.Value) != sha256.Size {
			t.Errorf("%s is not hashed: %+v", name, f)
		}
	}
	sum := sha256.Sum256([]byte("hide-me"))
	if !reflect.DeepEqual(got["env.CUSTOM"].Value, sum[:]) {
		t.Fatal("CUSTOM hash differs")
	}
	for i := 1; i < len(facts); i++ {
		if facts[i-1].Key >= facts[i].Key {
			t.Fatal("facts are not sorted")
		}
	}
}

func TestParseMountinfo(t *testing.T) {
	ms := ParseMountinfo(fixture(t, "mountinfo"))
	got := map[string]string{}
	for _, m := range ms {
		got[m.Path] = m.FSType + " " + m.Options
	}
	want := map[string]string{
		"/sys":            "sysfs rw,nosuid,nodev,noexec,relatime",
		"/proc":           "proc rw,nosuid,nodev,noexec,relatime",
		"/":               "ext4 rw,relatime",
		"/dev/shm":        "tmpfs rw,nosuid,nodev",
		"/mnt/with space": "tmpfs rw,relatime",
		"/tmp":            "tmpfs ro,nosuid,nodev", // the later mount is on top
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounts = %v", got)
	}
}

func TestParseCgroup(t *testing.T) {
	if p, ok := CgroupPath(fixture(t, "cgroup-v2"), ""); !ok || p != "/system.slice/app.service" {
		t.Fatalf("v2 path = %q %v", p, ok)
	}
	v1 := fixture(t, "cgroup-v1")
	if p, ok := CgroupPath(v1, "memory"); !ok || p != "/docker/abc123" {
		t.Fatalf("memory path = %q %v", p, ok)
	}
	if p, ok := CgroupPath(v1, "cpu"); !ok || p != "/docker/abc123" {
		t.Fatalf("cpu path = %q %v", p, ok)
	}
	if _, ok := CgroupPath(v1, "pids"); ok {
		t.Fatal("found a pids cgroup")
	}
	for in, want := range map[string]int{"200000 100000": 2, "150000 100000": 2, "50000 100000": 1} {
		if n, ok := ParseCPUMax(in); !ok || n != want {
			t.Errorf("ParseCPUMax(%q) = %d %v", in, n, ok)
		}
	}
	for _, in := range []string{"max 100000", "-1 100000", "1", ""} {
		if _, ok := ParseCPUMax(in); ok {
			t.Errorf("ParseCPUMax(%q) found a limit", in)
		}
	}
	if n, ok := ParseCgroupLimit("2147483648"); !ok || n != 2<<30 {
		t.Fatalf("limit = %d %v", n, ok)
	}
	for _, in := range []string{"max", "9223372036854771712", ""} {
		if _, ok := ParseCgroupLimit(in); ok {
			t.Errorf("ParseCgroupLimit(%q) found a limit", in)
		}
	}
	if n := ParseMemTotal("MemTotal:       16318564 kB\nMemFree: 1 kB\n"); n != 16318564*1024 {
		t.Fatalf("MemTotal = %d", n)
	}
}

func TestParseLogind(t *testing.T) {
	got := ParseLogindConf(fixture(t, "logind.conf"))
	want := map[string]string{"KillUserProcesses": "no", "RemoveIPC": "yes", "KillOnlyUsers": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conf = %v", got)
	}
	if v, ok := ParseBusctlBool("b true\n"); !ok || v != "yes" {
		t.Fatalf("busctl = %q %v", v, ok)
	}
	if _, ok := ParseBusctlBool("s \"x\""); ok {
		t.Fatal("parsed a string as a bool")
	}
	if n := ParseUserSessions(fixture(t, "user-1000")); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	if img := ParseContainerenvImage(fixture(t, "containerenv")); img != "docker.io/library/alpine:latest" {
		t.Fatalf("image = %q", img)
	}
	if n := PasswdName("root:x:0:0::/root:/bin/sh\nalice:x:1000:1000::/home/alice:/bin/sh\n", 1000); n != "alice" {
		t.Fatalf("passwd name = %q", n)
	}
}

// writeTree creates the files of a fake root, keyed by absolute path.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hostMap(c *Collector) map[string]string {
	m := map[string]string{}
	for _, f := range c.Host() {
		m[f.Key] = string(f.Value)
	}
	return m
}

func TestCollectLinux(t *testing.T) {
	root := writeTree(t, map[string]string{
		"/proc/self/mountinfo":                               fixture(t, "mountinfo"),
		"/proc/self/cgroup":                                  fixture(t, "cgroup-v2"),
		"/proc/meminfo":                                      "MemTotal: 16318564 kB\n",
		"/proc/sys/kernel/osrelease":                         "6.8.0-45-generic\n",
		"/proc/sys/kernel/shmmax":                            "18446744073692774399\n",
		"/proc/sys/fs/file-max":                              "9223372036854775807\n",
		"/proc/sys/vm/overcommit_memory":                     "0\n",
		"/sys/fs/cgroup/cgroup.controllers":                  "cpu memory\n",
		"/sys/fs/cgroup/system.slice/app.service/cpu.max":    "200000 100000\n",
		"/sys/fs/cgroup/system.slice/app.service/memory.max": "2147483648\n",
		"/sys/fs/cgroup/system.slice/memory.max":             "max\n",
		"/etc/passwd":                                        "wfinfra:x:1000:1000::/home/wfinfra:/bin/bash\n",
		"/etc/timezone":                                      "Europe/Berlin\n",
		"/usr/lib/systemd/logind.conf":                       "[Login]\nKillUserProcesses=no\nRemoveIPC=no\n",
		"/etc/systemd/logind.conf.d/10-ipc.conf":             "[Login]\nRemoveIPC=yes\n",
		"/usr/lib/systemd/logind.conf.d/20-kill.conf":        "[Login]\nKillUserProcesses=true\n",
		"/run/systemd/system/.keep":                          "",
		"/run/systemd/users/1000":                            fixture(t, "user-1000"),
		"/run/.containerenv":                                 fixture(t, "containerenv"),
	})
	c := &Collector{
		Root: root, OS: "linux",
		Run:      func(string, ...string) ([]byte, error) { return nil, errors.New("not found") },
		Environ:  func() []string { return []string{"LANG=de_DE.UTF-8"} },
		Hostname: func() (string, error) { return "web-1", nil },
		UID:      func() int { return 1000 },
	}
	got := hostMap(c)
	want := map[string]string{
		"host.os":                          "linux",
		"host.arch":                        runtime.GOARCH,
		"host.kernel":                      "6.8.0-45-generic",
		"host.hostname":                    "web-1",
		"host.uid":                         "1000",
		"host.user":                        "wfinfra",
		"host.memory_limit":                "2147483648",
		"host.tz":                          "Europe/Berlin",
		"host.locale":                      "de_DE.UTF-8",
		"host.mount./dev/shm":              "tmpfs rw,nosuid,nodev",
		"host.mount./tmp":                  "tmpfs ro,nosuid,nodev",
		"host.sysctl.kernel.shmmax":        "18446744073692774399",
		"host.sysctl.fs.file-max":          "9223372036854775807",
		"host.sysctl.vm.overcommit_memory": "0",
		"host.systemd.RemoveIPC":           "yes",
		"host.systemd.KillUserProcesses":   "yes",
		"host.sessions.wfinfra":            "2",
		"host.container.image":             "docker.io/library/alpine:latest",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if cpus := got["host.cpus"]; cpus == "" || (runtime.NumCPU() > 2 && cpus != "2") {
		t.Errorf("host.cpus = %q", cpus)
	}
	if _, ok := got["host.runtime"]; ok {
		t.Error("collector must not report host.runtime")
	}
	for k := range got {
		if !strings.HasPrefix(k, "host.") {
			t.Errorf("key %q is not a host fact", k)
		}
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if got["host.ulimit.nofile"] == "" {
			t.Error("no host.ulimit.nofile")
		}
	}
}

func TestLogindFromManager(t *testing.T) {
	root := writeTree(t, map[string]string{"/run/systemd/system/.keep": ""})
	c := &Collector{Root: root, OS: "linux",
		Run: func(name string, args ...string) ([]byte, error) {
			if name == "busctl" && args[len(args)-1] == "RemoveIPC" {
				return []byte("b false\n"), nil
			}
			return nil, errors.New("no")
		},
		Environ: func() []string { return nil },
	}
	got := hostMap(c)
	if got["host.systemd.RemoveIPC"] != "no" {
		t.Errorf("RemoveIPC = %q", got["host.systemd.RemoveIPC"])
	}
	// Nothing configured and the manager cannot be asked: systemd's defaults.
	if got["host.systemd.KillUserProcesses"] != "no" {
		t.Errorf("KillUserProcesses = %q", got["host.systemd.KillUserProcesses"])
	}
}

func TestWatcher(t *testing.T) {
	host := "a"
	c := &Collector{Root: t.TempDir(), OS: "other",
		Environ:  func() []string { return nil },
		Hostname: func() (string, error) { return host, nil },
	}
	w := NewWatcher(c, 0, c.Host())
	w.Poll()
	if got := w.Take(); got != nil {
		t.Fatalf("changes without a change: %v", got)
	}
	host = "b"
	w.Poll()
	got := w.Take()
	if len(got) != 1 || got[0].Key != "host.hostname" || string(got[0].Value) != "b" {
		t.Fatalf("changes = %v", got)
	}
	if w.Take() != nil {
		t.Fatal("Take did not forget")
	}
	host = ""
	w.Poll()
	got = w.Take()
	if len(got) != 1 || got[0].Form != journal.FactUnset {
		t.Fatalf("removal = %v", got)
	}
}
