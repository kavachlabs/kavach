//go:build unix

package main

import "syscall"

// detach starts a session of its own, so that signals to the service's process
// group, and its terminal, do not reach the recorder. It fails harmlessly when
// the recorder is already a session leader.
func detach() { syscall.Setsid() }
