package envfacts

// extraLimits are limits the syscall package has no constants for.
var extraLimits = []struct {
	name string
	res  int
}{
	{"nproc", 7},
	{"memlock", 6},
}
