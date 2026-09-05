// ibd-extractor - offline schema+data extraction from a MySQL 8.0/8.4, or
// 5.6/5.7, InnoDB .ibd file.
//
// Given table.ibd, produces table-schema.sql (a best-effort CREATE TABLE,
// reconstructed from the tablespace's embedded SDI, or its .frm file for a
// pre-8.0 table - see frm.go) and table-data.sql (one INSERT statement per
// live row, decoded by walking the clustered index's B+tree directly out
// of the file - no server involved).
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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	appName = "ibd-extractor"
	version = "0.4.0"
)

var (
	filePath      = flag.String("file", "", "Path to the .ibd file to extract")
	outDir        = flag.String("out-dir", "", "Directory for the output .sql files (default: the current directory)")
	tableName     = flag.String("table", "", "Table name to extract, if the SDI holds more than one")
	limitRows     = flag.Int("limit", 0, "Stop after this many rows (0 = all)")
	ddlOnly       = flag.Bool("ddl-only", false, "Only write the schema file; skip walking the table's data entirely")
	skipCorrupted = flag.Bool("skip-corrupted", false, "On a corrupted leaf page, note it and try to carry on from the next page instead of stopping")
	noProgress    = flag.Bool("no-progress", false, "Never draw the progress bar on stderr (also honored: NO_PROGRESS=1)")
	verbose       = flag.Bool("verbose", false, "Print extra .ibd/table details: page size, FSP flags, the MySQL version and dictionary/SDI versions that wrote the file, index/column counts")
	showVersion   = flag.Bool("version", false, "Print version and exit")
	dumpPage      = flag.Int("dump-page", -1, "debug: hex-dump one page and its record chain, then exit")
	debug         = flag.Bool("debug", false, "debug: print each record's decoded field byte-ranges as they're read")
)

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		return
	}
	if *filePath == "" {
		fmt.Printf("%s %s\n", appName, version)
		fmt.Printf("Usage: %s --file /path/to/table.ibd [--out-dir DIR] [--table NAME] [--limit N]\n", appName)
		fmt.Println("       [--ddl-only] [--skip-corrupted] [--no-progress] [--verbose] [--debug] [--dump-page N] [--version]")
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	sp, err := OpenSpace(*filePath)
	if err != nil {
		return err
	}
	defer sp.Close()

	debugFields = *debug
	initProgress(*noProgress)
	if *verbose {
		printSpaceInfo(sp, *filePath)
	}
	if *dumpPage >= 0 {
		return debugDumpPage(sp, uint32(*dumpPage))
	}

	var target *ddTableJSON
	if !sp.Flags.sdi {
		// Pre-8.0: no SDI at all - fall back to the .frm file that must
		// sit alongside the .ibd (see findLegacyFRM).
		frmPath, err := findLegacyFRM(*filePath)
		if err != nil {
			return err
		}
		if *verbose {
			fmt.Printf("SDI:         absent (pre-8.0 tablespace) - using %s\n", frmPath)
		}
		target, err = parseFRM(frmPath, sp)
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
		if *verbose {
			fmt.Printf("SDI:         present (written by MySQL %s, dd_version %d, sdi_version %d)\n",
				formatMySQLVersion(sdiInfo.MySQLVersionID), sdiInfo.DDVersion, sdiInfo.SDIVersion)
			fmt.Printf("Tables in SDI: %d\n", len(rawTables))
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
		fmt.Printf("Row format:  %s\n", rowFormatName(t.RowFormat))
		fmt.Printf("Clustered index: %q (id %d), root page %d\n", t.IndexName, t.IndexID, t.RootPage)
		fmt.Printf("Secondary indexes: %d\n", len(t.SecondaryIndexes))
		fmt.Printf("Physical fields: %d (%d output)\n", len(t.PhysicalFields), len(OutputColumns(t)))
	}

	base := strings.TrimSuffix(filepath.Base(*filePath), filepath.Ext(*filePath))
	dir := *outDir
	if dir == "" {
		dir = "."
	}
	schemaPath := filepath.Join(dir, base+"-schema.sql")
	dataPath := filepath.Join(dir, base+"-data.sql")

	outCols := OutputColumns(t)
	if len(outCols) == 0 {
		return fmt.Errorf("table %q has no columns this tool can output", t.Name)
	}

	var nOK, nErr, nCorruptPages int
	if *ddlOnly {
		fmt.Printf("%s %s\n", appName, version)
		fmt.Printf("Table:       %s.%s (row_format=%s)\n", t.SchemaRef, t.Name, rowFormatName(t.RowFormat))
		fmt.Printf("Columns:     %d output (%d total incl. system/hidden)\n", len(outCols), len(t.Columns))
	} else {
		autoIncIdx := -1
		for i, c := range outCols {
			if c == t.AutoIncrementCol {
				autoIncIdx = i
			}
		}

		f, err := os.Create(dataPath)
		if err != nil {
			return fmt.Errorf("writing %s: %w", dataPath, err)
		}
		defer f.Close()

		fmt.Fprintf(f, "-- Decoded by ibd-extractor from %s, table %s.%s\n", filepath.Base(*filePath), t.SchemaRef, t.Name)
		prefix := InsertPrefix(t, outCols)

		var maxAutoInc uint64
		onCorrupt := func(cp CorruptPage) {
			nCorruptPages++
			fmt.Fprintf(os.Stderr, "warning: corrupted page %d (index %q, id %d): %s\n", cp.PageNo, t.IndexName, t.IndexID, cp.Reason)
			fmt.Fprintf(f, "-- skipped corrupted page %d (table %s.%s, index %q, id %d): %s\n",
				cp.PageNo, t.SchemaRef, t.Name, t.IndexName, t.IndexID, cp.Reason)
		}
		// total is a rough proxy only: not every page in the file belongs to
		// this index, so the bar may not reach 100% on a small table sharing
		// a big tablespace, and rarely (page splits/allocation order) the
		// leaf chain can visit a lower page number after a higher one. Either
		// way it still gives a fair sense of progress on a large extraction.
		pr := newProg(fmt.Sprintf("decoding %s.%s", t.SchemaRef, t.Name), int64(sp.NumPages))
		defer pr.finish()
		walkErr := WalkRows(sp, t, outCols, *skipCorrupted, onCorrupt, func(roe RowOrError) bool {
			pr.set(int64(roe.PageNo))
			if roe.Err != nil {
				nErr++
				fmt.Fprintf(os.Stderr, "warning: page %d rec@0x%x: %v\n", roe.PageNo, roe.RecOff, roe.Err)
				fmt.Fprintf(f, "-- skipped a row at page %d rec@0x%x: %v\n", roe.PageNo, roe.RecOff, roe.Err)
				return *limitRows == 0 || nOK+nErr < *limitRows
			}
			fmt.Fprintf(f, "%s%s;\n", prefix, FormatRow(roe.Row))
			if autoIncIdx >= 0 {
				if v, err := strconv.ParseUint(roe.Row.Values[autoIncIdx], 10, 64); err == nil && v > maxAutoInc {
					maxAutoInc = v
				}
			}
			nOK++
			pr.setExtra(int64(nOK))
			return *limitRows == 0 || nOK < *limitRows
		})
		pr.finish()
		if walkErr != nil {
			return fmt.Errorf("walking table rows: %w", walkErr)
		}

		// The SDI carries no persisted auto-increment counter (see
		// Table.AutoIncrementNext in schema.go); approximate it from the data.
		if t.AutoIncrementCol != nil {
			next := maxAutoInc + 1
			t.AutoIncrementNext = &next
		}

		fmt.Printf("%s %s\n", appName, version)
		fmt.Printf("Table:       %s.%s (row_format=%s)\n", t.SchemaRef, t.Name, rowFormatName(t.RowFormat))
		fmt.Printf("Columns:     %d output (%d total incl. system/hidden)\n", len(outCols), len(t.Columns))
	}

	if err := os.WriteFile(schemaPath, []byte(GenerateDDL(t)), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", schemaPath, err)
	}
	fmt.Printf("Schema file: %s\n", schemaPath)

	if *ddlOnly {
		return nil
	}
	fmt.Printf("Data file:   %s (%d row(s) written", dataPath, nOK)
	if nErr > 0 {
		fmt.Printf(", %d row(s) skipped - see warnings above", nErr)
	}
	if nCorruptPages > 0 {
		fmt.Printf(", %d corrupted page(s) skipped - see warnings above", nCorruptPages)
	}
	fmt.Printf(")\n")
	return nil
}

// pickTable chooses which SDI table entry to extract when a tablespace's
// SDI (unusually) holds more than one, preferring an exact name match
// against --table or the .ibd file's own base name.
// printSpaceInfo prints --verbose's file/tablespace-level details: the
// stuff readable straight from the FSP header on page 0, before any
// SDI/dictionary parsing has happened (so it's useful even when that part
// fails or the file predates SDI entirely).
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

func printSpaceInfo(sp *Space, path string) {
	fi, err := os.Stat(path)
	size := int64(-1)
	if err == nil {
		size = fi.Size()
	}
	fmt.Printf("File:        %s (%d bytes)\n", path, size)
	fmt.Printf("Space ID:    %d\n", sp.SpaceID)
	fmt.Printf("Pages:       %d\n", sp.NumPages)
	if sp.Compressed {
		fmt.Printf("Page size:   %d bytes physical (KEY_BLOCK_SIZE=%d), %d bytes logical\n",
			sp.PhysPageSize, sp.PhysPageSize/1024, sp.PageSize)
	} else {
		fmt.Printf("Page size:   %d bytes\n", sp.PageSize)
	}
	f := sp.Flags
	fmt.Printf("FSP flags:   0x%08x (post_antelope=%v atomic_blobs=%v data_dir=%v shared=%v temporary=%v encryption=%v sdi=%v)\n",
		f.raw, f.postAntelope, f.atomicBlobs, f.dataDir, f.shared, f.temporary, f.encryption, f.sdi)
}

// formatMySQLVersion renders a dd_object's numeric mysqld_version_id
// (MMmmpp, e.g. 80046) the way MySQL itself reports it (e.g. "8.0.46").
func formatMySQLVersion(id uint32) string {
	if id == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", id/10000, (id/100)%100, id%100)
}

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
