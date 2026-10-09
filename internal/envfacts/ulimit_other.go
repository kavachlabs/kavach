//go:build !linux && !darwin

package envfacts

type limit struct{ name, value string }

// limits returns nothing: resource limits are collected on Linux and macOS.
func limits() []limit { return nil }
