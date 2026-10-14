// examine.go: --examine, a read-only health check of a datadir or file.
//
// It goes through exactly what an extraction would - every table found,
// every row decoded as if for output - but writes nothing, and always
// carries on past damage (as --skip-corrupted does) so that everything
// wrong gets counted, not just the first problem. Each table gets one line
// as it's checked; the run ends with a summary per engine and a list of
// every table that isn't clean, and exits non-zero if there's any.
package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

var examineMode bool

// examineEntry is one table's (or one unreadable file's) outcome.
type examineEntry struct {
	Name      string // schema.table, or a file path for a file nothing could be read from
	Engine    string
	Detail    string
	Rows      int
	RowErrs   int
	Corrupt   int
	Truncated bool
	Problem   string // the first problem met, for the summary
	Failed    error  // the table (or file) couldn't be examined at all
}

func (e *examineEntry) clean() bool {
	return e.Failed == nil && e.RowErrs == 0 && e.Corrupt == 0 && !e.Truncated
}

var examined []*examineEntry

// examineTable checks one table: walks its rows exactly as an extraction
// would (rendering every value), counting what it finds.
func examineTable(t *Table, makeWalker func(outCols []*Column, format outputFormat) RowWalker) *examineEntry {
	e := &examineEntry{Name: t.SchemaRef + "." + t.Name, Engine: t.Engine, Detail: examineDetail(t)}
	examined = append(examined, e)
	outCols := OutputColumns(t)
	if len(outCols) == 0 {
		e.Failed = fmt.Errorf("no columns this tool can output")
		printExamineLine(e)
		return e
	}
	rw := makeWalker(outCols, formatSQL)
	pr := newProg("examining "+e.Name, rw.ProgressTotal)
	err := rw.Walk(func(cp CorruptPage) {
		if cp.Info {
			pr.Warnf("%s %s: %s: %s\n", warn("warning:"), e.Name, t.corruptLabel(cp), cp.Reason)
			return
		}
		e.Corrupt++
		if cp.Truncated {
			e.Truncated = true
		}
		if e.Problem == "" {
			e.Problem = fmt.Sprintf("%s: %s", t.corruptLabel(cp), cp.Reason)
		}
		if *verbose {
			pr.Warnf("%s %s: %s: %s\n", warn("warning:"), e.Name, t.corruptLabel(cp), cp.Reason)
		}
	}, func(roe RowOrError) bool {
		pr.set(roe.ProgressBase + int64(roe.PageNo))
		if roe.Err != nil {
			e.RowErrs++
			if e.Problem == "" {
				e.Problem = fmt.Sprintf("%s: %v", t.rowLabel(roe), roe.Err)
			}
			if *verbose {
				pr.Warnf("%s %s: %s: %v\n", warn("warning:"), e.Name, t.rowLabel(roe), roe.Err)
			}
		} else {
			e.Rows++
			pr.setExtra(int64(e.Rows))
		}
		return *limitRows == 0 || e.Rows+e.RowErrs < *limitRows
	})
	pr.finish()
	if err != nil {
		e.Failed = err
	}
	printExamineLine(e)
	return e
}

// examineFailure records a table or file that couldn't be examined at all
// (no schema, unreadable header, unsupported format, ...).
func examineFailure(name, engine string, err error) {
	if !examineMode {
		return
	}
	if *sourceDir != "" {
		if rel, rerr := filepath.Rel(*sourceDir, name); rerr == nil && !strings.HasPrefix(rel, "..") {
			name = rel // a file under the examined directory: shown relative to it
		}
	}
	e := &examineEntry{Name: name, Engine: engine, Failed: err}
	examined = append(examined, e)
	printExamineLine(e)
}

func examineDetail(t *Table) string {
	var parts []string
	switch {
	case t.Engine == "MyISAM", t.Engine == "InnoDB" && t.RowFormat != rowFormatDynamic:
		parts = append(parts, "row_format="+rowFormatName(t.RowFormat))
	}
	if t.Compression != "" && !strings.EqualFold(t.Compression, "none") {
		parts = append(parts, "COMPRESSION="+t.Compression)
	}
	if t.PartitionClause != "" {
		parts = append(parts, "partitioned")
	}
	return strings.Join(parts, ", ")
}

func printExamineLine(e *examineEntry) {
	eng := e.Engine
	if eng == "" {
		eng = "?"
	}
	status := good("OK")
	switch {
	case e.Failed != nil:
		status = bad("UNREADABLE") + ": " + e.Failed.Error()
	case !e.clean():
		var p []string
		if e.Corrupt > 0 {
			p = append(p, fmt.Sprintf("%d damaged %s", e.Corrupt, unitNoun(e.Engine)))
		}
		if e.RowErrs > 0 {
			p = append(p, fmt.Sprintf("%d undecodable row(s)", e.RowErrs))
		}
		if e.Truncated {
			p = append(p, "source file truncated")
		}
		status = bad("DAMAGED") + " (" + strings.Join(p, ", ") + ")"
	}
	rows := ""
	if e.Failed == nil {
		rows = fmt.Sprintf("%d row(s)", e.Rows)
	}
	detail := ""
	if e.Detail != "" {
		detail = "  " + dim("["+e.Detail+"]")
	}
	fmt.Printf("  %s %s %14s  %s%s\n", id(fmt.Sprintf("%-40s", e.Name)), dim(fmt.Sprintf("%-7s", eng)), rows, status, detail)
}

func unitNoun(engine string) string {
	return (&Table{Engine: engine}).corruptNoun()
}

// finishExamine prints the summary and returns an error if anything was
// found wrong (so the exit code says so).
func finishExamine(extraNotes []string) error {
	type tally struct{ tables, rows, bad int }
	byEngine := map[string]*tally{}
	var engines []string
	var problems []*examineEntry
	totalRows := 0
	for _, e := range examined {
		eng := e.Engine
		if eng == "" {
			eng = "?"
		}
		t := byEngine[eng]
		if t == nil {
			t = &tally{}
			byEngine[eng] = t
			engines = append(engines, eng)
		}
		t.tables++
		t.rows += e.Rows
		totalRows += e.Rows
		if !e.clean() {
			t.bad++
			problems = append(problems, e)
		}
	}
	sort.Strings(engines)
	fmt.Println()
	fmt.Println(hi("Summary"))
	for _, eng := range engines {
		t := byEngine[eng]
		line := fmt.Sprintf("%-8s %5d table(s), %d row(s)", eng, t.tables, t.rows)
		if t.bad > 0 {
			line += ", " + bad(fmt.Sprintf("%d with problems", t.bad))
		}
		fmt.Printf("  %s\n", line)
	}
	fmt.Printf("  %-8s %5d table(s), %d row(s)\n", "total", len(examined), totalRows)
	for _, n := range extraNotes {
		fmt.Printf("%s %s\n", dim("Note:       "), n)
	}
	if len(problems) == 0 {
		fmt.Printf("%s every table read cleanly\n", good("Result:     "))
		return nil
	}
	fmt.Printf("%s %d table(s) or file(s) with problems:\n", bad("Result:     "), len(problems))
	for _, e := range problems {
		why := e.Problem
		if e.Failed != nil {
			why = e.Failed.Error()
		}
		fmt.Printf("  %s: %s\n", id(e.Name), why)
	}
	if !*verbose {
		fmt.Printf("%s --verbose lists every damaged page/block and row as it's found\n", dim("Hint:       "))
	}
	return fmt.Errorf("%d of %d table(s)/file(s) have problems - see the summary above", len(problems), len(examined))
}

// examineSingle finishes a single --file examine run.
func examineSingle() error {
	return finishExamine(nil)
}

// examineGuard rejects options that make no sense with --examine.
func examineGuard() error {
	switch {
	case *ddlOnly:
		return fmt.Errorf("--examine reads every row; --ddl-only can't be combined with it")
	case *dumpPage >= 0:
		return fmt.Errorf("--examine and --dump-page can't be combined")
	}
	*skipCorrupted = true // count every problem instead of stopping at the first
	return nil
}
