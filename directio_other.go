//go:build !linux && !darwin

package main

import "os"

// No page-cache control here: --direct-io only throttles (--io-limit).
func openDirect(path string) (*os.File, int, error) {
	f, err := os.Open(path)
	return f, cacheFallback, err
}

func dropCache(*os.File, int64, int64) {}
func noCacheOutput(*os.File)           {}
