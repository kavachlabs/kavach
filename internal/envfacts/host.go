package envfacts

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// sysctlKeys are the kernel parameters recorded on Linux: the ones that bound
// what a process can use (SPEC.md §4.8) and a few that commonly decide how it
// fails.
var sysctlKeys = []string{
	"kernel.shmmax", "kernel.shmall", "kernel.shmmni", "kernel.pid_max",
	"fs.file-max", "fs.nr_open", "vm.overcommit_memory", "vm.max_map_count",
}

// Host returns the `host.` facts of the machine, sorted by key.
func (c *Collector) Host() []journal.Fact {
	h := &host{c: c}
	h.collect()
	journal.SortFacts(h.facts)
	return h.facts
}

type host struct {
	c     *Collector
	facts []journal.Fact
}

func (h *host) add(key, value string) {
	h.facts = append(h.facts, journal.Fact{Key: "host." + key, Form: journal.FactValue, Value: []byte(value)})
}

// read returns the trimmed contents of the file at path under the root.
func (c *Collector) read(path string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(c.Root, path))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func (c *Collector) exists(path string) bool {
	_, err := os.Stat(filepath.Join(c.Root, path))
	return err == nil
}

func (c *Collector) run(name string, args ...string) (string, bool) {
	if c.Run == nil {
		return "", false
	}
	out, err := c.Run(name, args...)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func (h *host) collect() {
	c := h.c
	linux := c.OS == "linux"
	h.add("os", c.OS)
	h.add("arch", runtime.GOARCH)

	if linux {
		if v, ok := c.read("/proc/sys/kernel/osrelease"); ok {
			h.add("kernel", v)
		}
	} else if v, ok := c.run("uname", "-r"); ok {
		h.add("kernel", v)
	}
	if c.Hostname != nil {
		if v, err := c.Hostname(); err == nil && v != "" {
			h.add("hostname", v)
		}
	}
	uid := -1
	if c.UID != nil {
		uid = c.UID()
	}
	name := ""
	if uid >= 0 {
		h.add("uid", strconv.Itoa(uid))
		name = c.userName(uid)
		if name != "" {
			h.add("user", name)
		}
	}

	cpus := runtime.NumCPU()
	var memLimit uint64
	if linux {
		if q, ok := c.cgroupCPUs(); ok && q < cpus {
			cpus = q
		}
		memLimit = c.memoryLimit()
	} else if v, ok := c.run("sysctl", "-n", "hw.memsize"); ok {
		memLimit, _ = strconv.ParseUint(v, 10, 64)
	}
	h.add("cpus", strconv.Itoa(cpus))
	if memLimit > 0 {
		h.add("memory_limit", strconv.FormatUint(memLimit, 10))
	}

	h.add("tz", c.timeZone())
	if v := firstNonEmpty(c.getenv("LC_ALL"), c.getenv("LANG")); v != "" {
		h.add("locale", v)
	}
	for _, l := range limits() {
		h.add("ulimit."+l.name, l.value)
	}

	if !linux {
		return
	}
	if data, ok := c.read("/proc/self/mountinfo"); ok {
		for _, m := range ParseMountinfo(data) {
			h.add("mount."+m.Path, m.FSType+" "+m.Options)
		}
	}
	for _, key := range sysctlKeys {
		if v, ok := c.read("/proc/sys/" + strings.ReplaceAll(key, ".", "/")); ok {
			h.add("sysctl."+key, strings.Join(strings.Fields(v), " "))
		}
	}
	if c.exists("/run/systemd/system") {
		for _, s := range []string{"RemoveIPC", "KillUserProcesses"} {
			if v, ok := c.logindSetting(s); ok {
				h.add("systemd."+s, v)
			}
		}
		if name != "" {
			if n, ok := c.sessions(uid); ok {
				h.add("sessions."+name, strconv.Itoa(n))
			}
		}
	}
	if v := c.containerImage(); v != "" {
		h.add("container.image", v)
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// userName finds the name of uid in the passwd file, then in the OS's user
// database, then in $USER.
func (c *Collector) userName(uid int) string {
	if data, ok := c.read("/etc/passwd"); ok {
		if n := PasswdName(data, uid); n != "" {
			return n
		}
	}
	if c.Root == "/" {
		if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
			return u.Username
		}
	}
	return c.getenv("USER")
}

func (c *Collector) timeZone() string {
	if v := c.getenv("TZ"); v != "" {
		return strings.TrimPrefix(v, ":")
	}
	if v, ok := c.read("/etc/timezone"); ok && v != "" {
		return v
	}
	if link, err := os.Readlink(filepath.Join(c.Root, "/etc/localtime")); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			return zone
		}
	}
	name, _ := time.Now().Zone()
	return name
}

// memoryLimit is the memory available to the process in bytes: the tightest
// cgroup limit, or the machine's memory when there is none.
func (c *Collector) memoryLimit() uint64 {
	var limit uint64
	if m, ok := c.read("/proc/meminfo"); ok {
		limit = ParseMemTotal(m)
	}
	if l, ok := c.cgroupMemory(); ok && (limit == 0 || l < limit) {
		limit = l
	}
	return limit
}

// cgroupDirs lists, from the process's own cgroup up to the root, the
// directories of the cgroup file system that apply to controller ("" for the
// unified v2 hierarchy). A container often sees its own cgroup as the root.
func (c *Collector) cgroupDirs(controller string) []string {
	data, ok := c.read("/proc/self/cgroup")
	if !ok {
		return nil
	}
	path, ok := CgroupPath(data, controller)
	if !ok {
		return nil
	}
	base := "/sys/fs/cgroup"
	if controller != "" {
		base += "/" + controller
	} else if !c.exists(base + "/cgroup.controllers") {
		return nil
	}
	var dirs []string
	for p := filepath.Clean("/" + path); ; p = filepath.Dir(p) {
		dirs = append(dirs, filepath.Join(base, p))
		if p == "/" {
			break
		}
	}
	return dirs
}

func (c *Collector) cgroupMemory() (uint64, bool) {
	var best uint64
	for _, ctl := range []string{"", "memory"} {
		for _, dir := range c.cgroupDirs(ctl) {
			for _, f := range []string{"memory.max", "memory.limit_in_bytes"} {
				if v, ok := c.read(dir + "/" + f); ok {
					if n, ok := ParseCgroupLimit(v); ok && (best == 0 || n < best) {
						best = n
					}
				}
			}
		}
	}
	return best, best > 0
}

func (c *Collector) cgroupCPUs() (int, bool) {
	best := 0
	consider := func(n int, ok bool) {
		if ok && (best == 0 || n < best) {
			best = n
		}
	}
	for _, dir := range c.cgroupDirs("") {
		if v, ok := c.read(dir + "/cpu.max"); ok {
			consider(ParseCPUMax(v))
		}
	}
	for _, ctl := range []string{"cpu", "cpu,cpuacct"} {
		for _, dir := range c.cgroupDirs(ctl) {
			q, ok1 := c.read(dir + "/cpu.cfs_quota_us")
			p, ok2 := c.read(dir + "/cpu.cfs_period_us")
			if ok1 && ok2 {
				consider(ParseCPUMax(q + " " + p))
			}
		}
	}
	return best, best > 0
}

// logindSetting returns "yes" or "no" for a setting of systemd-logind, from
// the running manager when it can be asked, else from the configuration files.
func (c *Collector) logindSetting(name string) (string, bool) {
	if out, ok := c.run("busctl", "get-property", "org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager", name); ok {
		if v, ok := ParseBusctlBool(out); ok {
			return v, true
		}
	}
	if out, ok := c.run("loginctl", "show", "--property="+name); ok {
		if _, val, found := strings.Cut(out, "="); found {
			if v, ok := parseBool(val); ok {
				return v, true
			}
		}
	}
	// The main file, then drop-ins in file name order; a drop-in of the same
	// name in a later directory replaces an earlier one.
	value, found := "", false
	apply := func(content string) {
		if v, ok := ParseLogindConf(content)[name]; ok {
			value, found = v, true
		}
	}
	for _, f := range []string{"/usr/lib/systemd/logind.conf", "/etc/systemd/logind.conf"} {
		if s, ok := c.read(f); ok {
			apply(s)
		}
	}
	dropIns := map[string]string{}
	for _, dir := range []string{"/usr/lib/systemd/logind.conf.d", "/run/systemd/logind.conf.d", "/etc/systemd/logind.conf.d"} {
		names, _ := filepath.Glob(filepath.Join(c.Root, dir, "*.conf"))
		for _, n := range names {
			dropIns[filepath.Base(n)] = n
		}
	}
	bases := make([]string, 0, len(dropIns))
	for b := range dropIns {
		bases = append(bases, b)
	}
	sort.Strings(bases)
	for _, b := range bases {
		if data, err := os.ReadFile(dropIns[b]); err == nil {
			apply(string(data))
		}
	}
	if found {
		return value, true
	}
	// Built-in defaults of systemd-logind.
	switch name {
	case "RemoveIPC":
		return "yes", true
	case "KillUserProcesses":
		return "no", true
	}
	return "", false
}

// sessions counts the login sessions of uid that systemd-logind keeps.
func (c *Collector) sessions(uid int) (int, bool) {
	if !c.exists("/run/systemd/users") {
		return 0, false
	}
	data, ok := c.read("/run/systemd/users/" + strconv.Itoa(uid))
	if !ok {
		return 0, true // no state file: logind knows of no sessions of the user
	}
	return ParseUserSessions(data), true
}

// containerImage finds the image the process runs from, where the container
// runtime says so: Podman writes it to /run/.containerenv, and an operator can
// pass it in KAVACH_CONTAINER_IMAGE.
func (c *Collector) containerImage() string {
	if v := c.getenv("KAVACH_CONTAINER_IMAGE"); v != "" {
		return v
	}
	if data, ok := c.read("/run/.containerenv"); ok {
		return ParseContainerenvImage(data)
	}
	return ""
}
