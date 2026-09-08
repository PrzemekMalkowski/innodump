// color.go: ANSI terminal colour for this tool's own status output (not the
// generated SQL/TSV files, which are never colored) - styled after the
// author's gcache-inspector (github.com/PrzemekMalkowski/gcache-inspector):
// a bold banner line, dim labels, and a small set of semantic colour
// helpers used by name (ok/warn/bad/id/...) rather than raw ANSI codes
// scattered through the printing code.
package main

import "os"

// colorEnabled is set once at startup (initColor, called from run()). We
// honour NO_COLOR (https://no-color.org) and skip colour when stdout isn't
// a real terminal (piped/redirected output stays byte-identical either
// way) - isTTY itself lives in progress.go, shared with the progress bar's
// own terminal check.
var colorEnabled bool

func initColor() {
	if os.Getenv("NO_COLOR") != "" {
		colorEnabled = false
		return
	}
	colorEnabled = isTTY(os.Stdout)
}

// ANSI SGR escape sequences.
const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiCyan    = "\033[36m"
	ansiBoldRed = "\033[1;31m"
)

func col(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// Semantic colour helpers - use these in printing code, not raw ANSI codes.

// hi renders a section header / banner line (bold).
func hi(s string) string { return col(ansiBold, s) }

// id renders an identifier: schema.table name, file/directory path (bold cyan).
func id(s string) string { return col(ansiBold+ansiCyan, s) }

// dim renders a secondary label (field prefixes like "Table:", "Columns:",
// or a --help flag's description text - see printUsage in main.go).
func dim(s string) string { return col(ansiDim, s) }

// val renders a plain data value against its dim() label in --verbose
// output (e.g. "Space ID:" in dim, its "8" in val) - cyan, not bold, so it
// reads as distinct from id's bold cyan (reserved for identifiers: schema.
// table names, file/directory paths, --help flag names).
func val(s string) string { return col(ansiCyan, s) }

// good renders a successful count (rows written, files processed) - green.
func good(s string) string { return col(ansiGreen, s) }

// warn renders a skipped-but-recovered count, or a caution note - yellow.
func warn(s string) string { return col(ansiYellow, s) }

// bad renders an error count or failure - bold red.
func bad(s string) string { return col(ansiBoldRed, s) }
