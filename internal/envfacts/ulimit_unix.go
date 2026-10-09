//go:build linux || darwin

package envfacts

import (
	"strconv"
	"syscall"
)

type limit struct{ name, value string }

// limits returns the soft resource limits of the process (getrlimit(2)).
func limits() []limit {
	table := append([]struct {
		name string
		res  int
	}{
		{"nofile", syscall.RLIMIT_NOFILE},
		{"cpu", syscall.RLIMIT_CPU},
		{"data", syscall.RLIMIT_DATA},
		{"fsize", syscall.RLIMIT_FSIZE},
		{"stack", syscall.RLIMIT_STACK},
		{"core", syscall.RLIMIT_CORE},
		{"as", syscall.RLIMIT_AS},
	}, extraLimits...)
	var out []limit
	for _, l := range table {
		var r syscall.Rlimit
		if err := syscall.Getrlimit(l.res, &r); err != nil {
			continue
		}
		v := "unlimited"
		if uint64(r.Cur) < 1<<63-1 {
			v = strconv.FormatUint(uint64(r.Cur), 10)
		}
		out = append(out, limit{l.name, v})
	}
	return out
}
