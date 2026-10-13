// rocksdb_db.go: an offline, read-only view of a whole RocksDB database
// directory (MyRocks' .rocksdb/), built from the same files RocksDB itself
// reads at startup:
//
//   - CURRENT names the live MANIFEST, a log (rocksdb_log.go) of
//     VersionEdits; replaying it gives each column family's set of live
//     SST files (rocksdb_sst.go) and the oldest WAL still needed.
//   - Every WAL (NNNNNN.log) from that one on holds writes not yet flushed
//     to an SST; replaying them (WriteBatches, with two-phase-commit
//     prepare/commit markers resolved) gives an in-memory table.
//
// A key can have several versions across these sources (one per
// overwrite, until compaction drops the older ones); each version carries
// a sequence number, and the highest one is the key's current state - a
// value, or a deletion (point or range) hiding everything older. scanCF
// merges every source for one key range into exactly that view, the way
// RocksDB's own DBIter does.
//
// Damage tolerance: a MANIFEST that can't be fully replayed (missing,
// truncated, corrupt) is replaced by treating every SST file present as
// live, with a warning; WAL records that fail their checksum are reported
// and skipped. See rocksdb_sst.go for damaged SST files.
package main

import (
	"bytes"
	"cmp"
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// rdbEntry is one version of one key.
type rdbEntry struct {
	UserKey []byte
	Seq     uint64
	Type    byte
	Value   []byte
	Origin  entryOrigin // where it was read from, for warnings
}

// entryOrigin says where an entry came from; only formatted on demand
// (formatting it for every entry would dominate a large scan).
type entryOrigin struct {
	File string
	Off  int64
	Kind byte // originSST, originWAL, originSalvaged, originBinlogCommit
}

const (
	originSST = iota
	originWAL
	originSalvaged
	originBinlogCommit
)

func (o entryOrigin) String() string {
	switch o.Kind {
	case originWAL:
		return fmt.Sprintf("%s@%d", o.File, o.Off)
	case originSalvaged:
		return fmt.Sprintf("%s block@%d (salvaged)", o.File, o.Off)
	case originBinlogCommit:
		return fmt.Sprintf("%s@%d (prepared, committed per the binlog)", o.File, o.Off)
	}
	return fmt.Sprintf("%s block@%d", o.File, o.Off)
}

// rdbCF is one column family.
type rdbCF struct {
	ID       uint32
	Name     string
	Reverse  bool
	SSTs     []*rdbSSTRef
	mem      []rdbEntry // from the WAL, sorted by (user key, seq desc)
	rangeDel []rangeTombstone
	LogNum   uint64
	merge    func(base []byte, operands [][]byte) ([]byte, bool)
}

// rdbSSTRef is one live SST file of a column family, with what the
// MANIFEST knows about it.
type rdbSSTRef struct {
	Number            uint64
	Level             int
	Path              string
	Smallest, Largest []byte // user keys; nil if unknown
	SmallestSeq       uint64
	LargestSeq        uint64
	HaveSeqRange      bool
	HaveManifestRange bool
	Missing           bool // listed by the MANIFEST but not present
	CF                uint32
	sst               *sstFile
	salvaged          []rdbEntry // filled once if the file needed salvaging
	salvageIssues     []salvageIssue
	salvageDone       bool
	salvageNoted      bool // the "had to salvage" notice was shown already
}

// rdbProblem is one thing worth telling the user about while opening.
type rdbProblem struct {
	Where, What string
	Data        bool // could cost rows (vs. purely informational)
}

// rdbDB is an opened RocksDB directory.
type rdbDB struct {
	Dir          string
	CFs          map[uint32]*rdbCF
	ManifestPath string
	ManifestOK   bool // replayed completely; false means every SST present was treated as live
	LastSeq      uint64
	Problems     []rdbProblem
	WALFiles     []string
	Orphans      []string // SST files present but not in the MANIFEST
	Missing      []string // SST files the MANIFEST lists but that aren't present
	// prepared holds two-phase-commit transactions prepared in the WAL with
	// no COMMIT/ROLLBACK marker after them, by name, in prepare order
	// (preparedOrder) - see commitPrepared.
	prepared      map[string][]walOp
	preparedOrder []string
	maxSeq        uint64 // highest sequence number seen in the WAL

	// Corruption callback for block-level problems hit while scanning;
	// set by the caller for the duration of a scan.
	onCorrupt func(c rdbCorruption)
	// stop, set by onCorrupt, ends the current scan early.
	stop bool
	// damaged lists the key ranges of unreadable blocks/files met during
	// the current scan (see possiblyStale).
	damaged []damagedRange
}

// damagedRange is the key range of a block (or file) that couldn't be
// read: any key in it may have had a newer version there than the one
// the merge found elsewhere.
type damagedRange struct {
	CF     uint32
	Lo, Hi []byte // Lo exclusive unless LoIncl (nil: unbounded), Hi inclusive (nil: unbounded); comparator order
	LoIncl bool
	MaxSeq uint64 // newest sequence number the damaged data could hold
}

func (db *rdbDB) noteDamaged(cf uint32, lo, hi []byte, maxSeq uint64) {
	db.damaged = append(db.damaged, damagedRange{CF: cf, Lo: lo, Hi: hi, MaxSeq: maxSeq})
}

// possiblyStale reports whether e, a key's surviving newest version, could
// have been superseded by a version lost in a damaged block or file.
func (db *rdbDB) possiblyStale(cf *rdbCF, key []byte, seq uint64) bool {
	for _, d := range db.damaged {
		if d.CF != cf.ID {
			continue
		}
		if d.MaxSeq != 0 && seq >= d.MaxSeq {
			continue
		}
		if d.Lo != nil {
			c := compareUserKeys(key, d.Lo, cf.Reverse)
			if c < 0 || (c == 0 && !d.LoIncl) {
				continue
			}
		}
		if d.Hi != nil && compareUserKeys(key, d.Hi, cf.Reverse) > 0 {
			continue
		}
		return true
	}
	return false
}

func (db *rdbDB) problem(where, what string, data bool) {
	db.Problems = append(db.Problems, rdbProblem{Where: where, What: what, Data: data})
}

func (db *rdbDB) Close() {
	for _, cf := range db.CFs {
		for _, r := range cf.SSTs {
			if r.sst != nil {
				r.sst.Close()
			}
		}
	}
}

// openRocksDB opens dir read-only. It fails only if dir holds no RocksDB
// data at all; anything less is recorded in db.Problems.
func openRocksDB(dir string) (*rdbDB, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	db := &rdbDB{Dir: dir, CFs: map[uint32]*rdbCF{}}
	var ssts, logs, manifests []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch {
		case strings.HasSuffix(n, ".sst") || strings.HasSuffix(n, ".ldb"):
			ssts = append(ssts, n)
		case strings.HasSuffix(n, ".log") && isAllDigits(strings.TrimSuffix(n, ".log")):
			logs = append(logs, n)
		case strings.HasPrefix(n, "MANIFEST-"):
			manifests = append(manifests, n)
		}
	}
	if len(ssts) == 0 && len(logs) == 0 && len(manifests) == 0 {
		return nil, fmt.Errorf("%s holds no RocksDB files (no .sst, .log or MANIFEST)", dir)
	}

	sstByNum := map[uint64]string{}
	for _, n := range ssts {
		sstByNum[sstNumberFromName(n)] = n
	}

	// --- MANIFEST ---
	manifest := ""
	if cur, err := os.ReadFile(filepath.Join(dir, "CURRENT")); err == nil {
		name := strings.TrimSpace(string(cur))
		if strings.HasPrefix(name, "MANIFEST-") && !strings.ContainsAny(name, "/\\") {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				manifest = name
			} else {
				db.problem("CURRENT", fmt.Sprintf("names %s, which doesn't exist", name), false)
			}
		} else {
			db.problem("CURRENT", "doesn't name a MANIFEST file", false)
		}
	} else {
		db.problem("CURRENT", "missing or unreadable", false)
	}
	if manifest == "" && len(manifests) > 0 {
		sort.Slice(manifests, func(i, j int) bool {
			return manifestNum(manifests[i]) < manifestNum(manifests[j])
		})
		manifest = manifests[len(manifests)-1]
		db.problem(manifest, "using the newest MANIFEST found instead", false)
	}

	type liveFile struct {
		cf uint32
		mf manifestFile
	}
	live := map[uint64]liveFile{}
	cfNames := map[uint32]string{0: "default"}
	cfLog := map[uint32]uint64{}
	minLogToKeep := uint64(0)
	if manifest != "" {
		db.ManifestPath = filepath.Join(dir, manifest)
		db.ManifestOK = true
		recs, probs, err := readLogFile(db.ManifestPath, manifestNum(manifest))
		if err != nil {
			db.ManifestOK = false
			db.problem(manifest, err.Error(), true)
		}
		for _, p := range probs {
			db.ManifestOK = false
			db.problem(manifest, fmt.Sprintf("at offset %d: %s", p.Offset, p.Reason), true)
		}
		dropped := map[uint32]bool{}
		for _, rec := range recs {
			e, err := decodeVersionEdit(rec.Data)
			if err != nil {
				db.ManifestOK = false
				db.problem(manifest, fmt.Sprintf("VersionEdit at offset %d: %v", rec.Offset, err), true)
				continue
			}
			cf := uint32(0)
			if e.HasCF {
				cf = e.CF
			}
			if e.CFAdd {
				cfNames[cf] = e.CFName
				delete(dropped, cf)
			}
			if e.CFDrop {
				dropped[cf] = true
			}
			if e.HasLogNum {
				cfLog[cf] = e.LogNumber
			}
			if e.HasMinLog && e.MinLogToKeep > minLogToKeep {
				minLogToKeep = e.MinLogToKeep
			}
			if e.HasLastSeq && e.LastSeq > db.LastSeq {
				db.LastSeq = e.LastSeq
			}
			for _, n := range e.Deleted {
				delete(live, n)
			}
			for _, f := range e.NewFiles {
				live[f.Number] = liveFile{cf: cf, mf: f}
			}
		}
		for cf := range dropped {
			for n, lf := range live {
				if lf.cf == cf {
					delete(live, n)
				}
			}
			delete(cfNames, cf)
		}
		if len(recs) == 0 {
			db.ManifestOK = false
		}
	} else {
		db.problem(dir, "no MANIFEST found", true)
	}

	cfOf := func(id uint32) *rdbCF {
		cf := db.CFs[id]
		if cf == nil {
			cf = &rdbCF{ID: id, Name: cfNames[id], LogNum: cfLog[id]}
			db.CFs[id] = cf
		}
		return cf
	}
	for id := range cfNames {
		cfOf(id)
	}

	// --- SST files ---
	addSST := func(cfID uint32, num uint64, mf *manifestFile, s *sstFile) {
		cf := cfOf(cfID)
		ref := &rdbSSTRef{Number: num, Path: s.Path, sst: s, CF: cfID}
		if mf != nil {
			ref.Level = mf.Level
			ref.SmallestSeq, ref.LargestSeq, ref.HaveSeqRange = mf.SmallestSeq, mf.LargestSeq, true
			if uk, _, _, ok := splitInternalKey(mf.Smallest); ok {
				ref.Smallest = uk
			}
			if uk, _, _, ok := splitInternalKey(mf.Largest); ok {
				ref.Largest = uk
			}
			ref.HaveManifestRange = ref.Smallest != nil && ref.Largest != nil
			if s.External && s.GlobalSeqno == 0 {
				s.GlobalSeqno = mf.LargestSeq
			}
		}
		if s.Reverse {
			cf.Reverse = true
		}
		cf.SSTs = append(cf.SSTs, ref)
		cf.rangeDel = append(cf.rangeDel, s.RangeDels...)
	}

	if db.ManifestOK {
		var nums []uint64
		for n := range live {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		for _, n := range nums {
			lf := live[n]
			name, ok := sstByNum[n]
			if !ok {
				miss := fmt.Sprintf("%06d.sst", n)
				db.Missing = append(db.Missing, miss)
				db.problem(miss, "listed as live by the MANIFEST but not present - its rows are missing", true)
				ref := &rdbSSTRef{Number: n, Level: lf.mf.Level, Path: filepath.Join(dir, miss), Missing: true, CF: lf.cf}
				if uk, _, _, ok := splitInternalKey(lf.mf.Smallest); ok {
					ref.Smallest = uk
				}
				if uk, _, _, ok := splitInternalKey(lf.mf.Largest); ok {
					ref.Largest = uk
				}
				ref.HaveManifestRange = ref.Smallest != nil && ref.Largest != nil
				cfOf(lf.cf).SSTs = append(cfOf(lf.cf).SSTs, ref)
				continue
			}
			s, err := openSST(filepath.Join(dir, name))
			if err != nil {
				db.problem(name, err.Error(), true)
				continue
			}
			if s.OpenErr != nil {
				db.problem(name, s.OpenErr.Error()+" - will try to salvage its data blocks", true)
			} else if s.HaveCFID && s.CFID != lf.cf {
				db.problem(name, fmt.Sprintf("its properties say column family %d, the MANIFEST says %d - using the MANIFEST", s.CFID, lf.cf), false)
			}
			mf := lf.mf
			addSST(lf.cf, n, &mf, s)
			delete(sstByNum, n)
		}
		for n, name := range sstByNum {
			_ = n
			db.Orphans = append(db.Orphans, name)
		}
		sort.Strings(db.Orphans)
	} else {
		// No usable MANIFEST: every SST present is assumed live, its column
		// family taken from its own properties. Obsolete files a crash left
		// behind could then contribute stale versions - but every version
		// still carries its sequence number, so the newest one wins anyway.
		names := make([]string, 0, len(sstByNum))
		for _, name := range sstByNum {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s, err := openSST(filepath.Join(dir, name))
			if err != nil {
				db.problem(name, err.Error(), true)
				continue
			}
			cfID := uint32(0)
			if s.OpenErr != nil {
				db.problem(name, s.OpenErr.Error()+" - will try to salvage its data blocks (column family unknown, assuming 0)", true)
			} else if s.HaveCFID {
				cfID = s.CFID
				if s.CFName != "" {
					if _, known := cfNames[cfID]; !known || cfNames[cfID] == "" {
						cfNames[cfID] = s.CFName
					}
				}
			}
			addSST(cfID, sstNumberFromName(name), nil, s)
		}
		for id, cf := range db.CFs {
			if cf.Name == "" {
				cf.Name = cfNames[id]
			}
		}
	}

	// --- WAL ---
	minLog := uint64(0)
	if db.ManifestOK {
		first := true
		for _, cf := range db.CFs {
			if first || cf.LogNum < minLog {
				minLog, first = cf.LogNum, false
			}
		}
		if minLogToKeep > 0 && minLogToKeep < minLog {
			// 2PC: a log still holding a prepared section must be replayed
			// even if every column family has flushed past it.
			minLog = minLogToKeep
		}
	}
	sort.Slice(logs, func(i, j int) bool { return sstNumberFromName(logs[i]) < sstNumberFromName(logs[j]) })
	w := &walReplayer{db: db, prepared: map[string][]walOp{}}
	for _, name := range logs {
		num := sstNumberFromName(name)
		if num < minLog {
			continue
		}
		db.WALFiles = append(db.WALFiles, name)
		w.replay(filepath.Join(dir, name), num)
	}
	for _, cf := range db.CFs {
		// A salvaged file can't say which comparator it used; MyRocks names
		// every reverse-ordered column family "rev:...".
		if strings.HasPrefix(cf.Name, "rev:") {
			cf.Reverse = true
		}
	}
	db.prepared = w.prepared
	for _, x := range w.order {
		if _, ok := w.prepared[x]; ok {
			db.preparedOrder = append(db.preparedOrder, x)
		}
	}
	db.maxSeq = w.seq
	for _, cf := range db.CFs {
		db.sortMem(cf)
	}
	if cf := db.cfByName("__system__"); cf != nil {
		cf.merge = rdbSystemMerge
	}
	return db, nil
}

func (db *rdbDB) sortMem(c *rdbCF) {
	slices.SortStableFunc(c.mem, func(a, b rdbEntry) int {
		if x := compareUserKeys(a.UserKey, b.UserKey, c.Reverse); x != 0 {
			return x
		}
		return cmp.Compare(b.Seq, a.Seq)
	})
}

// PendingTxns is how many prepared transactions are still unresolved.
func (db *rdbDB) PendingTxns() int { return len(db.prepared) }

// commitPrepared resolves every prepared transaction left without a
// COMMIT or ROLLBACK marker: those decide() accepts are applied, as
// committed after everything else in the WAL (crash recovery commits them
// at startup), the rest dropped.
func (db *rdbDB) commitPrepared(decide func(name []byte) bool) (committed, rolledBack int) {
	w := &walReplayer{db: db, seq: db.maxSeq + 1}
	if db.LastSeq >= w.seq {
		w.seq = db.LastSeq + 1
	}
	touched := map[*rdbCF]bool{}
	for _, name := range db.preparedOrder {
		ops := db.prepared[name]
		if !decide([]byte(name)) {
			rolledBack++
			continue
		}
		committed++
		for _, o := range ops {
			o.Origin.Kind = originBinlogCommit
			w.apply(o)
			if cf := db.CFs[o.CF]; cf != nil {
				touched[cf] = true
			}
		}
	}
	for cf := range touched {
		db.sortMem(cf)
	}
	db.maxSeq = w.seq
	db.prepared, db.preparedOrder = nil, nil
	return committed, rolledBack
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func manifestNum(name string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(name, "MANIFEST-"), 10, 64)
	return n
}

// cfByName finds a column family by name.
func (db *rdbDB) cfByName(name string) *rdbCF {
	for _, cf := range db.CFs {
		if cf.Name == name {
			return cf
		}
	}
	return nil
}

// --- WAL replay ---

// walReplayer applies WAL WriteBatches the way recovery under MyRocks'
// default write policy (WriteCommitted) does: plain writes take sequence
// numbers in order; a two-phase-commit transaction's writes are held from
// its prepare section until its COMMIT marker, and take their sequence
// numbers then (or are dropped on ROLLBACK).
type walReplayer struct {
	db        *rdbDB
	seq       uint64
	inPrepare bool
	pending   []walOp
	prepared  map[string][]walOp
	order     []string
	origin    entryOrigin
	curLog    uint64
}

func (w *walReplayer) replay(path string, num uint64) {
	name := filepath.Base(path)
	recs, probs, err := readLogFile(path, num)
	if err != nil {
		w.db.problem(name, err.Error(), true)
		return
	}
	for _, p := range probs {
		if p.Tail {
			w.db.problem(name, fmt.Sprintf("at offset %d: %s (an unfinished last write - normal after a crash)", p.Offset, p.Reason), false)
		} else {
			w.db.problem(name, fmt.Sprintf("at offset %d: %s - skipped to the next 32KB block", p.Offset, p.Reason), true)
		}
	}
	w.curLog = num
	for _, rec := range recs {
		w.origin = entryOrigin{File: name, Off: rec.Offset, Kind: originWAL}
		_, err := decodeWriteBatch(rec.Data, w)
		if err != nil {
			w.db.problem(name, fmt.Sprintf("WriteBatch at offset %d: %v", rec.Offset, err), true)
		}
	}
}

func (w *walReplayer) start(seq uint64) {
	w.seq = seq
	w.inPrepare = false
	w.pending = nil
}

func (w *walReplayer) op(o walOp) {
	o.LogNum, o.Origin = w.curLog, w.origin
	if w.inPrepare {
		w.pending = append(w.pending, o)
		return
	}
	w.apply(o)
}

// apply adds one write to its column family's in-memory table - unless,
// as in RocksDB's own recovery (MemTableInserter::SeekToColumnFamily),
// the log it was written to predates the column family's log_number: its
// data was flushed to an SST already, and replaying it again could bring
// back rows deleted (and compacted away) since.
func (w *walReplayer) apply(o walOp) {
	seq := w.seq
	w.seq++
	cf := w.db.CFs[o.CF]
	if cf == nil {
		cf = &rdbCF{ID: o.CF}
		w.db.CFs[o.CF] = cf
	}
	if w.db.ManifestOK && o.LogNum < cf.LogNum {
		return
	}
	if o.Type == rdbTypeRangeDeletion {
		cf.rangeDel = append(cf.rangeDel, rangeTombstone{Start: o.Key, End: o.Value, Seq: seq})
		return
	}
	cf.mem = append(cf.mem, rdbEntry{UserKey: o.Key, Seq: seq, Type: o.Type, Value: o.Value, Origin: o.Origin})
}

func (w *walReplayer) beginPrepare() { w.inPrepare = true; w.pending = nil }

func (w *walReplayer) endPrepare(xid []byte) {
	w.prepared[string(xid)] = w.pending
	w.order = append(w.order, string(xid))
	w.inPrepare = false
	w.pending = nil
}

func (w *walReplayer) commit(xid []byte) {
	ops, ok := w.prepared[string(xid)]
	if !ok {
		return
	}
	delete(w.prepared, string(xid))
	for _, o := range ops {
		w.apply(o)
	}
}

func (w *walReplayer) rollback(xid []byte) { delete(w.prepared, string(xid)) }

// --- merged scans ---

// rdbKeyVersions is every surviving version of one user key, newest first.
type rdbKeyVersions struct {
	UserKey  []byte
	Versions []rdbEntry
}

// Current returns the key's live value, or ok=false if its newest version
// is a deletion (point or range) - i.e. the key doesn't exist. Merge
// operands on top are folded into the value below them with the column
// family's merge operator (only MyRocks' own __system__ column family has
// one - see rdbSystemMerge).
func (kv *rdbKeyVersions) Current(cf *rdbCF) (e rdbEntry, ok bool) {
	var operands [][]byte
	for i, v := range kv.Versions {
		if cf.rangeDeleted(kv.UserKey, v.Seq) {
			break
		}
		switch v.Type {
		case rdbTypeMerge:
			operands = append(operands, v.Value)
			continue
		case rdbTypeValue, rdbTypeBlobIndex, rdbTypeWideColumnEntity:
			if len(operands) == 0 {
				return v, true
			}
			return kv.merged(cf, kv.Versions[0], v.Value, operands)
		}
		_ = i
		break // a deletion
	}
	if len(operands) > 0 {
		return kv.merged(cf, kv.Versions[0], nil, operands)
	}
	return rdbEntry{}, false
}

func (kv *rdbKeyVersions) merged(cf *rdbCF, top rdbEntry, base []byte, operands [][]byte) (rdbEntry, bool) {
	if cf.merge == nil {
		// No merge operator known for this column family: the newest
		// operand is the best available guess.
		return top, true
	}
	v, ok := cf.merge(base, operands)
	if !ok {
		return top, true
	}
	top.Type, top.Value = rdbTypeValue, v
	return top, true
}

// rdbSystemMerge is MyRocks' Rdb_system_merge_op, the merge operator of
// its __system__ column family, used only for AUTO_INC entries: the
// result is the largest of the base value and every operand (each a
// 2-byte version plus an 8-byte big-endian counter).
func rdbSystemMerge(base []byte, operands [][]byte) ([]byte, bool) {
	var max uint64
	seen := false
	for _, b := range append([][]byte{base}, operands...) {
		if b == nil {
			continue
		}
		if len(b) != 10 {
			return nil, false
		}
		v := binary.BigEndian.Uint64(b[2:])
		if !seen || v > max {
			max, seen = v, true
		}
	}
	out := make([]byte, 10)
	binary.BigEndian.PutUint16(out, 1)
	binary.BigEndian.PutUint64(out[2:], max)
	return out, seen
}

// Deleted returns the newest value hidden behind a deletion - the row as
// it was right before it was deleted - for --deleted-only.
func (kv *rdbKeyVersions) Deleted(cf *rdbCF) (e rdbEntry, ok bool) {
	if _, live := kv.Current(cf); live {
		return rdbEntry{}, false
	}
	for _, v := range kv.Versions {
		if v.Type == rdbTypeValue {
			return v, true
		}
	}
	return rdbEntry{}, false
}

func (cf *rdbCF) rangeDeleted(key []byte, seq uint64) bool {
	for _, t := range cf.rangeDel {
		if t.Seq > seq && compareUserKeys(key, t.Start, cf.Reverse) >= 0 && compareUserKeys(key, t.End, cf.Reverse) < 0 {
			return true
		}
	}
	return false
}

// prefixSuccessor returns the smallest byte string greater than every
// string starting with p (nil if there is none: p is all 0xFF).
func prefixSuccessor(p []byte) []byte {
	s := append([]byte(nil), p...)
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != 0xFF {
			s[i]++
			return s[:i+1]
		}
	}
	return nil
}

// rdbSource yields one source's entries in comparator order, newest first
// within a key.
type rdbSource interface {
	next() (rdbEntry, bool)
}

type memSource struct {
	ents []rdbEntry
	i    int
}

func (m *memSource) next() (rdbEntry, bool) {
	if m.i >= len(m.ents) {
		return rdbEntry{}, false
	}
	m.i++
	return m.ents[m.i-1], true
}

// sstSource iterates one SST file's entries with user keys starting with
// prefix, block by block through its index (or from its salvaged entries).
type sstSource struct {
	db      *rdbDB
	ref     *rdbSSTRef
	prefix  []byte
	reverse bool
	index   []indexEntry
	bi      int
	cur     *blockIter
	blkOff  uint64
	done    bool
	salv    []rdbEntry
	si      int
	useSalv bool
	name    string
}

func newSSTSource(db *rdbDB, ref *rdbSSTRef, prefix []byte, reverse bool) *sstSource {
	src := &sstSource{db: db, ref: ref, prefix: prefix, reverse: reverse, name: ref.sst.Name()}
	s := ref.sst
	idx, err := s.loadIndex()
	if err != nil {
		src.useSalv = true
		src.salv = db.salvageSST(ref, err)
		// Every scan touching the file hears about what salvaging found
		// in its key range (salvaging itself runs once per file).
		for _, is := range ref.salvageIssues {
			if is.Info {
				if ref.salvageNoted {
					continue
				}
				ref.salvageNoted = true
			}
			if is.Global || keyRangeOverlapsPrefix(is.Lo, is.Hi, prefix, reverse) {
				if !is.Info {
					db.noteDamaged(ref.CF, is.Lo, is.Hi, ref.maxSeq())
				}
				db.report(rdbCorruption{File: s.Name(), Off: is.Off, Reason: is.Reason, Info: is.Info, Truncated: is.Truncated})
			}
		}
		start := 0
		if !reverse {
			start = sort.Search(len(src.salv), func(i int) bool { return bytes.Compare(src.salv[i].UserKey, prefix) >= 0 })
		}
		src.si = start
		return src
	}
	src.index = idx
	from := prefix
	if reverse {
		from = prefixSuccessor(prefix)
	}
	src.bi = firstBlockFor(idx, from, reverse)
	return src
}

// damaged records index entry i's block as unreadable: its keys lie after
// the previous block's separator, up to and including its own.
func (s *sstSource) damaged(i int) {
	d := damagedRange{CF: s.ref.CF, MaxSeq: s.ref.maxSeq()}
	if i > 0 {
		d.Lo = s.index[i-1].SepUserKey
	} else if s.ref.HaveManifestRange {
		d.Lo, d.LoIncl = s.ref.Smallest, true // the first block starts at the file's smallest key
	}
	if i < len(s.index) {
		d.Hi = s.index[i].SepUserKey
	}
	s.db.damaged = append(s.db.damaged, d)
}

// maxSeq is the newest sequence number the file can hold, or 0 if unknown.
func (r *rdbSSTRef) maxSeq() uint64 {
	if r.HaveSeqRange {
		return r.LargestSeq + 1
	}
	return 0
}

func (s *sstSource) inRange(uk []byte) int {
	if bytes.HasPrefix(uk, s.prefix) {
		return 0
	}
	c := bytes.Compare(uk, s.prefix)
	if s.reverse {
		c = -c
	}
	if c < 0 {
		return -1
	}
	return 1
}

func (s *sstSource) next() (rdbEntry, bool) {
	if s.useSalv {
		for s.si < len(s.salv) {
			e := s.salv[s.si]
			s.si++
			switch s.inRange(e.UserKey) {
			case -1:
				continue
			case 1:
				s.si = len(s.salv)
				return rdbEntry{}, false
			}
			return e, true
		}
		return rdbEntry{}, false
	}
	for !s.done {
		if s.cur == nil {
			if s.bi >= len(s.index) {
				s.done = true
				break
			}
			h := s.index[s.bi].H
			s.bi++
			data, err := s.ref.sst.readBlock(h)
			if err != nil {
				s.damaged(s.bi - 1)
				s.db.reportCorrupt(s.ref.sst.Name(), h.Off, fmt.Sprintf("data block (%s): %v", h, err))
				continue
			}
			s.cur = newBlockIter(data, false)
			s.blkOff = h.Off
			if s.cur.err != nil {
				s.damaged(s.bi - 1)
				s.db.reportCorrupt(s.ref.sst.Name(), h.Off, fmt.Sprintf("data block (%s): %v", h, s.cur.err))
				s.cur = nil
				continue
			}
		}
		if !s.cur.next() {
			if s.cur.err != nil {
				s.damaged(s.bi - 1)
				s.db.reportCorrupt(s.ref.sst.Name(), s.blkOff, fmt.Sprintf("data block at offset %d: %v - rest of the block skipped", s.blkOff, s.cur.err))
			}
			s.cur = nil
			continue
		}
		uk, seq, typ, ok := splitInternalKey(s.cur.key)
		if !ok {
			s.db.reportCorrupt(s.ref.sst.Name(), s.blkOff, fmt.Sprintf("data block at offset %d: key shorter than 8 bytes - rest of the block skipped", s.blkOff))
			s.cur = nil
			continue
		}
		switch s.inRange(uk) {
		case -1:
			continue
		case 1:
			s.done = true
			return rdbEntry{}, false
		}
		if s.ref.sst.External {
			seq = s.ref.sst.GlobalSeqno
		}
		return rdbEntry{UserKey: uk, Seq: seq, Type: typ, Value: s.cur.val,
			Origin: entryOrigin{File: s.name, Off: int64(s.blkOff)}}, true
	}
	return rdbEntry{}, false
}

// rdbCorruption is one problem met while scanning.
type rdbCorruption struct {
	File      string
	Off       uint64
	Reason    string
	Info      bool // nothing was lost (e.g. a salvage that recovered everything): warn, don't fail
	Truncated bool // the file ends early (an incomplete copy)
}

func (db *rdbDB) reportCorrupt(file string, off uint64, reason string) {
	db.report(rdbCorruption{File: file, Off: off, Reason: reason})
}

func (db *rdbDB) report(c rdbCorruption) {
	if db.onCorrupt != nil {
		db.onCorrupt(c)
	}
}

// mergeItem is one source's current entry in the merge heap. A source not
// opened yet sits in the heap as a placeholder at its smallest key.
type mergeItem struct {
	e           rdbEntry
	src         rdbSource
	placeholder *rdbSSTRef
	order       int
}

type mergeHeap struct {
	items   []*mergeItem
	reverse bool
}

func (h *mergeHeap) Len() int { return len(h.items) }
func (h *mergeHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if c := compareUserKeys(a.e.UserKey, b.e.UserKey, h.reverse); c != 0 {
		return c < 0
	}
	// At equal keys, open placeholders first (they may hold this key too),
	// then newest first.
	if (a.placeholder != nil) != (b.placeholder != nil) {
		return a.placeholder != nil
	}
	if a.e.Seq != b.e.Seq {
		return a.e.Seq > b.e.Seq
	}
	return a.order < b.order
}
func (h *mergeHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap) Push(x interface{}) { h.items = append(h.items, x.(*mergeItem)) }
func (h *mergeHeap) Pop() interface{} {
	it := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return it
}

// scanCF calls fn, in comparator order, with every user key starting with
// prefix in column family cf that has at least one version anywhere; fn
// returning false stops the scan.
func (db *rdbDB) scanCF(cf *rdbCF, prefix []byte, fn func(kv *rdbKeyVersions) bool) {
	h := &mergeHeap{reverse: cf.Reverse}
	order := 0
	push := func(it *mergeItem) {
		it.order = order
		order++
		heap.Push(h, it)
	}
	pullFrom := func(src rdbSource) {
		if e, ok := src.next(); ok {
			push(&mergeItem{e: e, src: src})
		}
	}

	// WAL entries.
	ms := &memSource{ents: cf.mem}
	if cf.Reverse {
		from := prefixSuccessor(prefix)
		ms.i = sort.Search(len(cf.mem), func(i int) bool {
			return from == nil || compareUserKeys(cf.mem[i].UserKey, from, true) > 0
		})
	} else {
		ms.i = sort.Search(len(cf.mem), func(i int) bool { return bytes.Compare(cf.mem[i].UserKey, prefix) >= 0 })
	}
	pullFrom(&prefixFilter{src: ms, prefix: prefix})

	// SST files: skip any whose key range can't hold the prefix; the rest
	// wait as placeholders until the merge reaches a key that sorts no
	// later than anything they can hold - the range's first possible key
	// in comparator order (the prefix itself, or for a reverse column
	// family its successor), or the file's own smallest key if later.
	lower := prefix
	if cf.Reverse {
		lower = prefixSuccessor(prefix)
	}
	for _, ref := range cf.SSTs {
		if ref.HaveManifestRange && !rangeMayHavePrefix(ref.Smallest, ref.Largest, prefix, cf.Reverse) {
			continue
		}
		if ref.Missing {
			d := damagedRange{CF: ref.CF, MaxSeq: ref.maxSeq()}
			if ref.HaveManifestRange {
				d.Lo, d.LoIncl, d.Hi = ref.Smallest, true, ref.Largest
			}
			db.damaged = append(db.damaged, d)
			db.reportCorrupt(filepath.Base(ref.Path), 0, "this live SST file (per the MANIFEST) is missing, and its key range overlaps this table - rows stored only there are lost, and older versions of them may show instead")
			continue
		}
		start := lower
		if ref.HaveManifestRange && (start == nil || compareUserKeys(ref.Smallest, start, cf.Reverse) > 0) {
			start = ref.Smallest
		}
		if start == nil {
			pullFrom(newSSTSource(db, ref, prefix, cf.Reverse))
			continue
		}
		push(&mergeItem{e: rdbEntry{UserKey: start}, placeholder: ref})
	}

	for h.Len() > 0 && !db.stop {
		top := heap.Pop(h).(*mergeItem)
		if top.placeholder != nil {
			src := newSSTSource(db, top.placeholder, prefix, cf.Reverse)
			pullFrom(src)
			continue
		}
		kv := &rdbKeyVersions{UserKey: top.e.UserKey, Versions: []rdbEntry{top.e}}
		pullFrom(top.src)
		for h.Len() > 0 {
			nx := h.items[0]
			if !bytes.Equal(nx.e.UserKey, kv.UserKey) {
				break
			}
			heap.Pop(h)
			if nx.placeholder != nil {
				pullFrom(newSSTSource(db, nx.placeholder, prefix, cf.Reverse))
				continue
			}
			kv.Versions = append(kv.Versions, nx.e)
			pullFrom(nx.src)
		}
		sort.SliceStable(kv.Versions, func(i, j int) bool { return kv.Versions[i].Seq > kv.Versions[j].Seq })
		if !fn(kv) {
			return
		}
	}
}

// keyRangeOverlapsPrefix reports whether the comparator-order range
// (lo, hi] (nil: unbounded) can hold a key starting with prefix.
func keyRangeOverlapsPrefix(lo, hi, prefix []byte, reverse bool) bool {
	a, b := lo, hi // bytewise low and high ends
	if reverse {
		a, b = hi, lo
	}
	succ := prefixSuccessor(prefix)
	if b != nil && bytes.Compare(b, prefix) < 0 {
		return false
	}
	if a != nil && succ != nil && bytes.Compare(a, succ) >= 0 {
		return false
	}
	return true
}

// prefixFilter stops a source at the first key past the prefix.
type prefixFilter struct {
	src    rdbSource
	prefix []byte
	done   bool
}

func (p *prefixFilter) next() (rdbEntry, bool) {
	if p.done {
		return rdbEntry{}, false
	}
	e, ok := p.src.next()
	if !ok || !bytes.HasPrefix(e.UserKey, p.prefix) {
		p.done = true
		return rdbEntry{}, false
	}
	return e, true
}

// rangeMayHavePrefix reports whether [smallest, largest] (comparator
// order) can contain a key starting with prefix.
func rangeMayHavePrefix(smallest, largest, prefix []byte, reverse bool) bool {
	lo, hi := smallest, largest
	if reverse {
		lo, hi = largest, smallest // bytewise order
	}
	if bytes.HasPrefix(lo, prefix) || bytes.HasPrefix(hi, prefix) {
		return true
	}
	// Bytewise: some key in [lo, hi] starts with prefix iff lo < succ(prefix) and hi >= prefix.
	succ := prefixSuccessor(prefix)
	return bytes.Compare(hi, prefix) >= 0 && (succ == nil || bytes.Compare(lo, succ) < 0)
}
