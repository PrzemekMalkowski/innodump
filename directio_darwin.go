//go:build darwin

package main

import (
	"os"
	"syscall"
)

// openDirect opens path and turns its caching off (F_NOCACHE: macOS has
// no O_DIRECT, and F_NOCACHE has no alignment rules).
func openDirect(path string) (*os.File, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 1)
	return f, cacheNoCache, nil
}

// dropCache is a no-op: F_NOCACHE files don't fill the cache to begin with.
func dropCache(*os.File, int64, int64) {}

func noCacheOutput(f *os.File) {
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 1)
}
