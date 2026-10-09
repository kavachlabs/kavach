package envfacts

import (
	"bufio"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	Path    string // mount point
	FSType  string
	Options string // per-mount options, e.g. "rw,nosuid,nodev"
}

// ParseMountinfo parses the contents of /proc/self/mountinfo (proc(5)). When
// several mounts share a mount point, the last, the one on top, is kept.
func ParseMountinfo(data string) []Mount {
	var out []Mount
	index := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		f := strings.Fields(sc.Text())
		sep := -1
		for i, w := range f {
			if w == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+1 >= len(f) {
			continue
		}
		m := Mount{Path: unescapeMount(f[4]), Options: f[5], FSType: f[sep+1]}
		if i, ok := index[m.Path]; ok {
			out[i] = m
			continue
		}
		index[m.Path] = len(out)
		out = append(out, m)
	}
	return out
}

// unescapeMount undoes the octal escapes the kernel uses for space, tab,
// newline and backslash in mountinfo paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// PasswdName returns the name of uid in the contents of /etc/passwd, or "".
func PasswdName(data string, uid int) string {
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(data, "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[2] == want && !strings.HasPrefix(line, "#") {
			return f[0]
		}
	}
	return ""
}

// ParseMemTotal returns MemTotal of /proc/meminfo in bytes, or 0.
func ParseMemTotal(data string) uint64 {
	for _, line := range strings.Split(data, "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) >= 1 {
				n, err := strconv.ParseUint(f[0], 10, 64)
				if err != nil {
					return 0
				}
				if len(f) > 1 && strings.EqualFold(f[1], "kB") {
					n *= 1024
				}
				return n
			}
		}
	}
	return 0
}

// CgroupPath finds the path of the process's cgroup in the contents of
// /proc/self/cgroup. With controller "" it returns the unified (v2) hierarchy's
// path; otherwise the path in the v1 hierarchy that has that controller.
func CgroupPath(data, controller string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		if controller == "" {
			if f[0] == "0" && f[1] == "" {
				return f[2], true
			}
			continue
		}
		if f[1] == controller {
			return f[2], true
		}
		for _, c := range strings.Split(f[1], ",") {
			if c == controller {
				return f[2], true
			}
		}
	}
	return "", false
}

// ParseCgroupLimit parses memory.max or memory.limit_in_bytes. A limit that is
// "max" or too large to be real is no limit.
func ParseCgroupLimit(v string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil || n == 0 || n >= 1<<62 {
		return 0, false
	}
	return n, true
}

// ParseCPUMax parses "<quota> <period>" (cpu.max of cgroup v2, or the v1 quota
// and period files joined) into a whole number of CPUs, rounded up. "max" and
// negative quotas are no limit.
func ParseCPUMax(v string) (int, bool) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return 0, false
	}
	quota, err1 := strconv.ParseInt(f[0], 10, 64)
	period, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0, false
	}
	return int((quota + period - 1) / period), true
}

// ParseBusctlBool parses `busctl get-property` output of a boolean ("b true").
func ParseBusctlBool(out string) (string, bool) {
	f := strings.Fields(out)
	if len(f) != 2 || f[0] != "b" {
		return "", false
	}
	return parseBool(f[1])
}

// parseBool normalizes a systemd boolean to "yes" or "no".
func parseBool(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "yes", "y", "true", "t", "on":
		return "yes", true
	case "0", "no", "n", "false", "f", "off":
		return "no", true
	}
	return "", false
}

// ParseLogindConf returns the settings in the [Login] section of a
// logind.conf file. Booleans are normalized to "yes" and "no"; a setting
// repeated in the file takes its last value.
func ParseLogindConf(data string) map[string]string {
	out := map[string]string{}
	inLogin := false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			inLogin = line == "[Login]"
			continue
		}
		if !inLogin {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if b, ok := parseBool(v); ok {
			v = b
		}
		out[k] = v
	}
	return out
}

// ParseUserSessions counts the sessions listed in a /run/systemd/users/<uid>
// state file.
func ParseUserSessions(data string) int {
	for _, line := range strings.Split(data, "\n") {
		if rest, ok := strings.CutPrefix(line, "SESSIONS="); ok {
			return len(strings.Fields(rest))
		}
	}
	return 0
}

// ParseContainerenvImage returns the image named by Podman's
// /run/.containerenv, or "".
func ParseContainerenvImage(data string) string {
	for _, line := range strings.Split(data, "\n") {
		if rest, ok := strings.CutPrefix(line, "image="); ok {
			return strings.Trim(rest, `"`)
		}
	}
	return ""
}
