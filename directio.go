// directio.go: how this tool touches the disk - --direct-io (bypass the
// page cache) and --io-limit (cap the bandwidth) - for extracting from a
// busy host, or copying a large datadir out for staging, without evicting
// the server's working set from the page cache or saturating its disks.
//
// Every source file (tablespaces, .MYD/.MYI, SST files, WALs, MANIFEST,
// binlogs, .frm/.sdi) is opened through openSource, and every output file
// through createOutput / writeOutputFile, so both options apply everywhere.
//
//   - Reads, with --direct-io: O_DIRECT on Linux (F_NOCACHE on macOS), so
//     source data never enters the page cache. O_DIRECT needs the offset,
//     length and buffer all aligned (to 4KB here, which covers every
//     common device), so each read is widened to aligned boundaries; a
//     small per-file window serves the next read when it falls inside the
//     last one (MyISAM's scan, for one, reads a few bytes at a time). A
//     filesystem that refuses O_DIRECT (tmpfs, some overlay filesystems)
//     gets ordinary reads followed by posix_fadvise(POSIX_FADV_DONTNEED).
//   - Writes, with --direct-io: output is plain buffered text of no fixed
//     shape, so rather than O_DIRECT it's flushed and dropped from the
//     page cache every few MB (fdatasync + POSIX_FADV_DONTNEED; F_NOCACHE
//     on macOS).
//   - --io-limit RATE: one token bucket, shared by every read and write
//     (bytes actually transferred, after alignment) - the total disk I/O
//     the run generates, whichever disks it lands on.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	directAlign    = 4096
	directWindow   = 1 << 20  // read-ahead window kept per file
	dropBehindStep = 16 << 20 // output: flush + drop from the cache every this many bytes
)

// How openDirect kept a source file out of the page cache.
const (
	cacheODirect  = iota // O_DIRECT: aligned reads only
	cacheNoCache         // F_NOCACHE (macOS): ordinary reads, never cached
	cacheFallback        // neither available: ordinary reads, dropped after
)

var (
	ioDirect   bool
	ioThrottle *throttle

	directFallbackOnce sync.Once
)

// initIO applies --direct-io and --io-limit.
func initIO(direct bool, limit string) error {
	ioDirect = direct
	if limit == "" || limit == "0" {
		return nil
	}
	rate, err := parseRate(limit)
	if err != nil {
		return fmt.Errorf("--io-limit: %w", err)
	}
	ioThrottle = newThrottle(rate)
	return nil
}

// parseRate reads a bytes-per-second rate: a number with an optional K, M
// or G suffix (powers of 1024), and an optional "/s" or "B/s".
func parseRate(s string) (float64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "/S")
	t = strings.TrimSuffix(t, "B")
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "K"):
		mult, t = 1<<10, strings.TrimSuffix(t, "K")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	case strings.HasSuffix(t, "G"):
		mult, t = 1<<30, strings.TrimSuffix(t, "G")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%q is not a rate like 50M (bytes per second; K, M and G suffixes allowed)", s)
	}
	if v*mult < 64<<10 {
		return 0, fmt.Errorf("%q is below the 64K/s minimum", s)
	}
	return v * mult, nil
}

// --- throttle ---

// throttle is a token bucket in bytes per second, with up to a tenth of a
// second's worth of burst; safe for concurrent use.
type throttle struct {
	mu    sync.Mutex
	rate  float64
	burst time.Duration
	next  time.Time // when the bucket is next empty
}

func newThrottle(rate float64) *throttle {
	return &throttle{rate: rate, burst: 100 * time.Millisecond}
}

// wait blocks until n more bytes may be transferred.
func (t *throttle) wait(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	now := time.Now()
	if t.next.Before(now.Add(-t.burst)) {
		t.next = now.Add(-t.burst)
	}
	t.next = t.next.Add(time.Duration(float64(n) / t.rate * float64(time.Second)))
	sleep := t.next.Sub(now)
	t.mu.Unlock()
	if sleep > 0 {
		time.Sleep(sleep)
	}
}

// --- source files ---

// srcFile is a read-only source file opened per --direct-io.
type srcFile struct {
	f      *os.File
	direct bool // opened with O_DIRECT: reads must be aligned
	drop   bool // read normally, then drop what was read from the page cache
	pos    int64

	mu      sync.Mutex
	winOff  int64
	win     []byte // the last aligned read (direct only)
	lastEnd int64  // where the previous read ended: reading on from there is sequential
}

// openSource opens a source file for reading.
func openSource(path string) (*srcFile, error) {
	if !ioDirect {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &srcFile{f: f}, nil
	}
	f, mode, err := openDirect(path)
	if err != nil {
		return nil, err
	}
	s := &srcFile{f: f, direct: mode == cacheODirect, drop: mode == cacheFallback}
	if mode == cacheFallback {
		directFallbackOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "%s --direct-io: %s's filesystem doesn't support O_DIRECT - reading normally and dropping what's read from the page cache instead\n",
				dim("note:"), path)
		})
	}
	return s, nil
}

func (s *srcFile) Close() error               { return s.f.Close() }
func (s *srcFile) Stat() (os.FileInfo, error) { return s.f.Stat() }
func (s *srcFile) Name() string               { return s.f.Name() }

// Read reads sequentially from the current position.
func (s *srcFile) Read(p []byte) (int, error) {
	n, err := s.ReadAt(p, s.pos)
	s.pos += int64(n)
	if n > 0 && err == io.EOF {
		err = nil
	}
	return n, err
}

// ReadAt is io.ReaderAt.
func (s *srcFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !s.direct {
		ioThrottle.wait(len(p))
		n, err := s.f.ReadAt(p, off)
		if s.drop && n > 0 {
			dropCache(s.f, off, int64(n))
		}
		return n, err
	}
	// Served from the last window?
	s.mu.Lock()
	if off >= s.winOff && off+int64(len(p)) <= s.winOff+int64(len(s.win)) {
		n := copy(p, s.win[off-s.winOff:])
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	start := off &^ (directAlign - 1)
	end := (off + int64(len(p)) + directAlign - 1) &^ (directAlign - 1)
	s.mu.Lock()
	sequential := off >= s.lastEnd-directAlign && off <= s.lastEnd+directAlign
	s.lastEnd = off + int64(len(p))
	s.mu.Unlock()
	if sequential && end-start < directWindow {
		end = start + directWindow // read ahead: the next small read is likely right after this one
	}
	buf := alignedBuf(int(end - start))
	ioThrottle.wait(len(buf))
	n, err := readFullAt(s.f, buf, start)
	if err != nil && err != io.EOF {
		return 0, err
	}
	buf = buf[:n]
	if off-start >= int64(len(buf)) {
		return 0, io.EOF
	}
	c := copy(p, buf[off-start:])
	s.mu.Lock()
	s.winOff, s.win = start, buf
	s.mu.Unlock()
	if c < len(p) {
		return c, io.EOF
	}
	return c, nil
}

// readFullAt reads until buf is full or the file ends (an O_DIRECT read
// can come back short only at end of file).
func readFullAt(f *os.File, buf []byte, off int64) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.ReadAt(buf[total:], off+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.EOF
		}
	}
	return total, nil
}

// alignedBuf returns a buffer whose start is aligned for O_DIRECT.
func alignedBuf(n int) []byte {
	raw := make([]byte, n+directAlign)
	shift := 0
	if r := int(uintptr(unsafe.Pointer(&raw[0])) & (directAlign - 1)); r != 0 {
		shift = directAlign - r
	}
	return raw[shift : shift+n : shift+n]
}

// readSourceFile is os.ReadFile through openSource.
func readSourceFile(path string) ([]byte, error) {
	if !ioDirect && ioThrottle == nil {
		return os.ReadFile(path)
	}
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, fi.Size())
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// --- output files ---

// outFile is an output file written per --direct-io/--io-limit.
type outFile struct {
	f       *os.File
	written int64
	dropped int64
}

// createOutput creates (truncates) an output file.
func createOutput(path string) (*outFile, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if ioDirect {
		noCacheOutput(f)
	}
	return &outFile{f: f}, nil
}

func (o *outFile) Write(p []byte) (int, error) {
	ioThrottle.wait(len(p))
	n, err := o.f.Write(p)
	o.written += int64(n)
	if ioDirect && o.written-o.dropped >= dropBehindStep {
		o.dropBehind()
	}
	return n, err
}

// dropBehind writes what's been written so far to disk and drops it from
// the page cache, so output never piles up as dirty pages.
func (o *outFile) dropBehind() {
	if o.written == o.dropped {
		return
	}
	if err := o.f.Sync(); err == nil {
		dropCache(o.f, 0, o.written)
		o.dropped = o.written
	}
}

func (o *outFile) Close() error {
	if ioDirect {
		o.dropBehind()
	}
	return o.f.Close()
}

// writeOutputFile is os.WriteFile through createOutput.
func writeOutputFile(path string, data []byte) error {
	if !ioDirect && ioThrottle == nil {
		return os.WriteFile(path, data, 0644)
	}
	o, err := createOutput(path)
	if err != nil {
		return err
	}
	if _, err := o.Write(data); err != nil {
		o.Close()
		return err
	}
	return o.Close()
}
