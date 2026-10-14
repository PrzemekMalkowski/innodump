//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

// openDirect opens path with O_DIRECT, or - if its filesystem refuses
// that (EINVAL) - normally, for cacheFallback.
func openDirect(path string) (*os.File, int, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err == nil {
		return f, cacheODirect, nil
	}
	if !errors.Is(err, syscall.EINVAL) {
		return nil, 0, err
	}
	f, err = os.Open(path)
	return f, cacheFallback, err
}

const posixFadvDontneed = 4

// dropCache asks the kernel to drop [off, off+n) of f from the page cache
// (posix_fadvise(POSIX_FADV_DONTNEED)); best effort.
func dropCache(f *os.File, off, n int64) {
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(n), posixFadvDontneed, 0, 0)
}

// noCacheOutput: nothing to set up on Linux - outFile drops behind itself.
func noCacheOutput(*os.File) {}
