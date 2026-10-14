package main

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSourceReads checks every srcFile mode (O_DIRECT where the test
// directory's filesystem allows it, the drop-from-cache fallback, and
// plain reads) against the file's real contents, over unaligned random
// reads, sequential small reads, and reads running past the end.
func TestSourceReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src")
	r := rand.New(rand.NewSource(1))
	want := make([]byte, 3<<20+1234) // deliberately not a multiple of the alignment
	r.Read(want)
	if err := os.WriteFile(path, want, 0644); err != nil {
		t.Fatal(err)
	}
	defer func() { ioDirect = false }()

	for _, mode := range []string{"direct", "fallback", "plain"} {
		ioDirect = mode != "plain"
		f, err := openSource(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "direct" && !f.direct {
			t.Logf("%s's filesystem refuses O_DIRECT: the direct mode runs as the fallback", dir)
		}
		if mode == "fallback" { // as openDirect sets it up when O_DIRECT is refused
			f.Close()
			plain, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			f = &srcFile{f: plain, drop: true}
		}
		for i := 0; i < 3000; i++ {
			off := int64(r.Intn(len(want) + 100))
			n := r.Intn(70000) + 1
			if i%3 == 0 { // runs of small sequential reads, as MyISAM's scan does
				off, n = int64(i*37)%int64(len(want)), r.Intn(64)+1
			}
			got := make([]byte, n)
			k, err := f.ReadAt(got, off)
			end := off + int64(n)
			if end > int64(len(want)) {
				end = int64(len(want))
			}
			exp := []byte{}
			if off < int64(len(want)) {
				exp = want[off:end]
			}
			if k != len(exp) || !bytes.Equal(got[:k], exp) {
				t.Fatalf("%s: ReadAt(%d bytes at %d) = %d bytes, %v; want %d bytes", mode, n, off, k, err, len(exp))
			}
			if k < n && err != io.EOF {
				t.Fatalf("%s: short read at %d without io.EOF: %v", mode, off, err)
			}
		}
		all, err := io.ReadAll(f) // sequential Read from the start
		if err != nil || !bytes.Equal(all, want) {
			t.Fatalf("%s: sequential read: %d bytes, %v", mode, len(all), err)
		}
		f.Close()
	}
}

// TestThrottle checks --io-limit's token bucket holds a rate.
func TestThrottle(t *testing.T) {
	th := newThrottle(10 << 20) // 10MB/s
	start := time.Now()
	for i := 0; i < 50; i++ {
		th.wait(64 << 10) // 3.2MB in all
	}
	// 3.2MB at 10MB/s is 320ms, less the 100ms burst allowance.
	if el := time.Since(start); el < 200*time.Millisecond || el > 600*time.Millisecond {
		t.Fatalf("3.2MB at 10MB/s took %v", el)
	}
}

func TestParseRate(t *testing.T) {
	for in, want := range map[string]float64{"50M": 50 << 20, "1.5G": 1.5 * (1 << 30), "512K/s": 512 << 10, "100MB/s": 100 << 20, "1048576": 1 << 20} {
		if got, err := parseRate(in); err != nil || got != want {
			t.Errorf("parseRate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "fast", "-5M", "10K"} {
		if _, err := parseRate(bad); err == nil {
			t.Errorf("parseRate(%q) accepted", bad)
		}
	}
}
