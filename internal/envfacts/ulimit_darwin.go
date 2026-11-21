package envfacts

var extraLimits = []struct {
	name string
	res  int
}{
	{"nproc", 7},
	{"memlock", 6},
}
