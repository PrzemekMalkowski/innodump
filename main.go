// innodump - offline schema+data extraction from a MySQL 8.0/8.4, or
// 5.6/5.7, InnoDB .ibd file.
//
// Given table.ibd, produces (by default, --format=sql) ./sqldump/schema.
// table-schema.sql (a best-effort CREATE TABLE, reconstructed from the
// tablespace's embedded SDI, or its .frm file for a pre-8.0 table - see
// frm.go) and ./sqldump/schema.table-data.sql (one INSERT statement per
// live row, decoded by walking the clustered index's B+tree directly out
// of the file - no server involved). Output files are named after the
// table itself rather than the .ibd file, since a shared/common
// tablespace can hold more than one table under a single .ibd (see
// --table below). --format=tsv instead produces a MySQL Shell
// util.loadDump()-compatible dump directory - see tsvdump.go. --source-dir
// recurses over every .ibd file under a directory instead of extracting a
// single --file.
//
// Scope (v1): MySQL 8.0.16+ / 8.4.x tablespaces, ROW_FORMAT=DYNAMIC,
// COMPACT, REDUNDANT, or COMPRESSED, non-partitioned (INSTANT ADD/DROP
// COLUMN history is supported - see instant.go), plus MySQL 5.6/5.7 given
// the table's .frm file alongside it. See schema.go's and frm.go's package
// comments and the README for why, and BuildTable's errors for exactly
// which of these a given file trips.
//
// # Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// Reference material: the mysql-server source (storage/innobase) for the
// on-disk page/record/SDI formats, and KernelMaker/ibdNinja (GPL-3.0),
// which was used throughout development to cross-check those layouts and
// the modern LOB storage format. Not affiliated with Oracle or KernelMaker.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const appName = "innodump"

// version is overridden at build time via -ldflags "-X main.version=...";
// GoReleaser sets it from the release's own git tag (see .goreleaser.yaml).
// The hardcoded fallback below is what a plain "go build"/"go install"
// (no ldflags) prints instead - kept in sync with the latest tagged
// release by hand.
var version = "0.7.9"

var (
	filePath             = flag.String("file", "", "Path to the .ibd file to extract")
	sourceDir            = flag.String("source-dir", "", "Recursively scan this directory for .ibd files and extract every table found in each, instead of a single --file")
	forceScan            = flag.Bool("force-scan", false, "With --source-dir, scan a directory even if it doesn't look like a single instance's datadir or a single database directory (see the README's \"Bulk extraction\" section)")
	includeSystemSchemas = flag.Bool("include-system-schemas", false, "With --source-dir and --format=tsv, also dump system schemas (mysql, sys, performance_schema, information_schema, ndbinfo) - excluded by default, matching MySQL Shell's own util.dumpInstance() default")
	outDir               = flag.String("out-dir", "", "Directory for the output files (default: ./sqldump_<timestamp> for --format=sql, ./tsvdump_<timestamp> for --format=tsv, timestamped date +%Y-%m-%d_%H.%M; created if it doesn't exist)")
	format               = flag.String("format", "sql", `Output format: "sql" (default; schema+data .sql files) or "tsv" (a MySQL Shell util.loadDump()-compatible dump directory - see the README)`)
	compression          = flag.String("compression", "none", `Data-file compression, --format=tsv only: "none" (default) or "zstd" (MySQL Shell util.loadDump()-compatible - see the README)`)
	compressLevel        = flag.Int("compression-level", 1, "zstd compression level, 1-22 (only used with --compression=zstd; default 1, matching util.dumpSchemas()' own lightest/fastest setting)")
	tableName            = flag.String("table", "", `Table name to extract, if the SDI holds more than one, or (required) "schema.table"/"schema/table" when --file is the shared/system tablespace itself (see the README) - not usable with --source-dir, which always extracts every table it finds`)
	limitRows            = flag.Int("limit", 0, "Stop after this many rows (0 = all)")
	ddlOnly              = flag.Bool("ddl-only", false, "Only write the schema file(s); skip walking any table's data entirely")
	deletedOnly          = flag.Bool("deleted-only", false, "Dump only deleted rows instead of live ones - both still delete-marked (not yet purged) and already purged but not yet overwritten (still on the page's own free list) - for forensic recovery of recently deleted data. The output data file name gets a \"-deleted\" suffix, so it never collides with a normal dump of the same table")
	skipCorrupted        = flag.Bool("skip-corrupted", false, "On a corrupted leaf page, note it and try to carry on from the next page instead of stopping")
	noProgress           = flag.Bool("no-progress", false, "Never draw the progress bar on stderr (also honored: NO_PROGRESS=1)")
	verbose              = flag.Bool("verbose", false, "Print extra .ibd/table details: page size, FSP flags, the MySQL version and dictionary/SDI versions that wrote the file, index/column counts")
	yes                  = flag.Bool("yes", false, "Overwrite existing output files without prompting (also honored: YES=1)")
	showVersion          = flag.Bool("version", false, "Print version and exit")
	dumpPage             = flag.Int("dump-page", -1, "debug: hex-dump one page and its record chain, then exit (requires --file)")
	debug                = flag.Bool("debug", false, "debug: print each record's decoded field byte-ranges as they're read")
	myisamToInnoDB       = flag.Bool("myisam-to-innodb", false, "Write every MyISAM table's DDL as ENGINE=InnoDB instead (adding a KEY for an AUTO_INCREMENT column InnoDB would otherwise reject), for migrating an old datadir's MyISAM tables to InnoDB on a new server - the row data itself is unchanged")
)

func main() {
	// Both need to be set before Parse(), since -h/--help makes flag.Parse
	// itself call flag.Usage and exit before any of our own code runs.
	initColor()
	flag.Usage = printUsage
	flag.Parse()
	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		return
	}
	if *filePath == "" && *sourceDir == "" {
		fmt.Printf("%s %s\n", appName, version)
		fmt.Printf("Usage: %s --file /path/to/table.ibd [--out-dir DIR] [--table NAME] [--limit N]\n", appName)
		fmt.Printf("       %s --source-dir /path/to/datadir [--out-dir DIR] [--limit N]\n", strings.Repeat(" ", len(appName)))
		fmt.Println("       [--format sql|tsv] [--ddl-only] [--myisam-to-innodb] [--skip-corrupted] [--no-progress]")
		fmt.Println("       [--verbose] [--yes] [--debug] [--dump-page N] [--version]")
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", bad("Error:"), err)
		os.Exit(1)
	}
}

// printBanner prints this run's own bold "=== innodump X.Y.Z ==="
// header - the one line every run prints before its actual results,
// whichever mode/format those end up in.
func printBanner() {
	fmt.Println(hi(fmt.Sprintf("=== %s %s ===", appName, version)))
}

// printUsage is flag.Usage (see main): -h/--help's per-flag listing, with
// each flag's own "-name type" line in id() and its indented description
// line in dim() - Go's flag.PrintDefaults itself has no hook for per-part
// styling, so this captures its plain-text output and colours it line by
// line instead of reimplementing its (default-value-aware, type-aware)
// formatting from scratch.
func printUsage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "Usage of %s:\n", os.Args[0])

	var buf bytes.Buffer
	flag.CommandLine.SetOutput(&buf)
	flag.PrintDefaults()
	flag.CommandLine.SetOutput(out)

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "  -") {
			fmt.Fprintln(out, id(line))
		} else {
			fmt.Fprintln(out, dim(line))
		}
	}
}

func run() error {
	switch *format {
	case "sql", "tsv":
	default:
		return fmt.Errorf(`--format must be "sql" or "tsv", not %q`, *format)
	}
	compressionKindVal, err := parseCompressionKind(*compression)
	if err != nil {
		return err
	}
	if compressionKindVal != compressionNone && *format != "tsv" {
		return fmt.Errorf("--compression=%s requires --format=tsv (MySQL Shell's own dump directory format)", *compression)
	}
	if *compressLevel < 1 || *compressLevel > 22 {
		return fmt.Errorf("--compression-level must be between 1 and 22, not %d", *compressLevel)
	}
	debugFields = *debug
	convertMyISAMToInnoDB = *myisamToInnoDB
	initProgress(*noProgress)

	if *sourceDir != "" {
		if *filePath != "" {
			return fmt.Errorf("--source-dir cannot be combined with --file")
		}
		if *tableName != "" {
			return fmt.Errorf("--source-dir always extracts every table it finds; --table cannot be used with it")
		}
		if *dumpPage >= 0 {
			return fmt.Errorf("--dump-page requires --file, not --source-dir")
		}
		return runSourceDir()
	}
	if isMyISAMDataFile(*filePath) {
		return runMyISAMFile()
	}
	if base, ok := partitionFileBase(*filePath); ok {
		return runPartitionedFile(base)
	}
	return runSingleFile()
}

// runSingleFile is the original, single-.ibd-file mode: --file (optionally
// --table, when the file's SDI holds more than one table).
func runSingleFile() error {
	sp, err := OpenSpace(*filePath)
	if err != nil {
		return err
	}
	defer sp.Close()

	if *verbose {
		printSpaceInfo(sp, *filePath)
	}
	if *dumpPage >= 0 {
		return debugDumpPage(sp, uint32(*dumpPage))
	}

	var target *ddTableJSON
	var mysqlVersionID uint32 // 0 for a pre-8.0/.frm-derived table (see tsvdump.go's noteSourceVersion)
	if !sp.Flags.sdi && sp.SpaceID == 0 {
		// The shared/system tablespace itself (ibdata1, space id 0) - every
		// table's data lives here when innodb_file_per_table=0, and any
		// individual table can still be left here explicitly otherwise.
		// See sysdict.go's package comment.
		if *verbose {
			fmt.Printf("%s %s\n", dim("SDI:        "), val("absent (pre-8.0 tablespace) - this is the shared/system tablespace (space id 0)"))
		}
		var err error
		target, err = resolveSharedTablespaceTable(sp, *filePath, *tableName)
		if err != nil {
			return err
		}
	} else if !sp.Flags.sdi {
		// Pre-8.0, file-per-table: no SDI at all - fall back to the .frm
		// file that must sit alongside the .ibd (see findLegacyFRM).
		frmPath, err := findLegacyFRM(*filePath)
		if err != nil {
			return err
		}
		if *verbose {
			fmt.Printf("%s %s\n", dim("SDI:        "), val(fmt.Sprintf("absent (pre-8.0 tablespace) - using %s", frmPath)))
		}
		target, err = parseFRM(frmPath, sp, nil)
		if err != nil {
			return fmt.Errorf("reading %s: %w", frmPath, err)
		}
	} else {
		rawTables, sdiInfo, err := LoadSDITables(sp)
		if err != nil {
			return fmt.Errorf("reading SDI: %w", err)
		}
		if len(rawTables) == 0 {
			return fmt.Errorf("no table definitions found in this tablespace's SDI")
		}
		mysqlVersionID = sdiInfo.MySQLVersionID
		if *verbose {
			fmt.Printf("%s %s\n", dim("SDI:        "), val(fmt.Sprintf("present (written by MySQL %s, dd_version %d, sdi_version %d)",
				formatMySQLVersion(sdiInfo.MySQLVersionID), sdiInfo.DDVersion, sdiInfo.SDIVersion)))
			fmt.Printf("%s %s\n", dim("Tables in SDI:"), val(fmt.Sprintf("%d", len(rawTables))))
		}

		var parsed []*ddTableJSON
		for _, raw := range rawTables {
			var dt ddTableJSON
			if err := json.Unmarshal(raw, &dt); err != nil {
				return fmt.Errorf("parsing SDI table JSON: %w", err)
			}
			parsed = append(parsed, &dt)
		}

		target, err = pickTable(parsed, *filePath, *tableName)
		if err != nil {
			return err
		}
	}

	t, err := BuildTable(target)
	if err != nil {
		return fmt.Errorf("table %q: %w", target.Name, err)
	}
	if *verbose {
		fmt.Printf("%s %s\n", dim("Row format: "), val(rowFormatName(t.RowFormat)))
		fmt.Printf("%s %s\n", dim("Clustered index:"), val(fmt.Sprintf("%q (id %d), root page %d", t.IndexName, t.IndexID, t.RootPage)))
		fmt.Printf("%s %s\n", dim("Secondary indexes:"), val(fmt.Sprintf("%d", len(t.SecondaryIndexes))))
		fmt.Printf("%s %s\n", dim("Physical fields:"), val(fmt.Sprintf("%d (%d output)", len(t.PhysicalFields), len(OutputColumns(t)))))
	}

	return writeSingleTable(t, *filePath, mysqlVersionID, "", func(outCols []*Column, format outputFormat) RowWalker {
		return innodbRowWalker(sp, t, outCols, format, *skipCorrupted, *deletedOnly)
	})
}

// writeSingleTable is --file's shared output half for an InnoDB table,
// once t is built: the banner, then either one TSV dump directory or one
// schema+data .sql pair. makeWalker builds the row source once the output
// columns and format are known; partitions, if non-empty, is printed as a
// "Partitions:" line (see runPartitionedFile).
func writeSingleTable(t *Table, srcPath string, mysqlVersionID uint32, partitions string,
	makeWalker func(outCols []*Column, format outputFormat) RowWalker) error {
	outCols := OutputColumns(t)
	if len(outCols) == 0 {
		return fmt.Errorf("table %q has no columns this tool can output", t.Name)
	}

	printBanner()
	fmt.Printf("%s %s (row_format=%s)\n", dim("Table:      "), id(t.SchemaRef+"."+t.Name), rowFormatName(t.RowFormat))
	if partitions != "" {
		fmt.Printf("%s %s\n", dim("Partitions: "), partitions)
	}
	fmt.Printf("%s %d output (%d total incl. system/hidden)\n", dim("Columns:    "), len(outCols), len(t.Columns))

	if *format == "tsv" {
		ck, _ := parseCompressionKind(*compression) // already validated in run()
		dir := *outDir
		if dir == "" {
			dir = defaultOutDir(*format)
		}
		if err := confirmOverwriteDir(dir, *yes || os.Getenv("YES") != ""); err != nil {
			return err
		}
		d, err := NewTSVDump(dir)
		if err != nil {
			return err
		}
		rw := makeWalker(outCols, formatTSV)
		nOK, nErr, nCorruptPages, truncated, err := d.AddTable(rw, t, outCols, *ddlOnly, *limitRows, *skipCorrupted, *deletedOnly,
			fmt.Sprintf("decoding %s.%s", t.SchemaRef, t.Name), mysqlVersionID, ck, *compressLevel)
		if err != nil {
			return err
		}
		if err := d.Finish(); err != nil {
			return err
		}
		compressNote := ""
		if ck != compressionNone {
			compressNote = fmt.Sprintf(", %s level %d", ck, *compressLevel)
		}
		fmt.Printf("%s %s (MySQL Shell util.loadDump()-compatible%s)\n", dim("Dump dir:   "), id(dir), compressNote)
		if *ddlOnly {
			return nil
		}
		printRowSummary(nOK, nErr, nCorruptPages, *deletedOnly)
		if truncated {
			printTruncatedNote()
		}
		return nil
	}

	// --format=sql (default)
	base := outputBaseName(t.SchemaRef, t.Name)
	dir := *outDir
	if dir == "" {
		dir = defaultOutDir(*format)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}
	schemaPath := filepath.Join(dir, base+"-schema.sql")
	dataPath := filepath.Join(dir, sqlDataFilename(base, *deletedOnly))

	wantPaths := []string{schemaPath}
	if !*ddlOnly {
		wantPaths = append(wantPaths, dataPath)
	}
	if err := confirmOverwrite(wantPaths, *yes || os.Getenv("YES") != ""); err != nil {
		return err
	}

	rw := makeWalker(outCols, formatSQL)
	nOK, nErr, nCorruptPages, truncated, err := writeSQLTable(rw, srcPath, schemaPath, dataPath, t, outCols,
		*ddlOnly, *limitRows, *skipCorrupted, *deletedOnly)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n", dim("Schema file:"), id(schemaPath))
	if *ddlOnly {
		return nil
	}
	rowNoun := "row(s)"
	if *deletedOnly {
		rowNoun = "delete-marked row(s)"
	}
	fmt.Printf("%s %s (%s %s written", dim("Data file:  "), id(dataPath), good(fmt.Sprintf("%d", nOK)), rowNoun)
	if nErr > 0 {
		fmt.Printf(", %s row(s) skipped - see warnings above", warn(fmt.Sprintf("%d", nErr)))
	}
	if nCorruptPages > 0 {
		fmt.Printf(", %s corrupted page(s) skipped - see warnings above", bad(fmt.Sprintf("%d", nCorruptPages)))
	}
	fmt.Printf(")\n")
	if truncated {
		printTruncatedNote()
	}
	return nil
}

// runPartitionedFile is --file's path for one partition of a partitioned
// InnoDB table ("t#p#p0.ibd"): a partition is never a table on its own,
// so this extracts the whole table - every partition file found next to
// the one given (see partition.go) - into one schema+data pair or TSV
// table, exactly as --source-dir would.
func runPartitionedFile(base string) error {
	if *tableName != "" {
		return fmt.Errorf("--table is not usable with a partition's file - every partition belongs to exactly one table, which is extracted as a whole")
	}
	if *dumpPage >= 0 {
		sp, err := OpenSpace(*filePath)
		if err != nil {
			return err
		}
		defer sp.Close()
		return debugDumpPage(sp, uint32(*dumpPage))
	}
	files, err := findPartitionFiles(base)
	if err != nil {
		return err
	}
	pt, err := resolvePartitionedTable(base, files)
	if err != nil {
		return err
	}
	op, err := openPartitionedTable(pt)
	if err != nil {
		return fmt.Errorf("table %q: %w", pt.Table, err)
	}
	defer op.Close()
	warnPartitionGaps(pt, op, *filePath)
	if *verbose {
		for i, t := range op.Tables {
			fmt.Printf("%s %s\n", dim("Partition:  "), val(fmt.Sprintf("%s - %s, space id %d, %d pages, clustered index id %d, root page %d",
				op.Names[i], filepath.Base(op.Paths[i]), op.Spaces[i].SpaceID, op.Spaces[i].NumPages, t.IndexID, t.RootPage)))
		}
	}
	return writeSingleTable(op.Main, *filePath, pt.MySQLVersionID, op.describe(), func(outCols []*Column, format outputFormat) RowWalker {
		return op.rowWalker(format, *skipCorrupted, *deletedOnly)
	})
}

// warnPartitionGaps warns (stderr) about partitions whose file is missing,
// and partition files no partition of the table's own definition claims.
func warnPartitionGaps(pt *partitionedTable, op *openPartitions, label string) {
	for _, m := range op.Missing {
		fmt.Fprintf(os.Stderr, "%s %s: table %s.%s: partition %s's tablespace file wasn't found - its rows are missing from this dump\n",
			warn("warning:"), label, pt.Schema, pt.Table, m)
	}
	for _, f := range pt.Extra {
		fmt.Fprintf(os.Stderr, "%s %s: %s isn't any partition of %s.%s's own definition - left out\n",
			warn("warning:"), label, filepath.Base(f), pt.Schema, pt.Table)
	}
}

// runMyISAMFile is --file's MyISAM path: --file points directly at a
// table's .MYD (or .MYI - myisamDataPath resolves either to the .MYD data
// itself always reads) rather than an InnoDB tablespace file - see
// isMyISAMDataFile (main.go's run()) and myisam.go's own package comment.
func runMyISAMFile() error {
	if *tableName != "" {
		return fmt.Errorf("--table is not usable with a MyISAM --file - a MyISAM data file always holds exactly one table")
	}
	if *dumpPage >= 0 {
		return fmt.Errorf("--dump-page requires an InnoDB --file, not a MyISAM one")
	}
	if *deletedOnly {
		return fmt.Errorf("--deleted-only is not supported for MyISAM tables yet (v1 limitation)")
	}
	mydPath := myisamDataPath(*filePath)

	raw, mysqlVersionID, err := findMyISAMSchema(mydPath)
	if err != nil {
		return err
	}
	t, err := BuildMyISAMTable(raw)
	if err != nil {
		return fmt.Errorf("table %q: %w", raw.Name, err)
	}
	layout, err := buildMyISAMLayout(t)
	if err != nil {
		return fmt.Errorf("table %q: %w", t.Name, err)
	}
	if err := applyMYIHeader(mydPath, layout); err != nil {
		return fmt.Errorf("table %q: %w", t.Name, err)
	}
	if *verbose {
		printMyISAMInfo(mydPath, t, layout)
	}

	outCols := OutputColumns(t)
	if len(outCols) == 0 {
		return fmt.Errorf("table %q has no columns this tool can output", t.Name)
	}

	printBanner()
	fmt.Printf("%s %s (engine=%s, row_format=%s)\n", dim("Table:      "), id(t.SchemaRef+"."+t.Name), t.Engine, rowFormatName(t.RowFormat))
	if convertMyISAMToInnoDB {
		note := ""
		if k := myisamToInnoDBAutoIncKey(t); k != "" {
			note = fmt.Sprintf(", with an extra KEY %s added for its AUTO_INCREMENT column - see the DDL header", backquote(k))
		}
		fmt.Printf("%s DDL written as ENGINE=InnoDB (--myisam-to-innodb)%s\n", dim("Converted:  "), note)
	}
	fmt.Printf("%s %d output (%d total incl. hidden)\n", dim("Columns:    "), len(outCols), len(t.Columns))

	if *format == "tsv" {
		ck, _ := parseCompressionKind(*compression) // already validated in run()
		dir := *outDir
		if dir == "" {
			dir = defaultOutDir(*format)
		}
		if err := confirmOverwriteDir(dir, *yes || os.Getenv("YES") != ""); err != nil {
			return err
		}
		d, err := NewTSVDump(dir)
		if err != nil {
			return err
		}
		rw := myisamRowWalker(mydPath, t, layout, outCols, formatTSV, *skipCorrupted)
		nOK, nErr, nCorruptPages, truncated, err := d.AddTable(rw, t, outCols, *ddlOnly, *limitRows, *skipCorrupted, false,
			fmt.Sprintf("decoding %s.%s", t.SchemaRef, t.Name), mysqlVersionID, ck, *compressLevel)
		if err != nil {
			return err
		}
		if err := d.Finish(); err != nil {
			return err
		}
		compressNote := ""
		if ck != compressionNone {
			compressNote = fmt.Sprintf(", %s level %d", ck, *compressLevel)
		}
		fmt.Printf("%s %s (MySQL Shell util.loadDump()-compatible%s)\n", dim("Dump dir:   "), id(dir), compressNote)
		if *ddlOnly {
			return nil
		}
		printRowSummary(nOK, nErr, nCorruptPages, false)
		if truncated {
			printTruncatedNote()
		}
		return nil
	}

	// --format=sql (default)
	base := outputBaseName(t.SchemaRef, t.Name)
	dir := *outDir
	if dir == "" {
		dir = defaultOutDir(*format)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}
	schemaPath := filepath.Join(dir, base+"-schema.sql")
	dataPath := filepath.Join(dir, sqlDataFilename(base, false))

	wantPaths := []string{schemaPath}
	if !*ddlOnly {
		wantPaths = append(wantPaths, dataPath)
	}
	if err := confirmOverwrite(wantPaths, *yes || os.Getenv("YES") != ""); err != nil {
		return err
	}

	rw := myisamRowWalker(mydPath, t, layout, outCols, formatSQL, *skipCorrupted)
	nOK, nErr, nCorruptPages, truncated, err := writeSQLTable(rw, mydPath, schemaPath, dataPath, t, outCols,
		*ddlOnly, *limitRows, *skipCorrupted, false)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n", dim("Schema file:"), id(schemaPath))
	if *ddlOnly {
		return nil
	}
	fmt.Printf("%s %s (%s row(s) written", dim("Data file:  "), id(dataPath), good(fmt.Sprintf("%d", nOK)))
	if nErr > 0 {
		fmt.Printf(", %s row(s) skipped - see warnings above", warn(fmt.Sprintf("%d", nErr)))
	}
	if nCorruptPages > 0 {
		fmt.Printf(", %s corrupted record(s) skipped - see warnings above", bad(fmt.Sprintf("%d", nCorruptPages)))
	}
	fmt.Printf(")\n")
	if truncated {
		printTruncatedNote()
	}
	return nil
}

// printMyISAMInfo prints --verbose's extra detail for a MyISAM --file run -
// the counterpart to printSpaceInfo's InnoDB tablespace detail.
func printMyISAMInfo(path string, t *Table, lay *myisamLayout) {
	fi, err := os.Stat(path)
	size := int64(-1)
	if err == nil {
		size = fi.Size()
	}
	fmt.Printf("%s %s (%d bytes)\n", dim("File:       "), val(path), size)
	fmt.Printf("%s %s\n", dim("Row format: "), val(rowFormatName(t.RowFormat)))
	fmt.Printf("%s %s\n", dim("Null bytes: "), val(fmt.Sprintf("%d", lay.NullBytes)))
	fmt.Printf("%s %s\n", dim("Unpacked record length:"), val(fmt.Sprintf("%d bytes", lay.RecLength)))
	fmt.Printf("%s %s\n", dim("Physical fields:"), val(fmt.Sprintf("%d (%d output)", len(lay.Fields), len(OutputColumns(t)))))
	fmt.Printf("%s %s\n", dim("Secondary indexes:"), val(fmt.Sprintf("%d", len(t.SecondaryIndexes))))
	if lay.HaveMYI {
		fmt.Printf("%s %s\n", dim(".MYI row count:"), val(fmt.Sprintf("%d (as of the table's last clean close)", lay.MYIRecords)))
	} else {
		fmt.Printf("%s %s\n", dim(".MYI header:"), val("not found or unreadable - record layout not cross-checked"))
	}
}

// writeSQLTable writes t's schema+data .sql files (schemaPath/dataPath),
// exactly as the original (pre-multi-file) single-table SQL path always
// has - shared by runSingleFile and runSourceDir's --format=sql branch.
// truncated is true if the walk ended because the source file turned out
// to hold fewer pages than the table's own B+tree expects (see btree.go's
// truncatedReason) - the caller should make sure that's not lost in the
// generic nCorruptPages count, since it means specifically that the file
// wasn't fully copied, not that one page among many good ones was bad.
func writeSQLTable(rw RowWalker, srcPath, schemaPath, dataPath string, t *Table, outCols []*Column,
	ddlOnly bool, limitRows int, skipCorrupted, deletedOnly bool) (nOK, nErr, nCorruptPages int, truncated bool, err error) {
	if !ddlOnly {
		autoIncIdx := -1
		for i, c := range outCols {
			if c == t.AutoIncrementCol {
				autoIncIdx = i
			}
		}

		f, ferr := os.Create(dataPath)
		if ferr != nil {
			return 0, 0, 0, false, fmt.Errorf("writing %s: %w", dataPath, ferr)
		}
		defer f.Close()
		// Buffered: at 10M+ rows, one unbuffered Write per row (as this used
		// to be) turns into one write(2) syscall per row, which dominates
		// wall-clock time far more than any of the actual decoding does.
		bw := bufio.NewWriterSize(f, 1<<20)

		fmt.Fprintf(bw, "-- Decoded by %s from %s, table %s.%s\n", appName, filepath.Base(srcPath), t.SchemaRef, t.Name)
		prefix := InsertPrefix(t, outCols)

		var maxAutoInc uint64
		// total is a rough proxy only: not every page in the file belongs to
		// this index, so the bar may not reach 100% on a small table sharing
		// a big tablespace, and rarely (page splits/allocation order) the
		// leaf chain can visit a lower page number after a higher one. Either
		// way it still gives a fair sense of progress on a large extraction.
		pr := newProg(fmt.Sprintf("decoding %s.%s", t.SchemaRef, t.Name), rw.ProgressTotal)
		defer pr.finish()
		onCorrupt := func(cp CorruptPage) {
			nCorruptPages++
			if cp.Truncated {
				truncated = true
			}
			// pr.Warnf (not a plain Fprintf) clears the bar's own
			// in-progress line first, so this can't land mid-redraw and
			// splice into it - see progress.go.
			pr.Warnf("%s corrupted %s: %s\n", warn("warning:"), t.corruptUnitLabel(cp.PageNo), cp.Reason)
			fmt.Fprintf(bw, "-- skipped corrupted %s (table %s.%s): %s\n",
				t.corruptUnitLabel(cp.PageNo), t.SchemaRef, t.Name, cp.Reason)
		}
		onRow := func(roe RowOrError) bool {
			pr.set(roe.ProgressBase + int64(roe.PageNo))
			if roe.Err != nil {
				nErr++
				pr.Warnf("%s %s: %v\n", warn("warning:"), t.corruptRowLabel(roe.PageNo, roe.RecOff), roe.Err)
				fmt.Fprintf(bw, "-- skipped a row at %s: %v\n", t.corruptRowLabel(roe.PageNo, roe.RecOff), roe.Err)
				return limitRows == 0 || nOK+nErr < limitRows
			}
			fmt.Fprintf(bw, "%s%s;\n", prefix, FormatRow(roe.Row))
			if autoIncIdx >= 0 {
				if v, perr := strconv.ParseUint(roe.Row.Values[autoIncIdx], 10, 64); perr == nil && v > maxAutoInc {
					maxAutoInc = v
				}
			}
			nOK++
			pr.setExtra(int64(nOK))
			return limitRows == 0 || nOK < limitRows
		}
		if deletedOnly {
			// The same historical row content can genuinely turn up more
			// than once (see dedupDeletedRows) - drop exact repeats so
			// nOK, and the data file itself, only ever reflect distinct
			// recovered rows.
			onRow = dedupDeletedRows(onRow)
		}
		walkErr := rw.Walk(onCorrupt, onRow)
		pr.finish()
		if ferr := bw.Flush(); ferr != nil && walkErr == nil {
			walkErr = ferr
		}
		if walkErr != nil {
			return nOK, nErr, nCorruptPages, truncated, fmt.Errorf("walking table rows: %w", walkErr)
		}

		// The SDI carries no persisted auto-increment counter (see
		// Table.AutoIncrementNext in schema.go); approximate it from the data.
		if t.AutoIncrementCol != nil {
			next := maxAutoInc + 1
			t.AutoIncrementNext = &next
		}
	}

	if err := os.WriteFile(schemaPath, []byte(GenerateDDL(t)), 0644); err != nil {
		return nOK, nErr, nCorruptPages, truncated, fmt.Errorf("writing %s: %w", schemaPath, err)
	}
	return nOK, nErr, nCorruptPages, truncated, nil
}

func printRowSummary(nOK, nErr, nCorruptPages int, deletedOnly bool) {
	label := "Rows:"
	if deletedOnly {
		label = "Deleted rows:"
	}
	fmt.Printf("%s %s written", dim(fmt.Sprintf("%-12s", label)), good(fmt.Sprintf("%d", nOK)))
	if nErr > 0 {
		fmt.Printf(", %s skipped - see warnings above", warn(fmt.Sprintf("%d", nErr)))
	}
	if nCorruptPages > 0 {
		fmt.Printf(", %s corrupted page(s) skipped - see warnings above", bad(fmt.Sprintf("%d", nCorruptPages)))
	}
	fmt.Printf("\n")
}

// printTruncatedNote is the closing message a run owes the user whenever a
// table's own source file turned out to end early (see btree.go's
// truncatedReason) - distinct from the generic "N corrupted page(s)
// skipped" printRowSummary already reports, since that alone doesn't make
// clear the walk stopped because the file simply ran out partway through
// (most likely an interrupted copy), not because of one bad page among
// otherwise-good ones.
func printTruncatedNote() {
	fmt.Printf("%s the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.\n",
		warn("Note:       "))
}

// runSourceDir recursively finds every .ibd file under --source-dir and
// extracts every table each one holds (unlike --file, which extracts one
// table - or errors out asking which one - from a single tablespace).
func runSourceDir() error {
	kind, safe, err := classifySourceDir(*sourceDir)
	if err != nil {
		return fmt.Errorf("checking %s: %w", *sourceDir, err)
	}
	if !safe && !*forceScan {
		return fmt.Errorf("%s doesn't look like a single instance's datadir (no ibdata1 found directly inside it) or a single database directory "+
			"(it holds subdirectories of its own, which a real one never does) - pointing --source-dir at a directory holding several instances "+
			"or backups side by side could extract far more than intended. Point it at the specific datadir/database directory you want instead, "+
			"or pass --force-scan to scan %s exactly as given", *sourceDir, *sourceDir)
	}

	printBanner()
	if safe {
		fmt.Printf("Following %s recursively — recognized as %s, so every InnoDB table found inside (file-per-table .ibd files, plus any table found only in ibdata1's own shared tablespace) and every MyISAM table (.MYD files) will be dumped.\n", id(*sourceDir), kind)
	} else {
		fmt.Printf("Following %s recursively (--force-scan) — every InnoDB table found anywhere inside (file-per-table .ibd files, plus any table found only in ibdata1's own shared tablespace) and every MyISAM table (.MYD files) will be dumped.\n", id(*sourceDir))
	}

	dir := *outDir
	if dir == "" {
		dir = defaultOutDir(*format)
	}

	var d *TSVDump
	if *format == "tsv" {
		if err := confirmOverwriteDir(dir, *yes || os.Getenv("YES") != ""); err != nil {
			return err
		}
		var err error
		d, err = NewTSVDump(dir)
		if err != nil {
			return err
		}
	} else if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}

	// Both .ibd (file-per-table) and .frm are collected in the same walk:
	// a .frm with no matching .ibd next to it (below) is a candidate for
	// shared-tablespace resolution - see the package comment on
	// sysdict.go for why this is only ever needed for a pre-8.0/MariaDB
	// tablespace (innodb_file_per_table=0), never a modern SDI-based one.
	var ibdFiles, frmFiles, mydFiles []string
	walkErr := filepath.WalkDir(*sourceDir, func(path string, ent fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ent.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".ibd":
			ibdFiles = append(ibdFiles, path)
		case ".frm":
			frmFiles = append(frmFiles, path)
		case ".myd":
			mydFiles = append(mydFiles, path)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("scanning %s: %w", *sourceDir, walkErr)
	}
	sort.Strings(ibdFiles)
	sort.Strings(frmFiles)
	sort.Strings(mydFiles)

	// A partitioned table's partitions are separate .ibd files, but one
	// table: group them by the table they belong to (partition.go) and
	// extract each group once, rather than each file on its own.
	partGroups := map[string][]string{}
	var partBases []string
	nPartFiles := 0
	{
		var plain []string
		for _, p := range ibdFiles {
			if base, ok := partitionFileBase(p); ok {
				if partGroups[base] == nil {
					partBases = append(partBases, base)
				}
				partGroups[base] = append(partGroups[base], p)
				nPartFiles++
				continue
			}
			plain = append(plain, p)
		}
		ibdFiles = plain
	}

	// A .frm with a same-named .ibd right beside it is an ordinary
	// file-per-table table, already covered by the ibdFiles loop below
	// (loadTargets/findLegacyFRM), and one with a same-named .MYD is a
	// pre-8.0 MyISAM table, covered by the mydFiles loop (findMyISAMSchema)
	// - only a .frm left with neither might be a shared-tablespace table
	// worth resolving via ibdata1.
	coveredBases := make(map[string]bool, len(ibdFiles)+len(mydFiles)+len(partBases))
	for _, p := range append(append([]string(nil), ibdFiles...), mydFiles...) {
		coveredBases[strings.TrimSuffix(p, filepath.Ext(p))] = true
	}
	for _, base := range partBases { // a partitioned table's own .frm
		coveredBases[base] = true
	}
	var sharedFRMs []string
	for _, p := range frmFiles {
		if !coveredBases[strings.TrimSuffix(p, filepath.Ext(p))] {
			sharedFRMs = append(sharedFRMs, p)
		}
	}

	if len(ibdFiles) == 0 && len(sharedFRMs) == 0 && len(mydFiles) == 0 && len(partBases) == 0 {
		return fmt.Errorf("no .ibd, .frm, or .MYD files found under %s", *sourceDir)
	}

	srcMsg := fmt.Sprintf("%d .ibd file(s)", len(ibdFiles)+nPartFiles)
	if len(partBases) > 0 {
		srcMsg += fmt.Sprintf(" - %d of them partitions of %d partitioned table(s)", nPartFiles, len(partBases))
	}
	if len(sharedFRMs) > 0 {
		srcMsg += fmt.Sprintf(", %d .frm-only table(s) to resolve via ibdata1", len(sharedFRMs))
	}
	if len(mydFiles) > 0 {
		srcMsg += fmt.Sprintf(", %d MyISAM .MYD file(s)", len(mydFiles))
	}
	fmt.Printf("%s %s (%s found)\n", dim("Source:     "), id(*sourceDir), srcMsg)

	var nFilesOK, nFileErrs, nTables, nRowsTotal, nRowErrTotal, nCorruptTotal, nTruncatedTotal, nSkippedTotal, nNotFoundTotal int
	for _, path := range ibdFiles {
		nt, nr, nre, nc, ntr, ns, err := extractFileTables(path, dir, d)
		nTables += nt
		nRowsTotal += nr
		nRowErrTotal += nre
		nCorruptTotal += nc
		nTruncatedTotal += ntr
		nSkippedTotal += ns
		if err != nil {
			nFileErrs++
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", warn("warning:"), path, err)
			continue
		}
		nFilesOK++
	}

	for _, base := range partBases {
		files := partGroups[base]
		nt, nr, nre, nc, ntr, ns, err := extractPartitionedTable(base, files, dir, d)
		nTables += nt
		nRowsTotal += nr
		nRowErrTotal += nre
		nCorruptTotal += nc
		nTruncatedTotal += ntr
		nSkippedTotal += ns
		if err != nil {
			nFileErrs += len(files)
			fmt.Fprintf(os.Stderr, "%s %s (partitioned table, %d partition file(s)): %v\n", warn("warning:"), base, len(files), err)
			continue
		}
		nFilesOK += len(files)
	}

	var nMydOK, nMydErrs, nConverted, nConvertedKeys int
	for _, path := range mydFiles {
		nt, nr, nre, nc, ntr, ns, err := extractMyISAMFileTable(path, dir, d)
		if nt > 0 && convertMyISAMToInnoDB {
			nConverted++
			if myisamConvertAddsKey(path) {
				nConvertedKeys++
			}
		}
		nTables += nt
		nRowsTotal += nr
		nRowErrTotal += nre
		nCorruptTotal += nc
		nTruncatedTotal += ntr
		nSkippedTotal += ns
		if err != nil {
			nMydErrs++
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", warn("warning:"), path, err)
			continue
		}
		nMydOK++
	}

	if len(sharedFRMs) > 0 {
		ibdataPath := filepath.Join(*sourceDir, "ibdata1")
		if _, statErr := os.Stat(ibdataPath); statErr != nil {
			fmt.Fprintf(os.Stderr, "%s %d .frm file(s) with no matching .ibd were found under %s, but no ibdata1 sits directly inside it - "+
				"they can't be resolved from here (a shared/general tablespace elsewhere, or simply non-InnoDB tables)\n",
				warn("warning:"), len(sharedFRMs), *sourceDir)
		} else if sp, err := OpenSpace(ibdataPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s opening %s: %v\n", warn("warning:"), ibdataPath, err)
		} else {
			locs, err := ListTableLocations(sp)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s reading %s's internal dictionary: %v\n", warn("warning:"), ibdataPath, err)
			} else {
				nt, nr, nre, nc, ntr, ns, nnf := extractSharedFRMTables(sp, ibdataPath, dir, sharedFRMs, locs, d)
				nTables += nt
				nRowsTotal += nr
				nRowErrTotal += nre
				nCorruptTotal += nc
				nTruncatedTotal += ntr
				nSkippedTotal += ns
				nNotFoundTotal += nnf
			}
			sp.Close()
		}
	}

	if d != nil {
		if err := d.Finish(); err != nil {
			return fmt.Errorf("writing dump metadata: %w", err)
		}
	}

	if len(ibdFiles)+nPartFiles > 0 {
		fmt.Printf("%s %s/%d .ibd file(s) processed", dim("Files:      "), good(fmt.Sprintf("%d", nFilesOK)), len(ibdFiles)+nPartFiles)
		if nFileErrs > 0 {
			fmt.Printf(" (%s failed - see warnings above)", bad(fmt.Sprintf("%d", nFileErrs)))
		}
		fmt.Println()
	}
	if len(mydFiles) > 0 {
		fmt.Printf("%s %s/%d MyISAM .MYD file(s) processed", dim("Files:      "), good(fmt.Sprintf("%d", nMydOK)), len(mydFiles))
		if nMydErrs > 0 {
			fmt.Printf(" (%s failed - see warnings above)", bad(fmt.Sprintf("%d", nMydErrs)))
		}
		fmt.Println()
	}
	fmt.Printf("%s %d extracted\n", dim("Tables:     "), nTables)
	if convertMyISAMToInnoDB && len(mydFiles) > 0 {
		fmt.Printf("%s %s MyISAM table(s) written as ENGINE=InnoDB (--myisam-to-innodb)", dim("Converted:  "), good(fmt.Sprintf("%d", nConverted)))
		if nConvertedKeys > 0 {
			fmt.Printf(", %d of them with an extra KEY added for their AUTO_INCREMENT column - see each one's DDL header", nConvertedKeys)
		}
		fmt.Println()
	}
	if nSkippedTotal > 0 {
		fmt.Printf("%s %d system-schema table(s) skipped (mysql/sys/performance_schema/information_schema/ndbinfo) - pass %s to include them\n",
			dim("Skipped:    "), nSkippedTotal, id("--include-system-schemas"))
	}
	if nNotFoundTotal > 0 {
		fmt.Printf("%s %d .frm file(s) with no matching .ibd had no InnoDB entry in ibdata1's own dictionary either - likely a non-InnoDB table, a VIEW, or a general tablespace this tool can't reach - skipped\n",
			dim("Note:       "), nNotFoundTotal)
	}
	if !*ddlOnly {
		printRowSummary(nRowsTotal, nRowErrTotal, nCorruptTotal, *deletedOnly)
	}
	fmt.Printf("%s %s\n", dim("Output:     "), id(dir))
	if nTruncatedTotal > 0 {
		plural := ""
		if nTruncatedTotal != 1 {
			plural = "s"
		}
		fmt.Printf("%s %d source file%s looked truncated (not fully copied) - recovered every row up to where each one ends; anything stored after that point is missing.\n",
			warn("Note:       "), nTruncatedTotal, plural)
	}
	if nFileErrs > 0 || nMydErrs > 0 {
		var parts []string
		if nFileErrs > 0 {
			parts = append(parts, fmt.Sprintf("%d of %d .ibd file(s)", nFileErrs, len(ibdFiles)+nPartFiles))
		}
		if nMydErrs > 0 {
			parts = append(parts, fmt.Sprintf("%d of %d MyISAM .MYD file(s)", nMydErrs, len(mydFiles)))
		}
		return fmt.Errorf("%s failed - see warnings above", strings.Join(parts, ", "))
	}
	return nil
}

// classifySourceDir decides whether dir is safe to recurse into without an
// explicit --force-scan: either it's a single MySQL/MariaDB instance's own
// datadir (an ibdata1 file sits directly inside it), or a single database
// directory (every entry directly inside it is a file, not a further
// subdirectory - a real schema directory never nests another one inside
// itself). Anything else - most commonly a directory holding several
// instances' datadirs side by side (a sandbox root, say), or a folder of
// old backups - could mean "extract everything found under here" pulls in
// far more than intended, so runSourceDir refuses it by default.
func classifySourceDir(dir string) (kind string, safe bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	hasIbdata1 := false
	hasSubdir := false
	for _, e := range entries {
		if e.IsDir() {
			hasSubdir = true
			continue
		}
		if e.Name() == "ibdata1" {
			hasIbdata1 = true
		}
	}
	switch {
	case hasIbdata1:
		return "a single instance's datadir (found ibdata1)", true, nil
	case !hasSubdir:
		return "a single database directory (no subdirectories of its own)", true, nil
	default:
		return "", false, nil
	}
}

// extractFileTables opens one .ibd file found under --source-dir and
// extracts every table its SDI (or, pre-8.0, its .frm) holds. tsvDump is
// nil for --format=sql (each table's schema+data .sql files are written
// straight into dir, named as writeSQLTable always names them). nSkipped
// counts tables silently left out because they belong to a system schema
// (see isSystemSchema) and --include-system-schemas wasn't given.
func extractFileTables(path, dir string, tsvDump *TSVDump) (nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped int, err error) {
	sp, err := OpenSpace(path)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	defer sp.Close()

	targets, mysqlVersionID, err := loadTargets(sp, path)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}

	for _, target := range targets {
		ok, tOK, tErr, tCorrupt, tTruncated, skippedSystem := processTable(sp, path, dir, target, mysqlVersionID, tsvDump)
		if skippedSystem {
			nSkipped++
			continue
		}
		if !ok {
			continue
		}
		nTables++
		nOK += tOK
		nErr += tErr
		nCorruptPages += tCorrupt
		if tTruncated {
			nTruncated++
		}
	}
	return nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped, nil
}

// extractMyISAMFileTable extracts the single table one .MYD file found
// under --source-dir describes - the MyISAM counterpart to extractFileTables.
// Unlike a .ibd (whose SDI can hold more than one table in a shared
// tablespace), a MyISAM data file always holds exactly one, so there's no
// per-file loop over multiple targets here.
func extractMyISAMFileTable(mydPath, dir string, tsvDump *TSVDump) (nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped int, err error) {
	raw, mysqlVersionID, err := findMyISAMSchema(mydPath)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	// Same "leave system schemas out of a whole-instance --format=tsv scan
	// by default" rule processTable applies for InnoDB - see isSystemSchema.
	if tsvDump != nil && !*includeSystemSchemas && isSystemSchema(raw.SchemaRef) {
		return 0, 0, 0, 0, 0, 1, nil
	}
	t, err := BuildMyISAMTable(raw)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("table %q: %w", raw.Name, err)
	}
	layout, err := buildMyISAMLayout(t)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("table %q: %w", t.Name, err)
	}
	if err := applyMYIHeader(mydPath, layout); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("table %q: %w", t.Name, err)
	}
	ok, tOK, tErr, tCorrupt, tTruncated := writeAndSummarizeTable(t, mydPath, dir, mysqlVersionID, tsvDump,
		func(outCols []*Column, format outputFormat) RowWalker {
			return myisamRowWalker(mydPath, t, layout, outCols, format, *skipCorrupted)
		})
	if !ok {
		return 0, 0, 0, 0, 0, 0, nil // already warned by writeAndSummarizeTable
	}
	nt := 0
	if tTruncated {
		nt = 1
	}
	return 1, tOK, tErr, tCorrupt, nt, 0, nil
}

// extractPartitionedTable extracts one partitioned InnoDB table found under
// --source-dir - every one of its partition files (files, all sharing
// base; see partition.go) - as a single table, the partitioned-table
// counterpart to extractFileTables.
func extractPartitionedTable(base string, files []string, dir string, tsvDump *TSVDump) (nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped int, err error) {
	pt, err := resolvePartitionedTable(base, files)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	if tsvDump != nil && !*includeSystemSchemas && isSystemSchema(pt.Schema) {
		return 0, 0, 0, 0, 0, 1, nil
	}
	op, err := openPartitionedTable(pt)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("table %q: %w", pt.Table, err)
	}
	defer op.Close()
	warnPartitionGaps(pt, op, base)
	ok, tOK, tErr, tCorrupt, tTruncated := writeAndSummarizeTable(op.Main, files[0], dir, pt.MySQLVersionID, tsvDump,
		func(outCols []*Column, format outputFormat) RowWalker {
			return op.rowWalker(format, *skipCorrupted, *deletedOnly)
		})
	if !ok {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("table %q wasn't written - see the warning above", pt.Table)
	}
	nt := 0
	if tTruncated {
		nt = 1
	}
	return 1, tOK, tErr, tCorrupt, nt, 0, nil
}

// processTable builds, decodes, and writes one already-resolved table
// target - the shared per-table work behind both extractFileTables' SDI/
// .frm targets and extractSharedFRMTables' shared-tablespace ones. srcLabel
// names the table's source file in warnings and (for --format=sql) the
// generated schema/data files' own header comment - the .ibd file itself
// for a file-per-table target, or ibdata1's own path for one resolved via
// its internal dictionary (see resolveSharedTablespaceTable, which
// --file/--table already uses this same convention for). ok is false if
// this table was skipped for any reason (already warned to stderr, unless
// skippedSystem is true, which needs no warning - it's the intended,
// silent default; see isSystemSchema).
func processTable(sp *Space, srcLabel, dir string, target *ddTableJSON, mysqlVersionID uint32, tsvDump *TSVDump) (ok bool, tOK, tErr, tCorrupt int, tTruncated, skippedSystem bool) {
	// This is a bulk instance-wide scan (--source-dir), not a deliberate
	// "give me this one table" ask - so, same as MySQL Shell's own
	// util.dumpInstance(), leave the server's own housekeeping schemas out
	// unless asked for them explicitly. Only applies to --format=tsv:
	// dumping db2.mytable via --file or --table is always exactly what
	// was asked for, regardless of schema name.
	if tsvDump != nil && !*includeSystemSchemas && isSystemSchema(target.SchemaRef) {
		return false, 0, 0, 0, false, true
	}
	t, berr := BuildTable(target)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "%s %s: table %q: %v\n", warn("warning:"), srcLabel, target.Name, berr)
		return false, 0, 0, 0, false, false
	}
	ok, tOK, tErr, tCorrupt, tTruncated = writeAndSummarizeTable(t, srcLabel, dir, mysqlVersionID, tsvDump,
		func(outCols []*Column, format outputFormat) RowWalker {
			return innodbRowWalker(sp, t, outCols, format, *skipCorrupted, *deletedOnly)
		})
	return ok, tOK, tErr, tCorrupt, tTruncated, false
}

// writeAndSummarizeTable is the shared "decode+write the table, then print
// one summary line" tail both engines' --source-dir table processing goes
// through once each has built its own *Table (processTable for InnoDB via
// BuildTable, processMyISAMTable for MyISAM via BuildMyISAMTable) -
// everything past that point (picking --format=sql vs tsv, writing the
// files, and the "  schema.table: N row(s)..." summary line) is identical
// either way. makeWalker builds the engine-specific RowWalker once outCols
// and the target format (sql vs tsv) are known.
func writeAndSummarizeTable(t *Table, srcLabel, dir string, mysqlVersionID uint32, tsvDump *TSVDump,
	makeWalker func(outCols []*Column, format outputFormat) RowWalker) (ok bool, tOK, tErr, tCorrupt int, tTruncated bool) {
	outCols := OutputColumns(t)
	if len(outCols) == 0 {
		fmt.Fprintf(os.Stderr, "%s %s: table %q has no columns this tool can output\n", warn("warning:"), srcLabel, t.Name)
		return false, 0, 0, 0, false
	}

	label := fmt.Sprintf("decoding %s.%s", t.SchemaRef, t.Name)
	var err error
	if tsvDump != nil {
		rw := makeWalker(outCols, formatTSV)
		ck, _ := parseCompressionKind(*compression) // already validated in run()
		tOK, tErr, tCorrupt, tTruncated, err = tsvDump.AddTable(rw, t, outCols, *ddlOnly, *limitRows, *skipCorrupted, *deletedOnly, label, mysqlVersionID, ck, *compressLevel)
	} else {
		rw := makeWalker(outCols, formatSQL)
		base := outputBaseName(t.SchemaRef, t.Name)
		schemaPath := filepath.Join(dir, base+"-schema.sql")
		dataPath := filepath.Join(dir, sqlDataFilename(base, *deletedOnly))
		wantPaths := []string{schemaPath}
		if !*ddlOnly {
			wantPaths = append(wantPaths, dataPath)
		}
		if err = confirmOverwrite(wantPaths, *yes || os.Getenv("YES") != ""); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: table %q: %v\n", warn("warning:"), srcLabel, t.Name, err)
			return false, 0, 0, 0, false
		}
		tOK, tErr, tCorrupt, tTruncated, err = writeSQLTable(rw, srcLabel, schemaPath, dataPath, t, outCols,
			*ddlOnly, *limitRows, *skipCorrupted, *deletedOnly)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: table %q: %v\n", warn("warning:"), srcLabel, t.Name, err)
		return false, 0, 0, 0, false
	}
	fmt.Printf("  %s: ", id(t.SchemaRef+"."+t.Name))
	if *ddlOnly {
		fmt.Printf("schema only\n")
	} else {
		rowNoun := "row(s)"
		if *deletedOnly {
			rowNoun = "delete-marked row(s)"
		}
		fmt.Printf("%s %s", good(fmt.Sprintf("%d", tOK)), rowNoun)
		if tErr > 0 {
			fmt.Printf(", %s skipped", warn(fmt.Sprintf("%d", tErr)))
		}
		if tCorrupt > 0 {
			fmt.Printf(", %s corrupted page(s) skipped", bad(fmt.Sprintf("%d", tCorrupt)))
		}
		if tTruncated {
			fmt.Printf(" %s", warn("[source file truncated]"))
		}
		fmt.Println()
	}
	return true, tOK, tErr, tCorrupt, tTruncated
}

// extractSharedFRMTables resolves and extracts every .frm file in frmPaths
// that has no .ibd of its own (innodb_file_per_table=0, the classic
// pre-8.0/MariaDB internal-dictionary case - see sysdict.go) against locs,
// sp's own full table listing (ListTableLocations). A .frm with no entry
// in locs - or one whose clustered index lives in some other tablespace
// entirely (a general tablespace this tool has no way to reach from here)
// - isn't necessarily a problem: plenty of real .frm files describe a
// non-InnoDB table (MyISAM, CSV, ...) or a VIEW, neither of which this
// tool can or needs to handle, so those are counted in nNotFound rather
// than warned about individually.
func extractSharedFRMTables(sp *Space, ibdataPath, dir string, frmPaths []string, locs map[string]*TableLocation, tsvDump *TSVDump) (nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped, nNotFound int) {
	for _, frmPath := range frmPaths {
		schema := filepath.Base(filepath.Dir(frmPath))
		table := strings.TrimSuffix(filepath.Base(frmPath), filepath.Ext(frmPath))
		loc, found := locs[schema+"/"+table]
		if !found || loc.SpaceID != sp.SpaceID {
			nNotFound++
			continue
		}
		target, err := parseFRM(frmPath, sp, loc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", warn("warning:"), frmPath, err)
			continue
		}
		ok, tOK, tErr, tCorrupt, tTruncated, skippedSystem := processTable(sp, ibdataPath, dir, target, 0, tsvDump)
		if skippedSystem {
			nSkipped++
			continue
		}
		if !ok {
			continue
		}
		nTables++
		nOK += tOK
		nErr += tErr
		nCorruptPages += tCorrupt
		if tTruncated {
			nTruncated++
		}
	}
	return nTables, nOK, nErr, nCorruptPages, nTruncated, nSkipped, nNotFound
}

// systemSchemas lists the schema names MySQL Shell's own util.dumpInstance()
// leaves out of a whole-instance dump by default. innodump mirrors that
// list for --source-dir --format=tsv, since that combination is the same
// "dump this whole server" use case, not a deliberate ask for one table.
var systemSchemas = map[string]bool{
	"mysql":              true,
	"sys":                true,
	"information_schema": true,
	"performance_schema": true,
	"ndbinfo":            true,
}

func isSystemSchema(schema string) bool {
	return systemSchemas[strings.ToLower(schema)]
}

// loadTargets returns every table definition sp's tablespace holds - every
// SDI table for an 8.0+ file (--source-dir always extracts all of them,
// unlike --file/--table's single pick - see pickTable), or the one table a
// pre-8.0 file's .frm describes - plus the tablespace's own SDI
// mysqld_version_id (0 for a .frm-derived table, which carries no such
// version at all; see TSVDump.noteSourceVersion in tsvdump.go).
func loadTargets(sp *Space, path string) ([]*ddTableJSON, uint32, error) {
	if !sp.Flags.sdi {
		frmPath, err := findLegacyFRM(path)
		if err != nil {
			return nil, 0, err
		}
		// --source-dir only ever finds a genuine file-per-table .ibd (it
		// matches by extension, so the shared/system tablespace itself -
		// ibdata1, no ".ibd" suffix - is never among them): loc stays nil,
		// same as the "root page 3" convention this has always used.
		target, err := parseFRM(frmPath, sp, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("reading %s: %w", frmPath, err)
		}
		return []*ddTableJSON{target}, 0, nil
	}

	rawTables, sdiInfo, err := LoadSDITables(sp)
	if err != nil {
		return nil, 0, fmt.Errorf("reading SDI: %w", err)
	}
	if len(rawTables) == 0 {
		return nil, 0, fmt.Errorf("no table definitions found in this tablespace's SDI")
	}
	parsed := make([]*ddTableJSON, 0, len(rawTables))
	for _, raw := range rawTables {
		var dt ddTableJSON
		if err := json.Unmarshal(raw, &dt); err != nil {
			return nil, 0, fmt.Errorf("parsing SDI table JSON: %w", err)
		}
		parsed = append(parsed, &dt)
	}
	return parsed, sdiInfo.MySQLVersionID, nil
}

// findLegacyFRM looks for the .frm file a pre-8.0 .ibd file's schema must
// come from, right next to it (table.ibd -> table.frm in the same
// directory - the two are always siblings on a real server, since MySQL
// itself requires that layout).
func findLegacyFRM(ibdPath string) (string, error) {
	dir := filepath.Dir(ibdPath)
	base := strings.TrimSuffix(filepath.Base(ibdPath), filepath.Ext(ibdPath))
	frmPath := filepath.Join(dir, base+".frm")
	if _, err := os.Stat(frmPath); err != nil {
		return "", fmt.Errorf("this tablespace has no SDI (a pre-8.0 MySQL 5.6/5.7 file), and no matching %s was found next to it - "+
			"place the table's .frm file in the same directory as the .ibd file (MariaDB's .frm format differs and isn't supported)", filepath.Base(frmPath))
	}
	return frmPath, nil
}

// resolveSharedTablespaceTable builds the ddTableJSON for one table living
// in the shared/system tablespace sp (see sysdict.go's package comment).
// want is --table's value: "schema.table" or "schema/table" (InnoDB's own
// dictionary form) - required here, since one shared tablespace can hold
// many schemas' tables; an empty want (or one not found) fails with a
// listing of every table this tablespace's own dictionary actually holds.
func resolveSharedTablespaceTable(sp *Space, ibdataPath, want string) (*ddTableJSON, error) {
	if want == "" {
		_, err := FindTableLocation(sp, "")
		return nil, err
	}
	loc, err := FindTableLocation(sp, want)
	if err != nil {
		return nil, err
	}
	if loc.SpaceID != sp.SpaceID {
		return nil, fmt.Errorf("table %q isn't actually stored in this tablespace - its internal dictionary entry points at tablespace id %d, "+
			"not this one (id %d); point --file at that table's own .ibd (or shared tablespace) file instead", want, loc.SpaceID, sp.SpaceID)
	}

	schemaTable := strings.Replace(want, ".", "/", 1)
	parts := strings.SplitN(schemaTable, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf(`--table must be "schema.table" (or "schema/table") for a shared tablespace, not %q`, want)
	}
	frmPath := filepath.Join(filepath.Dir(ibdataPath), parts[0], parts[1]+".frm")
	if _, err := os.Stat(frmPath); err != nil {
		return nil, fmt.Errorf("found %q in this tablespace's internal dictionary (root page %d), but its .frm file is missing - "+
			"expected it at %s (a table's .frm always sits in its schema's own directory, a sibling of the shared tablespace file, "+
			"regardless of where its InnoDB data physically lives)", want, loc.RootPage, frmPath)
	}
	target, err := parseFRM(frmPath, sp, loc)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", frmPath, err)
	}
	return target, nil
}

func printSpaceInfo(sp *Space, path string) {
	fi, err := os.Stat(path)
	size := int64(-1)
	if err == nil {
		size = fi.Size()
	}
	fmt.Printf("%s %s (%d bytes)\n", dim("File:       "), val(path), size)
	fmt.Printf("%s %s\n", dim("Space ID:   "), val(fmt.Sprintf("%d", sp.SpaceID)))
	fmt.Printf("%s %s\n", dim("Pages:      "), val(fmt.Sprintf("%d", sp.NumPages)))
	if sp.Compressed {
		fmt.Printf("%s %s bytes physical (KEY_BLOCK_SIZE=%d), %d bytes logical\n",
			dim("Page size:  "), val(fmt.Sprintf("%d", sp.PhysPageSize)), sp.PhysPageSize/1024, sp.PageSize)
	} else {
		fmt.Printf("%s %s\n", dim("Page size:  "), val(fmt.Sprintf("%d bytes", sp.PageSize)))
	}
	f := sp.Flags
	fmt.Printf("%s %s (post_antelope=%v atomic_blobs=%v data_dir=%v shared=%v temporary=%v encryption=%v sdi=%v)\n",
		dim("FSP flags:  "), val(fmt.Sprintf("0x%08x", f.raw)), f.postAntelope, f.atomicBlobs, f.dataDir, f.shared, f.temporary, f.encryption, f.sdi)
}

// formatMySQLVersion renders a dd_object's numeric mysqld_version_id
// (MMmmpp, e.g. 80046) the way MySQL itself reports it (e.g. "8.0.46").
func formatMySQLVersion(id uint32) string {
	if id == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", id/10000, (id/100)%100, id%100)
}

// runTimestamp is computed once per run (not per call to defaultOutDir), so
// every default-named output directory a single invocation writes uses the
// same timestamp even if it straddles a minute boundary.
var runTimestamp = time.Now().Format("2006-01-02_15.04")

// defaultOutDir builds --out-dir's default when none is given: "sqldump" or
// "tsvdump" (matching --format), suffixed with this run's own timestamp
// (date +%Y-%m-%d_%H.%M) so repeated runs land in their own directory
// instead of colliding or silently overwriting one another.
func defaultOutDir(format string) string {
	base := "sqldump"
	if format == "tsv" {
		base = "tsvdump"
	}
	return base + "_" + runTimestamp
}

// outputBaseName builds the "schema.table" base name output files are named
// after. schemaRef is usually already filesystem-safe (it's a real database
// name), but "/" is replaced defensively since it would otherwise be read as
// a directory separator by filepath.Join.
func outputBaseName(schemaRef, table string) string {
	safe := func(s string) string { return strings.ReplaceAll(s, "/", "_") }
	if schemaRef == "" {
		return safe(table)
	}
	return safe(schemaRef) + "." + safe(table)
}

// sqlDataFilename builds the "-data.sql" file's own basename from base (see
// outputBaseName): plain "<base>-data.sql", or "<base>-data-deleted.sql"
// with --deleted-only, so a deleted-rows dump never silently overwrites (or
// gets overwritten by) a normal one written to the same output directory.
func sqlDataFilename(base string, deletedOnly bool) string {
	if deletedOnly {
		return base + "-data-deleted.sql"
	}
	return base + "-data.sql"
}

// confirmOverwrite checks paths for ones that already exist and, unless yes
// is set, asks before letting the caller overwrite them: interactively (a y
// /N prompt on stdin) when stdin is a real terminal, or by refusing outright
// otherwise - a non-interactive run (e.g. a script looping over many tables)
// has no one to answer a prompt, so silently clobbering an existing file
// would be the wrong default and hanging on a read that never completes
// would be worse.
func confirmOverwrite(paths []string, yes bool) error {
	if yes {
		return nil
	}
	var existing []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	if !isTTY(os.Stdin) {
		return fmt.Errorf("output file(s) already exist: %s (pass --yes, or set YES=1, to overwrite)", strings.Join(existing, ", "))
	}
	fmt.Printf("The following output file(s) already exist:\n")
	for _, p := range existing {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("Overwrite? [y/N]: ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer != "y" && answer != "Y" && !strings.EqualFold(answer, "yes") {
		return fmt.Errorf("not overwriting existing output file(s): %s", strings.Join(existing, ", "))
	}
	return nil
}

// pickTable chooses which SDI table entry to extract when a tablespace's
// SDI (unusually) holds more than one, preferring an exact name match
// against --table or the .ibd file's own base name.
func pickTable(tables []*ddTableJSON, ibdPath, want string) (*ddTableJSON, error) {
	if len(tables) == 1 && want == "" {
		return tables[0], nil
	}
	if want == "" {
		want = strings.TrimSuffix(filepath.Base(ibdPath), filepath.Ext(ibdPath))
	}
	var names []string
	for _, t := range tables {
		names = append(names, t.Name)
		if strings.EqualFold(t.Name, want) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("this tablespace's SDI holds %d tables (%s); pass --table to pick one",
		len(tables), strings.Join(names, ", "))
}

func debugDumpPage(sp *Space, pageNo uint32) error {
	page, err := sp.ReadPage(pageNo)
	if err != nil {
		return err
	}
	fmt.Printf("page %d: type=%d level=%d nrecs=%d indexID=%d next=%d prev=%d\n",
		pageNo, filType(page), pageGetLevel(page), pageGetNRecs(page), pageGetIndexID(page), filNextPage(page), filPrevPage(page))
	fmt.Printf("infimum offset=%d supremum offset=%d\n", pageNewInfimum, pageNewSupremum)
	off := recNextOffset(page, pageNewInfimum, sp.PageSize)
	n := 0
	for off != 0 && off != pageNewSupremum && n < 20 {
		fmt.Printf("--- record at offset %d (0x%x) ---\n", off, off)
		lo := off - 16
		hi := off + 32
		if hi > uint32(len(page)) {
			hi = uint32(len(page))
		}
		for p := lo; p < hi; p += 16 {
			e := p + 16
			if e > hi {
				e = hi
			}
			fmt.Printf("  %04x: ", p)
			for k := p; k < e; k++ {
				marker := " "
				if k == off {
					marker = ">"
				}
				fmt.Printf("%02x%s", page[k], marker)
			}
			fmt.Println()
		}
		fmt.Printf("  status=%d deleted=%v info=0x%02x\n", recStatus(page, off), recDeleted(page, off), recInfoBits(page, off))
		off = recNextOffset(page, off, sp.PageSize)
		n++
	}
	return nil
}
