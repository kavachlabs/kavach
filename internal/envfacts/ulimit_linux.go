package envfacts

// extraLimits are limits the syscall package has no constants for. The numbers
// are those of Linux on amd64 and arm64.
var extraLimits = []struct {
	name string
	res  int
}{
	{"nproc", 6},
	{"memlock", 8},
}
