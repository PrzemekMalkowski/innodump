// progress.go: a stderr progress indicator for long-running extractions.
// Enabled only when stderr is a real terminal (piping/redirecting stays
// byte-identical either way) and only shown once a run has taken long
// enough to be worth the flicker - most files finish well before that.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// progEnabled is set once at startup, after flag parsing.
var progEnabled bool

func initProgress(noProgress bool) {
	if noProgress || os.Getenv("NO_PROGRESS") != "" {
		progEnabled = false
		return
	}
	progEnabled = isTTY(os.Stderr)
}

// isTTY reports whether f is a real terminal using the TIOCGWINSZ ioctl,
// which is available on Linux and macOS and requires no external package.
func isTTY(f *os.File) bool {
	var ws [4]uint16 // struct winsize: ws_row, ws_col, ws_xpixel, ws_ypixel
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
	return errno == 0
}

const (
	progDelay = 150 * time.Millisecond // hides the flicker on fast runs
	progTick  = 100 * time.Millisecond // redraw interval
	progWidth = 24                     // bar width in cells
)

var spinFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// prog is a spinner, optionally with a percentage bar (when total is known)
// and a secondary "extra" counter (this tool uses it for the row count). A
// prog that was never activated (not a TTY, or --no-progress) is still safe
// to use - every method is a no-op, so callers need no conditionals.
type prog struct {
	label string
	total int64 // 0 = unknown -> spinner only, no bar/percentage
	cur   atomic.Int64
	extra atomic.Int64
	start time.Time
	stop  chan struct{}
	done  chan struct{}
	on    bool
}

// newProg starts a progress indicator. total is the expected final value
// for set() (e.g. the tablespace's page count, used as a rough proxy for
// how far through the file the walk has gotten); pass 0 if unknown.
func newProg(label string, total int64) *prog {
	p := &prog{label: label, total: total, start: time.Now()}
	if !progEnabled {
		return p
	}
	p.on = true
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go p.run()
	return p
}

// set reports current progress toward total (e.g. the current page number).
func (p *prog) set(n int64) {
	if p.on {
		p.cur.Store(n)
	}
}

// setExtra reports a secondary counter shown alongside the bar (row count).
func (p *prog) setExtra(n int64) {
	if p.on {
		p.extra.Store(n)
	}
}

// finish stops the drawing goroutine and wipes the line. Safe to call twice
// (defer it and also call it explicitly before printing the final summary).
func (p *prog) finish() {
	if !p.on {
		return
	}
	p.on = false
	close(p.stop)
	<-p.done
	fmt.Fprint(os.Stderr, "\r\033[2K")
}

func (p *prog) run() {
	defer close(p.done)
	select { // stay silent if the work finishes almost immediately
	case <-p.stop:
		return
	case <-time.After(progDelay):
	}
	t := time.NewTicker(progTick)
	defer t.Stop()
	for i := 0; ; i++ {
		p.draw(spinFrames[i%len(spinFrames)])
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

func (p *prog) draw(spin string) {
	elapsed := fmt.Sprintf("%4.1fs", time.Since(p.start).Seconds())
	rows := p.extra.Load()
	if p.total <= 0 {
		fmt.Fprintf(os.Stderr, "\r\033[2K%s %s  %d row(s)  %s", spin, p.label, rows, elapsed)
		return
	}
	cur := p.cur.Load()
	if cur > p.total {
		cur = p.total
	}
	frac := float64(cur) / float64(p.total)
	filled := int(frac*progWidth + 0.5)
	if filled > progWidth {
		filled = progWidth
	}
	fmt.Fprintf(os.Stderr, "\r\033[2K%s %s [%s%s] %3.0f%%  %d row(s)  %s",
		spin, p.label,
		strings.Repeat("█", filled), strings.Repeat("░", progWidth-filled),
		frac*100, rows, elapsed)
}
