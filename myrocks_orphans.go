// myrocks_orphans.go: finding a MyRocks table's rows when the MyRocks data
// dictionary has lost track of it.
//
// MyRocks maps tables to index numbers only in its own dictionary (the
// __system__ column family - see myrocks.go); neither the table's .sdi nor
// its .frm records the number. If the dictionary's blocks are damaged, a
// table can disappear from it while its rows, in other files, are intact.
// For each such table - one whose schema file is present but which the
// dictionary doesn't list - this looks through the index numbers that hold
// data but that the dictionary doesn't claim, and decodes a sample of
// each one's rows with the table's own layout: an index whose sampled rows
// all decode cleanly (every key and value byte accounted for) fits. A
// table matched by exactly one such index, which no other table also
// fits, is extracted from it, with a warning saying so.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	orphanSampleRows = 24
	firstUserIndexID = 256 // Rdb_key_def::END_DICT_INDEX_ID + 1
)

// mrSchemaFile is one MyRocks table definition found on disk.
type mrSchemaFile struct {
	DBDir, TableBase string
	Path             string
}

// findMyRocksSchemaFiles lists every MyRocks table's .sdi/.frm under
// datadir's schema directories.
func findMyRocksSchemaFiles(datadir string) []mrSchemaFile {
	var out []mrSchemaFile
	dirs, err := os.ReadDir(datadir)
	if err != nil {
		return nil
	}
	sdiRe := regexp.MustCompile(`^(.+)_[0-9]+\.sdi$`)
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "#") {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(datadir, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			p := filepath.Join(datadir, d.Name(), name)
			switch {
			case sdiRe.MatchString(name):
				raw, _, err := loadStandaloneSDI(p)
				if err == nil && strings.EqualFold(raw.Engine, "ROCKSDB") {
					out = append(out, mrSchemaFile{DBDir: d.Name(), TableBase: sdiRe.FindStringSubmatch(name)[1], Path: p})
				}
			case strings.HasSuffix(strings.ToLower(name), ".frm"):
				if def, err := readFRMDefinition(p); err == nil && strings.EqualFold(def.engineName, "ROCKSDB") {
					out = append(out, mrSchemaFile{DBDir: d.Name(), TableBase: strings.TrimSuffix(name, filepath.Ext(name)), Path: p})
				}
			}
		}
	}
	return out
}

// unclaimedIndexIDs lists, per user column family, the index numbers that
// may hold data but that no dictionary entry claims: every number between
// two consecutive index separators of an SST could appear inside the
// block they bound, so the whole span is a candidate (each is checked by
// an actual scan later).
func unclaimedIndexIDs(db *rdbDB, dict *mrDict) map[uint32][]uint32 {
	claimed := map[uint32]bool{}
	for _, td := range dict.Defs {
		for _, ix := range td.Indexes {
			claimed[ix.ID] = true
		}
	}
	idOf := func(k []byte) (uint32, bool) {
		if len(k) < 4 {
			return 0, false
		}
		return binary.BigEndian.Uint32(k), true
	}
	out := map[uint32][]uint32{}
	for _, cf := range db.CFs {
		if cf.Name == mrSystemCFName {
			continue
		}
		seen := map[uint32]bool{}
		addRange := func(a, b uint32) {
			if a > b {
				a, b = b, a
			}
			if b-a > 100000 {
				return // nonsense span (a damaged separator): ignore
			}
			for id := a; ; id++ {
				if id >= firstUserIndexID && !claimed[id] {
					seen[id] = true
				}
				if id == b {
					break
				}
			}
		}
		for _, ref := range cf.SSTs {
			if ref.Missing || ref.sst == nil {
				continue
			}
			idx, err := ref.sst.loadIndex()
			if err != nil {
				for _, e := range db.salvageSST(ref, err) {
					if id, ok := idOf(e.UserKey); ok {
						addRange(id, id)
					}
				}
				continue
			}
			prev, havePrev := uint32(0), false
			if ref.HaveManifestRange {
				prev, havePrev = idOf(ref.Smallest)
			}
			for _, e := range idx {
				id, ok := idOf(e.SepUserKey)
				if !ok {
					continue
				}
				if havePrev {
					addRange(prev, id)
				} else {
					addRange(id, id)
				}
				prev, havePrev = id, true
			}
		}
		for _, e := range cf.mem {
			if id, ok := idOf(e.UserKey); ok {
				addRange(id, id)
			}
		}
		for id := range seen {
			out[cf.ID] = append(out[cf.ID], id)
		}
		sort.Slice(out[cf.ID], func(i, j int) bool { return out[cf.ID][i] < out[cf.ID][j] })
	}
	return out
}

// orphanSample is the first few live rows of one candidate index.
type orphanSample struct {
	CF, ID uint32
	Keys   [][]byte
	Vals   [][]byte
}

func sampleIndex(db *rdbDB, cf *rdbCF, id uint32) orphanSample {
	s := orphanSample{CF: cf.ID, ID: id}
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], id)
	saved := db.onCorrupt
	db.onCorrupt = nil // damage met while sampling is reported by the real scan later
	db.scanCF(cf, p[:], func(kv *rdbKeyVersions) bool {
		if e, ok := kv.Current(cf); ok {
			s.Keys = append(s.Keys, kv.UserKey)
			s.Vals = append(s.Vals, e.Value)
		}
		return len(s.Keys) < orphanSampleRows
	})
	db.onCorrupt = saved
	return s
}

var (
	ttlCommentRe = regexp.MustCompile(`(?i)ttl_duration=([0-9]+)`)
	cfCommentRe  = regexp.MustCompile(`(?i)(?:^|;)\s*cfname=([^;]+)`)
)

// sameSample reports whether two samples hold the same rows (keys compared
// without their index number).
func sameSample(a, b orphanSample) bool {
	if len(a.Keys) != len(b.Keys) {
		return false
	}
	for i := range a.Keys {
		if string(a.Keys[i][4:]) != string(b.Keys[i][4:]) || string(a.Vals[i]) != string(b.Vals[i]) {
			return false
		}
	}
	return true
}

// orphanFitter tries one table definition against sampled index rows.
type orphanFitter struct {
	db      *rdbDB
	t       *Table
	raw     *ddTableJSON
	rules   mrRules
	kvs     []uint16
	typ     byte
	ttl     uint64
	wantCF  string
	outCols []*Column
}

func newOrphanFitter(db *rdbDB, raw *ddTableJSON, schemaPath string) (*orphanFitter, error) {
	ft := &orphanFitter{db: db, raw: raw, rules: mrRules80, kvs: []uint16{15, 14, 13, 12, 11, 10}, typ: mrIndexHiddenPrimary}
	if strings.HasSuffix(strings.ToLower(schemaPath), ".frm") {
		ft.rules, ft.kvs = mrRules57, []uint16{13, 12, 11, 10}
	}
	pkPos, pkComment := -1, ""
	for i, idx := range raw.Indexes {
		if idx.Type == ddIndexPrimary {
			pkPos, ft.typ, pkComment = i, mrIndexPrimary, idx.Comment
			break
		}
	}
	t, err := BuildRocksDBTable(raw, pkPos)
	if err != nil {
		return nil, err
	}
	ft.t = t
	if m := ttlCommentRe.FindStringSubmatch(raw.Comment); m != nil {
		ft.ttl, _ = strconv.ParseUint(m[1], 10, 64)
	}
	ft.wantCF = myrocksCFName(pkComment)
	ft.outCols = OutputColumns(t)
	return ft, nil
}

// fit returns the primary index description (kv_format_version included)
// under which every sampled row of s decodes cleanly, if any.
func (ft *orphanFitter) fit(s orphanSample) (mrIndex, bool) {
	for _, kv := range ft.kvs {
		ix := mrIndex{CF: s.CF, ID: s.ID, Type: ft.typ, KVVersion: kv, Known: true, TTL: ft.ttl}
		if ft.ttl > 0 {
			ix.Flags = mrTTLFlag
		}
		lay, err := buildMyRocksLayout(ft.t, ft.raw, ix, &mrTableDef{}, ft.db, ft.rules)
		if err != nil {
			return mrIndex{}, false
		}
		lay.strict = true
		ok := len(s.Keys) > 0
		for k := range s.Keys {
			if _, _, err := lay.decodeRow(s.Keys[k], s.Vals[k], ft.outCols, formatSQL, 0); err != nil {
				ok = false
				break
			}
		}
		if ok {
			return ix, true
		}
	}
	return mrIndex{}, false
}

// forceMyRocksIndex makes lt's (unpartitioned) table read from index
// number id (--rocksdb-index), whatever the dictionary says.
func forceMyRocksIndex(db *rdbDB, dict *mrDict, datadir string, lt *mrLogicalTable, id uint32) error {
	raw, _, schemaPath, err := findMyRocksSchema(datadir, lt)
	if err != nil {
		return err
	}
	if raw.PartitionType != 0 {
		return fmt.Errorf("--rocksdb-index can't be used with a partitioned table (each partition is a separate index)")
	}
	ft, err := newOrphanFitter(db, raw, schemaPath)
	if err != nil {
		return err
	}
	var known *mrIndex
	for _, td := range dict.Defs {
		for i := range td.Indexes {
			if td.Indexes[i].ID == id && td.Indexes[i].Known {
				known = &td.Indexes[i]
			}
		}
	}
	for _, cf := range db.CFs {
		if cf.Name == mrSystemCFName {
			continue
		}
		s := sampleIndex(db, cf, id)
		if len(s.Keys) == 0 {
			continue
		}
		ix, ok := ft.fit(s)
		if known != nil && known.CF == cf.ID {
			ix, ok = *known, true
			ix.Type = ft.typ
		}
		if !ok {
			return fmt.Errorf("index number %d (column family %q) holds rows, but they don't decode as %s's - wrong index?", id, cf.Name, lt.fullName())
		}
		lt.Parts = []*mrTableDef{{RawName: lt.DBDir + "." + lt.TableBase, DBDir: lt.DBDir, TableBase: lt.TableBase,
			Schema: lt.Schema, Table: lt.Table, Indexes: []mrIndex{ix}, Inferred: true}}
		lt.Partitioned = false
		return nil
	}
	return fmt.Errorf("no column family holds any row under index number %d", id)
}

// myrocksCFName is the column family an index COMMENT places it in:
// "cfname=<name>" (current MyRocks), or the whole comment when it holds no
// "=" (the original syntax); "default" otherwise.
func myrocksCFName(comment string) string {
	comment = strings.TrimSpace(comment)
	if m := cfCommentRe.FindStringSubmatch(comment); m != nil {
		return strings.TrimSpace(m[1])
	}
	if comment != "" && !strings.Contains(comment, "=") {
		return comment
	}
	return "default"
}

// recoverOrphanTables adds a dictionary entry, marked Inferred, for every
// MyRocks table whose schema is in datadir but whose dictionary entry is
// gone, when exactly one unclaimed index fits it (see the file comment).
// It reports what it couldn't match.
func recoverOrphanTables(db *rdbDB, dict *mrDict, datadir string) {
	known := map[string]bool{}
	for _, td := range dict.Defs {
		known[td.DBDir+"\x00"+td.TableBase] = true
	}
	var orphans []mrSchemaFile
	for _, sf := range findMyRocksSchemaFiles(datadir) {
		if !known[sf.DBDir+"\x00"+sf.TableBase] {
			orphans = append(orphans, sf)
		}
	}
	if len(orphans) == 0 {
		return
	}

	cands := unclaimedIndexIDs(db, dict)
	var samples []orphanSample
	for cfID, ids := range cands {
		cf := db.CFs[cfID]
		for _, id := range ids {
			if s := sampleIndex(db, cf, id); len(s.Keys) > 0 {
				samples = append(samples, s)
			}
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].ID < samples[j].ID })

	type fit struct {
		sample int
		ix     mrIndex
	}
	fits := make([][]fit, len(orphans))
	for oi, sf := range orphans {
		lt := &mrLogicalTable{DBDir: sf.DBDir, TableBase: sf.TableBase, Schema: decodeMySQLFilename(sf.DBDir), Table: decodeMySQLFilename(sf.TableBase)}
		raw, _, schemaPath, err := findMyRocksSchema(datadir, lt)
		if err != nil || raw.PartitionType != 0 {
			continue // a partitioned table's partitions can't be told apart this way
		}
		ft, err := newOrphanFitter(db, raw, schemaPath)
		if err != nil {
			continue
		}
		for si, s := range samples {
			if cf := db.CFs[s.CF]; cf == nil || cf.Name != ft.wantCF {
				continue
			}
			if ix, ok := ft.fit(s); ok {
				fits[oi] = append(fits[oi], fit{sample: si, ix: ix})
			}
		}
	}
	fitCount := make([]int, len(samples))
	for oi := range fits {
		for _, f := range fits[oi] {
			fitCount[f.sample]++
		}
	}

	for oi, sf := range orphans {
		name := decodeMySQLFilename(sf.DBDir) + "." + decodeMySQLFilename(sf.TableBase)
		switch {
		case len(fits[oi]) == 1 && fitCount[fits[oi][0].sample] == 1:
			f := fits[oi][0]
			td := &mrTableDef{RawName: sf.DBDir + "." + sf.TableBase, DBDir: sf.DBDir, TableBase: sf.TableBase,
				Schema: decodeMySQLFilename(sf.DBDir), Table: decodeMySQLFilename(sf.TableBase), Indexes: []mrIndex{f.ix}, Inferred: true}
			dict.Defs = append(dict.Defs, td)
			fmt.Fprintf(os.Stderr, "%s MyRocks table %s isn't in the (damaged?) MyRocks dictionary - its rows were found under index number %d (column family %d), the only unclaimed index they all decode from; check the result\n",
				warn("warning:"), name, f.ix.ID, f.ix.CF)
		case len(fits[oi]) > 1:
			// Several indexes fit: other tables of the same shape, or older
			// copies of this one (an ALTER TABLE that copied it, not yet
			// compacted away) - whose first rows can even be identical
			// without the tables being so. Not guessed at.
			var ids []string
			for i, f := range fits[oi] {
				note := ""
				for j, g := range fits[oi] {
					if i != j && sameSample(samples[f.sample], samples[g.sample]) {
						note = " (first rows identical to another candidate's)"
						break
					}
				}
				ids = append(ids, fmt.Sprintf("%d%s", f.ix.ID, note))
			}
			fmt.Fprintf(os.Stderr, "%s MyRocks table %s isn't in the (damaged?) MyRocks dictionary, and the rows of more than one index fit it: %s - skipped; extract it with --file <datadir> --table %s --rocksdb-index <number> once you know which (newer tables get higher numbers)\n",
				warn("warning:"), name, strings.Join(ids, ", "), name)
		case len(fits[oi]) == 1 && fitCount[fits[oi][0].sample] > 1:
			fmt.Fprintf(os.Stderr, "%s MyRocks table %s isn't in the (damaged?) MyRocks dictionary; index number %d fits it, but fits another table too - skipped (see --rocksdb-index)\n",
				warn("warning:"), name, fits[oi][0].ix.ID)
		default:
			fmt.Fprintf(os.Stderr, "%s MyRocks table %s has a schema file but no entry in the MyRocks dictionary, and no unclaimed index holds rows that fit it - dropped before this copy was taken, or its rows are gone\n",
				warn("warning:"), name)
		}
	}
}
