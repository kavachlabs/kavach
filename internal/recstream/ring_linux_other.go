//go:build linux && !amd64 && !arm64

package recstream

// No memfd_create number here: such platforms use the temporary file.
const sysMemfdCreate = 0
