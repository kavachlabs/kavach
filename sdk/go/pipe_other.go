//go:build !linux

package kavach

import "os"

func setPipeSize(*os.File) {}
