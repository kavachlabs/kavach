//go:build !linux && !darwin

package envfacts

type limit struct{ name, value string }

func limits() []limit { return nil }
