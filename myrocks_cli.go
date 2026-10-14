// myrocks_cli.go: the --file and --source-dir entry points for MyRocks
// tables (see myrocks.go).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// myrocksTarget is what --file pointed at, MyRocks-wise.
type myrocksTarget struct {
	RocksDir string // the RocksDB directory (.rocksdb)
	Datadir  string // where the schema directories (.sdi/.frm) live
	Want     string // "schema.table" named by the file itself (.sdi/.frm), or ""
}

// isMyRocksFileArg reports whether --file names something MyRocks: a
// RocksDB directory (or a file inside one), a datadir holding one, or a
// ROCKSDB table's .sdi/.frm.
func isMyRocksFileArg(path string) (myrocksTarget, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return myrocksTarget{}, false
	}
	if st.IsDir() {
		if looksLikeRocksDBDir(path) {
			return myrocksTarget{RocksDir: path, Datadir: filepath.Dir(filepath.Clean(path))}, true
		}
		if d := findRocksDBDir(path); d != "" {
			return myrocksTarget{RocksDir: d, Datadir: path}, true
		}
		if *rocksdbDir != "" {
			return myrocksTarget{RocksDir: *rocksdbDir, Datadir: path}, true
		}
		return myrocksTarget{}, false
	}
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".sst") || strings.HasPrefix(name, "MANIFEST-") || name == "CURRENT" ||
		strings.HasPrefix(name, "OPTIONS-") || name == "IDENTITY" || (strings.HasSuffix(lower, ".log") && isAllDigits(strings.TrimSuffix(name, ".log"))):
		if looksLikeRocksDBDir(dir) {
			return myrocksTarget{RocksDir: dir, Datadir: filepath.Dir(dir)}, true
		}
	case strings.HasSuffix(lower, ".sdi"):
		raw, _, err := loadStandaloneSDI(path)
		if err != nil || !strings.EqualFold(raw.Engine, "ROCKSDB") {
			return myrocksTarget{}, false
		}
		return myrocksSchemaFileTarget(dir, raw.SchemaRef+"."+raw.Name), true
	case strings.HasSuffix(lower, ".frm"):
		def, err := readFRMDefinition(path)
		if err != nil || !def.engineIsRocksDB() {
			return myrocksTarget{}, false
		}
		return myrocksSchemaFileTarget(dir, def.table.SchemaRef+"."+def.table.Name), true
	}
	return myrocksTarget{}, false
}

func myrocksSchemaFileTarget(schemaDir, want string) myrocksTarget {
	datadir := filepath.Dir(filepath.Clean(schemaDir))
	rd := *rocksdbDir
	if rd == "" {
		rd = findRocksDBDir(datadir)
	}
	return myrocksTarget{RocksDir: rd, Datadir: datadir, Want: want}
}

// openMyRocks opens dir and its dictionary, printing what opening it found.
func openMyRocks(dir, datadir string) (*rdbDB, *mrDict, error) {
	if dir == "" {
		return nil, nil, fmt.Errorf("no MyRocks data directory (.rocksdb/ or #rocksdb/) found next to the schema directories - pass --rocksdb-dir if rocksdb_datadir pointed elsewhere")
	}
	db, err := openRocksDB(dir)
	if err != nil {
		return nil, nil, err
	}
	resolvePreparedFromBinlog(db, datadir)
	printRocksDBProblems(db)
	dict, err := loadMyRocksDict(db)
	if err == errMyRocksNoTables {
		return db, dict, nil // an empty dictionary: zero tables, not an error
	}
	if err != nil {
		if len(findMyRocksSchemaFiles(datadir)) == 0 {
			db.Close()
			return nil, nil, err
		}
		// No dictionary at all, but table definitions on disk: try to
		// find their rows anyway.
		fmt.Fprintf(os.Stderr, "%s %v\n", warn("warning:"), err)
		dict = &mrDict{AutoInc: map[uint64]uint64{}, Dropping: map[uint64]bool{}, CFFlags: map[uint32]uint32{}}
	}
	recoverOrphanTables(db, dict, datadir)
	return db, dict, nil
}

// printRocksDBProblems prints, once per run, what openRocksDB had to work
// around.
func printRocksDBProblems(db *rdbDB) {
	for _, p := range db.Problems {
		if !p.Data && !*verbose {
			continue
		}
		label := warn("warning:")
		if !p.Data {
			label = dim("note:")
		}
		fmt.Fprintf(os.Stderr, "%s RocksDB %s: %s\n", label, p.Where, p.What)
	}
	if !db.ManifestOK {
		fmt.Fprintf(os.Stderr, "%s RocksDB: the MANIFEST couldn't be fully read, so every .sst file present is treated as live - rows deleted long ago could reappear if a crash left obsolete files behind\n", warn("warning:"))
	}
	if len(db.Orphans) > 0 && *verbose {
		fmt.Fprintf(os.Stderr, "%s RocksDB: %d .sst file(s) not in the MANIFEST (obsolete, not yet deleted) were ignored: %s\n",
			dim("note:"), len(db.Orphans), strings.Join(db.Orphans, ", "))
	}
}

// pickMyRocksTable finds want ("schema.table", or a bare table name if
// unique) among tables.
func pickMyRocksTable(tables []*mrLogicalTable, want string) (*mrLogicalTable, error) {
	var hits []*mrLogicalTable
	for _, lt := range tables {
		if strings.EqualFold(lt.fullName(), want) || (!strings.Contains(want, ".") && strings.EqualFold(lt.Table, want)) ||
			strings.EqualFold(lt.Schema+"/"+lt.Table, want) {
			hits = append(hits, lt)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("no MyRocks table %q in this instance's dictionary - %s", want, myrocksTableList(tables))
	}
	return nil, fmt.Errorf("%q matches more than one MyRocks table - name it as schema.table; %s", want, myrocksTableList(tables))
}

func myrocksTableList(tables []*mrLogicalTable) string {
	var names []string
	for _, lt := range tables {
		if !isSystemSchema(lt.Schema) {
			names = append(names, lt.fullName())
		}
	}
	if len(names) == 0 {
		return "it lists no user tables"
	}
	if len(names) > 40 {
		return fmt.Sprintf("it lists %d tables, e.g. %s, ...", len(names), strings.Join(names[:40], ", "))
	}
	return "it lists: " + strings.Join(names, ", ")
}

// prepareMyRocksTable resolves one logical table's schema and per-
// partition layouts.
func prepareMyRocksTable(db *rdbDB, dict *mrDict, datadir string, lt *mrLogicalTable) (t *Table, lays []*mrLayout, versionID uint32, partDesc string, err error) {
	raw, versionID, schemaPath, err := findMyRocksSchema(datadir, lt)
	if err != nil {
		return nil, nil, 0, "", err
	}
	rules := mrRules80
	if strings.HasSuffix(strings.ToLower(schemaPath), ".frm") {
		rules = mrRules57
	}
	defs := lt.Parts
	if raw.PartitionType != 0 {
		ordered, desc, err := orderMyRocksPartitions(raw, lt, datadir)
		if err != nil {
			return nil, nil, 0, "", err
		}
		defs, partDesc = ordered, desc
	} else if lt.Partitioned {
		return nil, nil, 0, "", fmt.Errorf("the MyRocks dictionary lists partitions of %s, but its definition isn't partitioned", lt.fullName())
	}
	if len(defs) == 0 {
		return nil, nil, 0, "", fmt.Errorf("no partition of %s has an entry in the MyRocks dictionary", lt.fullName())
	}

	_, pkPos, ok := defs[0].pkIndex()
	if !ok {
		return nil, nil, 0, "", fmt.Errorf("the MyRocks dictionary lists no primary index for %s", lt.fullName())
	}
	if defs[0].Indexes[pkPos].Type == mrIndexHiddenPrimary {
		pkPos = -1
	}
	t, err = BuildRocksDBTable(raw, pkPos)
	if err != nil {
		return nil, nil, 0, "", err
	}
	if raw.PartitionType != 0 {
		if raw.frmPartitionClause != "" {
			t.PartitionClause = frmWrapPartitionClause(raw.frmPartitionClause)
		} else if clause, cerr := sdiPartitionClause(raw); cerr == nil {
			t.PartitionClause = clause
		} else {
			return nil, nil, 0, "", cerr
		}
	}
	for _, def := range defs {
		ix, _, ok := def.pkIndex()
		if !ok {
			return nil, nil, 0, "", fmt.Errorf("the MyRocks dictionary lists no primary index for %s", def.RawName)
		}
		if dict.Dropping[ix.gl()] {
			return nil, nil, 0, "", fmt.Errorf("%s's primary index %d is being dropped (DROP/ALTER in progress)", def.RawName, ix.ID)
		}
		lay, err := buildMyRocksLayout(t, raw, ix, def, db, rules)
		if err != nil {
			return nil, nil, 0, "", fmt.Errorf("%s: %w", def.RawName, err)
		}
		lays = append(lays, lay)
		if v := dict.AutoInc[ix.gl()]; v > t.AutoIncrementStored && t.AutoIncrementCol != nil {
			t.AutoIncrementStored = v
		}
	}
	if t.AutoIncrementStored > 0 {
		v := t.AutoIncrementStored
		t.AutoIncrementNext = &v
	}
	return t, lays, versionID, partDesc, nil
}

// orderMyRocksPartitions puts a partitioned table's dictionary entries in
// the partition order its own definition lists.
func orderMyRocksPartitions(raw *ddTableJSON, lt *mrLogicalTable, datadir string) ([]*mrTableDef, string, error) {
	bySuffix := map[string]*mrTableDef{}
	for _, d := range lt.Parts {
		bySuffix[strings.ToLower(d.Partition)] = d
	}
	var out []*mrTableDef
	var names, missing []string
	add := func(suffix, name string) {
		names = append(names, name)
		if d, ok := bySuffix[strings.ToLower(suffix)]; ok {
			out = append(out, d)
			delete(bySuffix, strings.ToLower(suffix))
		} else {
			missing = append(missing, name)
		}
	}
	for _, p := range raw.Partitions {
		pn := mysqlFilenameEncode(p.Name)
		if len(p.Subpartitions) == 0 {
			add("p#"+pn, p.Name)
			continue
		}
		for _, sp := range p.Subpartitions {
			add("p#"+pn+"#sp#"+mysqlFilenameEncode(sp.Name), p.Name+"."+sp.Name)
		}
	}
	if len(raw.Partitions) == 0 {
		// A .frm-described table: its partitions, in order, come from the
		// .par file (5.6-style generic partitioning, MariaDB) or the
		// clause itself (5.7) - see partition.go.
		leaves, err := parsePARFile(filepath.Join(datadir, lt.DBDir, lt.TableBase+".par"))
		if err != nil {
			leaves = frmClausePartitionNames(raw.frmPartitionClause)
		}
		for _, l := range leaves {
			add("p#"+l, strings.ReplaceAll(strings.ReplaceAll(l, "#SP#", "."), "#sp#", "."))
		}
		if len(leaves) == 0 {
			for _, d := range lt.Parts {
				out = append(out, d)
				names = append(names, d.Partition)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Partition < out[j].Partition })
			return out, fmt.Sprintf("%d (in the MyRocks dictionary's order)", len(out)), nil
		}
	}
	for _, m := range missing {
		fmt.Fprintf(os.Stderr, "%s %s: partition %s has no entry in the MyRocks dictionary - its rows are missing from this dump\n", warn("warning:"), lt.fullName(), m)
	}
	for s := range bySuffix {
		fmt.Fprintf(os.Stderr, "%s %s: the MyRocks dictionary lists partition %q, which isn't in the table's own definition - left out\n", warn("warning:"), lt.fullName(), s)
	}
	return out, fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", ")), nil
}

// runMyRocksFile is --file's MyRocks path.
func runMyRocksFile(tgt myrocksTarget) error {
	if *dumpPage >= 0 {
		return fmt.Errorf("--dump-page applies to InnoDB tablespaces, not MyRocks")
	}
	if *rocksdbDir != "" {
		tgt.RocksDir = *rocksdbDir
	}
	want := *tableName
	if want == "" {
		want = tgt.Want
	} else if tgt.Want != "" && !strings.EqualFold(want, tgt.Want) {
		return fmt.Errorf("--table %s doesn't match the table %s describes", want, *filePath)
	}

	db, dict, err := openMyRocks(tgt.RocksDir, tgt.Datadir)
	if err != nil {
		return err
	}
	defer db.Close()
	tables := dict.logicalTables()
	if len(tables) == 0 && *rocksdbIndex == 0 {
		return fmt.Errorf("%s: %w", tgt.RocksDir, errMyRocksNoTables)
	}

	if want == "" {
		var user []*mrLogicalTable
		for _, lt := range tables {
			if !isSystemSchema(lt.Schema) {
				user = append(user, lt)
			}
		}
		if len(user) != 1 {
			return fmt.Errorf("%s holds every MyRocks table of the instance - name one with --table schema.table (or use --source-dir to extract them all); %s",
				tgt.RocksDir, myrocksTableList(tables))
		}
		want = user[0].fullName()
	}
	lt, err := pickMyRocksTable(tables, want)
	if err != nil && *rocksdbIndex != 0 {
		// Not in the dictionary at all: take the name from the schema files.
		for _, sf := range findMyRocksSchemaFiles(tgt.Datadir) {
			c := &mrLogicalTable{DBDir: sf.DBDir, TableBase: sf.TableBase, Schema: decodeMySQLFilename(sf.DBDir), Table: decodeMySQLFilename(sf.TableBase)}
			if l, e := pickMyRocksTable([]*mrLogicalTable{c}, want); e == nil {
				lt, err = l, nil
				break
			}
		}
	}
	if err != nil {
		return err
	}
	if *rocksdbIndex != 0 {
		if err := forceMyRocksIndex(db, dict, tgt.Datadir, lt, uint32(*rocksdbIndex)); err != nil {
			return fmt.Errorf("--rocksdb-index %d: %w", *rocksdbIndex, err)
		}
	}
	if *verbose {
		printRocksDBInfo(db)
	}
	t, lays, versionID, partDesc, err := prepareMyRocksTable(db, dict, tgt.Datadir, lt)
	if err != nil {
		return fmt.Errorf("table %s: %w", lt.fullName(), err)
	}
	if *verbose {
		printMyRocksTableInfo(t, lays)
	}
	stats := &mrWalkStats{}
	err = writeSingleTable(t, tgt.RocksDir, versionID, partDesc, func(outCols []*Column, format outputFormat) RowWalker {
		return myrocksRowWalker(db, lays, outCols, format, *skipCorrupted, *deletedOnly, stats)
	})
	if err != nil {
		return err
	}
	printMyRocksNotes(stats)
	return nil
}

func printMyRocksNotes(stats *mrWalkStats) {
	if stats.Stale > 0 {
		fmt.Printf("%s %d row(s) lie in the key range of a damaged block or missing file, so a newer version of them may have been lost - they're written as found, but may be out of date (or deleted rows brought back)\n",
			warn("Note:       "), stats.Stale)
	}
	if stats.Expired > 0 {
		fmt.Printf("%s %d row(s) past their TTL were left out, as the server's own reads hide them until compaction removes them\n",
			dim("Note:       "), stats.Expired)
	}
}

func printRocksDBInfo(db *rdbDB) {
	fmt.Printf("%s %s\n", dim("RocksDB dir:"), val(db.Dir))
	if db.ManifestPath != "" {
		state := "replayed"
		if !db.ManifestOK {
			state = "damaged - every .sst file present treated as live"
		}
		fmt.Printf("%s %s (%s, last sequence %d)\n", dim("MANIFEST:   "), val(filepath.Base(db.ManifestPath)), state, db.LastSeq)
	}
	var ids []int
	for id := range db.CFs {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		cf := db.CFs[uint32(id)]
		order := ""
		if cf.Reverse {
			order = ", reverse order"
		}
		fmt.Printf("%s %s\n", dim("Column family:"), val(fmt.Sprintf("%d %q: %d live .sst file(s), %d unflushed WAL entr(ies)%s", cf.ID, cf.Name, len(cf.SSTs), len(cf.mem), order)))
	}
	if len(db.WALFiles) > 0 {
		fmt.Printf("%s %s\n", dim("WAL files:  "), val(strings.Join(db.WALFiles, ", ")))
	}
}

func printMyRocksTableInfo(t *Table, lays []*mrLayout) {
	for _, lay := range lays {
		ix := lay.Index
		name := "PRIMARY"
		if lay.HiddenPK {
			name = "hidden primary key"
		}
		extra := ""
		if lay.TTL {
			extra = fmt.Sprintf(", TTL %ds", ix.TTL)
		}
		fmt.Printf("%s %s\n", dim("Primary index:"), val(fmt.Sprintf("%s: %s, index number %d in column family %d %q, kv_format_version %d%s",
			lay.Def.RawName, name, ix.ID, ix.CF, lay.CF.Name, ix.KVVersion, extra)))
	}
	var stored []string
	for _, f := range lays[0].Fields {
		if f.KeyPart >= 0 && f.Storage != mrStoreAll {
			how := "key"
			if f.Storage == mrStoreSome {
				how = "key + unpack_info"
			}
			stored = append(stored, fmt.Sprintf("%s (%s)", f.Col.Name, how))
		}
	}
	if len(stored) > 0 {
		fmt.Printf("%s %s\n", dim("Decoded from the key:"), val(strings.Join(stored, ", ")))
	}
	fmt.Printf("%s %s\n", dim("Null bitmap:"), val(fmt.Sprintf("%d byte(s)", lays[0].NullBytes)))
}

// extractMyRocksTables extracts every MyRocks table of the instance whose
// RocksDB directory is rocksDir (--source-dir), returning the same tallies
// as the other extract* helpers.
func extractMyRocksTables(rocksDir, datadir, outDir string, tsvDump *TSVDump) (nTables, nOK, nErr, nCorruptPages, nSkipped, nFailed int, err error) {
	db, dict, err := openMyRocks(rocksDir, datadir)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	defer db.Close()
	if len(dict.Defs) == 0 {
		fmt.Printf("%s %s: %v\n", dim("Note:       "), rocksDir, errMyRocksNoTables)
	}
	stats := &mrWalkStats{}
	for _, lt := range dict.logicalTables() {
		if tsvDump != nil && !*includeSystemSchemas && isSystemSchema(lt.Schema) {
			nSkipped++
			continue
		}
		t, lays, versionID, _, err := prepareMyRocksTable(db, dict, datadir, lt)
		if err != nil {
			nFailed++
			if !examineMode {
				fmt.Fprintf(os.Stderr, "%s MyRocks table %s: %v\n", warn("warning:"), lt.fullName(), err)
			}
			examineFailure(lt.fullName(), "ROCKSDB", err)
			continue
		}
		ok, tOK, tErr, tCorrupt, _ := writeAndSummarizeTable(t, rocksDir, outDir, versionID, tsvDump,
			func(outCols []*Column, format outputFormat) RowWalker {
				return myrocksRowWalker(db, lays, outCols, format, *skipCorrupted, *deletedOnly, stats)
			})
		if !ok {
			nFailed++
			continue
		}
		nTables++
		nOK += tOK
		nErr += tErr
		nCorruptPages += tCorrupt
	}
	printMyRocksNotes(stats)
	return nTables, nOK, nErr, nCorruptPages, nSkipped, nFailed, nil
}
