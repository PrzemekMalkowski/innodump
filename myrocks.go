// myrocks.go: extracting MyRocks (ENGINE=ROCKSDB) tables.
//
// MyRocks keeps every table of an instance in one RocksDB database, the
// datadir's .rocksdb/ directory (#rocksdb/ on MariaDB), read here through
// rocksdb_db.go. Each index of each table is a range of RocksDB keys
// sharing a 4-byte big-endian index number prefix; a table's rows are its
// primary key's range (or its hidden primary key's, for a table without
// one), one key/value pair per row:
//
//	key:   index number | PK columns, mem-comparable (myrocks_key.go)
//	value: [8-byte TTL timestamp] | NULL bitmap | [unpack_info] |
//	       every column not decodable from the key, in table order
//	       (NULLs omitted), in MySQL's own record format
//
// Which index numbers belong to which table is recorded in MyRocks' own
// data dictionary, kept as ordinary key/values in the "__system__" column
// family (Rdb_dict_manager in rdb_datadic.cc). The column definitions
// themselves are not in RocksDB at all: they come from the table's .sdi
// file (MySQL 8.0+, exactly as for MyISAM) or its .frm (5.6/5.7, MariaDB).
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MyRocks data dictionary record types (Rdb_key_def::DATA_DICT_TYPE).
const (
	mrDictDDLEntry      = 1
	mrDictIndexInfo     = 2
	mrDictCFDefinition  = 3
	mrDictDropOngoing   = 5
	mrDictIndexStats    = 6
	mrDictMaxIndexID    = 7
	mrDictCreateOngoing = 8
	mrDictAutoInc       = 9
	mrDictDroppedCF     = 10

	mrIndexPrimary       = 1
	mrIndexSecondary     = 2
	mrIndexHiddenPrimary = 3

	mrTTLFlag      = 1
	mrReverseCF    = 1 // CF_DEFINITION flag
	mrSystemCFName = "__system__"
)

// mrIndex is one index's entry in the MyRocks dictionary.
type mrIndex struct {
	CF, ID    uint32
	Type      byte
	KVVersion uint16
	Flags     uint32
	TTL       uint64
	Known     bool // an INDEX_INFO record was found
}

func (ix mrIndex) gl() uint64 { return uint64(ix.CF)<<32 | uint64(ix.ID) }

func (ix mrIndex) typeName() string {
	switch ix.Type {
	case mrIndexPrimary:
		return "PRIMARY"
	case mrIndexSecondary:
		return "secondary"
	case mrIndexHiddenPrimary:
		return "hidden PRIMARY"
	}
	return fmt.Sprintf("type %d", ix.Type)
}

// mrTableDef is one DDL_ENTRY record: one table, or one partition of a
// partitioned table, and its indexes in key order (a hidden primary key,
// if any, last).
type mrTableDef struct {
	RawName   string // "db.table" or "db.table#P#p0", filename-encoded as on disk
	DBDir     string // the schema's directory name (encoded)
	TableBase string // the table's file base name (encoded), partition suffix removed
	Partition string // "" or the partition suffix after the first '#' (e.g. "p#p0")
	Schema    string // decoded
	Table     string // decoded
	Indexes   []mrIndex
	Inferred  bool // not in the dictionary: matched by recoverOrphanTables
}

func (td *mrTableDef) pkIndex() (mrIndex, int, bool) {
	for i, ix := range td.Indexes {
		if ix.Type == mrIndexPrimary || ix.Type == mrIndexHiddenPrimary {
			return ix, i, true
		}
	}
	return mrIndex{}, -1, false
}

// mrDict is the part of the MyRocks dictionary this tool uses.
type mrDict struct {
	Defs     []*mrTableDef
	AutoInc  map[uint64]uint64 // gl index id -> next AUTO_INCREMENT value
	Dropping map[uint64]bool   // indexes whose DROP hasn't finished yet
	CFFlags  map[uint32]uint32
}

// loadMyRocksDict reads the dictionary out of the __system__ column family.
func loadMyRocksDict(db *rdbDB) (*mrDict, error) {
	sys := db.cfByName(mrSystemCFName)
	if sys == nil {
		return nil, errors.New("no __system__ column family - this RocksDB directory wasn't written by MyRocks, or its MANIFEST and system-CF SST files are all unreadable")
	}
	d := &mrDict{AutoInc: map[uint64]uint64{}, Dropping: map[uint64]bool{}, CFFlags: map[uint32]uint32{}}
	infos := map[uint64]mrIndex{}

	scan := func(typ uint32, fn func(key, val []byte)) {
		var p [4]byte
		binary.BigEndian.PutUint32(p[:], typ)
		db.scanCF(sys, p[:], func(kv *rdbKeyVersions) bool {
			if e, ok := kv.Current(sys); ok {
				fn(kv.UserKey[4:], e.Value)
			}
			return true
		})
	}

	scan(mrDictIndexInfo, func(key, val []byte) {
		if len(key) != 8 || len(val) < 2 {
			return
		}
		ix := mrIndex{CF: binary.BigEndian.Uint32(key), ID: binary.BigEndian.Uint32(key[4:]), Known: true}
		ver := binary.BigEndian.Uint16(val)
		v := val[2:]
		if ver >= 2 && len(v) >= 3 {
			ix.Type = v[0]
			ix.KVVersion = binary.BigEndian.Uint16(v[1:])
			v = v[3:]
		}
		switch {
		case ver >= 6 && len(v) >= 12: // INDEX_INFO_VERSION_FIELD_FLAGS
			ix.Flags = binary.BigEndian.Uint32(v)
			ix.TTL = binary.BigEndian.Uint64(v[4:])
		case ver == 5 && len(v) >= 8: // INDEX_INFO_VERSION_TTL
			ix.TTL = binary.BigEndian.Uint64(v)
			if ix.KVVersion >= 13 && ix.TTL > 0 {
				ix.Flags = mrTTLFlag
			}
		}
		infos[ix.gl()] = ix
	})
	scan(mrDictCFDefinition, func(key, val []byte) {
		if len(key) == 4 && len(val) >= 6 {
			d.CFFlags[binary.BigEndian.Uint32(key)] = binary.BigEndian.Uint32(val[2:])
		}
	})
	scan(mrDictAutoInc, func(key, val []byte) {
		if len(key) == 8 && len(val) == 10 {
			d.AutoInc[uint64(binary.BigEndian.Uint32(key))<<32|uint64(binary.BigEndian.Uint32(key[4:]))] = binary.BigEndian.Uint64(val[2:])
		}
	})
	scan(mrDictDropOngoing, func(key, val []byte) {
		if len(key) == 8 {
			d.Dropping[uint64(binary.BigEndian.Uint32(key))<<32|uint64(binary.BigEndian.Uint32(key[4:]))] = true
		}
	})
	scan(mrDictDDLEntry, func(key, val []byte) {
		if len(val) < 2 {
			return
		}
		td := &mrTableDef{RawName: string(key)}
		dot := strings.IndexByte(td.RawName, '.')
		if dot < 0 {
			return
		}
		td.DBDir = td.RawName[:dot]
		td.TableBase = td.RawName[dot+1:]
		if h := strings.Index(strings.ToLower(td.TableBase), "#p#"); h >= 0 {
			td.Partition = td.TableBase[h+1:]
			td.TableBase = td.TableBase[:h]
		}
		td.Schema = decodeMySQLFilename(td.DBDir)
		td.Table = decodeMySQLFilename(td.TableBase)
		for v := val[2:]; len(v) >= 8; v = v[8:] {
			ix := mrIndex{CF: binary.BigEndian.Uint32(v), ID: binary.BigEndian.Uint32(v[4:])}
			if info, ok := infos[ix.gl()]; ok {
				ix = info
			}
			td.Indexes = append(td.Indexes, ix)
		}
		d.Defs = append(d.Defs, td)
	})
	if len(d.Defs) == 0 && len(infos) == 0 {
		userData := false
		for _, cf := range db.CFs {
			if cf.Name != mrSystemCFName && (len(cf.SSTs) > 0 || len(cf.mem) > 0) {
				userData = true
			}
		}
		if len(d.CFFlags) > 0 && !userData {
			return d, errMyRocksNoTables
		}
		return d, errors.New("the MyRocks data dictionary (in the __system__ column family) is empty or unreadable")
	}
	return d, nil
}

// errMyRocksNoTables: the dictionary reads fine and holds no tables, and
// no column family holds any data either - nothing to extract, nothing
// damaged.
var errMyRocksNoTables = errors.New("this RocksDB directory holds no MyRocks tables: its dictionary is readable but lists none, and no column family besides __system__ holds any data (none were ever created, or all were dropped)")

// mrLogicalTable groups the dictionary entries of one table: just one, or
// one per partition.
type mrLogicalTable struct {
	Schema, Table    string
	DBDir, TableBase string
	Parts            []*mrTableDef // a partitioned table's partitions, sorted by suffix; one element otherwise
	Partitioned      bool
}

func (lt *mrLogicalTable) fullName() string { return lt.Schema + "." + lt.Table }

// logicalTables groups d's entries by table, sorted by name.
func (d *mrDict) logicalTables() []*mrLogicalTable {
	byKey := map[string]*mrLogicalTable{}
	var out []*mrLogicalTable
	for _, td := range d.Defs {
		k := td.DBDir + "\x00" + td.TableBase
		lt := byKey[k]
		if lt == nil {
			lt = &mrLogicalTable{Schema: td.Schema, Table: td.Table, DBDir: td.DBDir, TableBase: td.TableBase}
			byKey[k] = lt
			out = append(out, lt)
		}
		lt.Parts = append(lt.Parts, td)
		if td.Partition != "" {
			lt.Partitioned = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].fullName() < out[j].fullName() })
	return out
}

// findRocksDBDir returns datadir's MyRocks directory, or "".
func findRocksDBDir(datadir string) string {
	for _, n := range []string{".rocksdb", "#rocksdb"} {
		p := filepath.Join(datadir, n)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	return ""
}

// looksLikeRocksDBDir reports whether dir holds a RocksDB database.
func looksLikeRocksDBDir(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		n := e.Name()
		if n == "CURRENT" || strings.HasPrefix(n, "MANIFEST-") || strings.HasSuffix(n, ".sst") {
			return true
		}
	}
	return false
}

// --- schema ---

// findMyRocksSchema locates the table definition for lt under datadir: a
// standalone .sdi (8.0+) or a .frm.
func findMyRocksSchema(datadir string, lt *mrLogicalTable) (*ddTableJSON, uint32, string, error) {
	dir := filepath.Join(datadir, lt.DBDir)
	sdiPath, err := findStandaloneSDI(dir, lt.TableBase)
	if err != nil && !os.IsNotExist(err) {
		return nil, 0, "", err
	}
	if sdiPath != "" {
		raw, versionID, err := loadStandaloneSDI(sdiPath)
		if err != nil {
			return nil, 0, "", err
		}
		if !strings.EqualFold(raw.Engine, "ROCKSDB") {
			return nil, 0, "", fmt.Errorf("%s describes an %s table, not a ROCKSDB one - the MyRocks dictionary still lists it, perhaps from before an ALTER TABLE ... ENGINE", filepath.Base(sdiPath), raw.Engine)
		}
		return raw, versionID, sdiPath, nil
	}
	frmPath := filepath.Join(dir, lt.TableBase+".frm")
	if _, err := os.Stat(frmPath); err == nil {
		raw, err := parseRocksDBFRM(frmPath)
		if err != nil {
			return nil, 0, "", fmt.Errorf("reading %s: %w", filepath.Base(frmPath), err)
		}
		return raw, 0, frmPath, nil
	}
	return nil, 0, "", fmt.Errorf("no schema found for %s - expected a %s_<id>.sdi (MySQL 8.0+) or a %s.frm (5.6/5.7, MariaDB) in %s",
		lt.fullName(), lt.TableBase, lt.TableBase, dir)
}

// BuildRocksDBTable resolves a MyRocks table's SDI/.frm definition. The
// table's clustered index is whatever the MyRocks dictionary says it is
// (pkPos: the position of its PRIMARY index among the table's indexes, or
// -1 for a hidden primary key) - which can be a UNIQUE NOT NULL key the
// server promoted when no PRIMARY KEY was declared.
func BuildRocksDBTable(raw *ddTableJSON, pkPos int) (*Table, error) {
	flat := *raw
	flat.PartitionType = 0
	flat.Partitions = nil
	promoted := false
	if pkPos >= 0 && pkPos < len(flat.Indexes) && flat.Indexes[pkPos].Type != ddIndexPrimary {
		// A promoted UNIQUE key: move it first, marked PRIMARY, so
		// buildPlainTable treats it as the clustered index.
		idx := append([]ddIndexJSON(nil), flat.Indexes...)
		pk := idx[pkPos]
		pk.Type = ddIndexPrimary
		idx = append(idx[:pkPos], idx[pkPos+1:]...)
		flat.Indexes = append([]ddIndexJSON{pk}, idx...)
		promoted = true
	}
	t, err := buildPlainTable(&flat, "ROCKSDB")
	if err != nil {
		return nil, err
	}
	t.PKIsUniqueKey = promoted
	return t, nil
}

// --- physical layout ---

type mrField struct {
	Col      *Column
	NullBit  int // -1: NOT NULL
	Storage  mrStorage
	PackLen  int
	LenBytes int // VARCHAR / BLOB length prefix bytes
	Virtual  bool
	Instant  bool // added by INSTANT ADD COLUMN: older rows may end before it
	KeyPart  int  // index into mrLayout.PK, or -1
}

// mrLayout is one table's (or partition's) row layout.
type mrLayout struct {
	Index       mrIndex
	Def         *mrTableDef
	CF          *rdbCF
	Prefix      []byte
	Fields      []mrField
	NullBytes   int
	PK          []mrKeyPart
	HiddenPK    bool
	TTL         bool
	MaybeUnpack bool
	fieldIdx    map[*Column]int
	strict      bool // decodeRow also insists every value byte is accounted for (orphan matching)
}

// buildMyRocksLayout mirrors Rdb_converter::setup_field_encoders and
// Rdb_key_def::setup for t's primary index ix.
func buildMyRocksLayout(t *Table, raw *ddTableJSON, ix mrIndex, def *mrTableDef, db *rdbDB, rules mrRules) (*mrLayout, error) {
	lay := &mrLayout{Index: ix, Def: def, fieldIdx: map[*Column]int{}}
	lay.CF = db.CFs[ix.CF]
	if lay.CF == nil {
		return nil, fmt.Errorf("its primary index lives in column family %d, which this RocksDB directory doesn't have (anymore)", ix.CF)
	}
	lay.Prefix = make([]byte, 4)
	binary.BigEndian.PutUint32(lay.Prefix, ix.ID)
	lay.HiddenPK = ix.Type == mrIndexHiddenPrimary
	lay.TTL = ix.Flags&mrTTLFlag != 0
	if ix.Known && ix.Type != mrIndexPrimary && ix.Type != mrIndexHiddenPrimary {
		return nil, fmt.Errorf("index %d is a %s index, not a primary one", ix.ID, ix.typeName())
	}

	instantCols := -1
	if v, ok := sePropString(raw.SePrivateData)["instant_col"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			instantCols = n
		}
	}

	nullBit := 0
	for i, c := range t.Columns {
		pl, err := packLength(&c.raw)
		if err != nil {
			return nil, err
		}
		f := mrField{Col: c, NullBit: -1, PackLen: int(pl), Virtual: c.IsVirtual, KeyPart: -1, Storage: mrStoreAll}
		switch {
		case c.DDType == ddVarchar:
			f.LenBytes = 1
			if c.CharLength >= 256 {
				f.LenBytes = 2
			}
		case isBlobFamily(c.DDType) || c.DDType == ddJSON || c.DDType == ddGeometry || c.DDType == ddVector:
			f.LenBytes = int(pl) - 8
		}
		if c.IsNullable {
			f.NullBit = nullBit
			nullBit++
		}
		if instantCols >= 0 && i >= instantCols {
			f.Instant = true
		}
		lay.fieldIdx[c] = len(lay.Fields)
		lay.Fields = append(lay.Fields, f)
	}
	lay.NullBytes = (nullBit + 7) / 8

	if !lay.HiddenPK {
		for kpNo, pf := range t.PKFields {
			fi, ok := lay.fieldIdx[pf.Col]
			if !ok {
				return nil, fmt.Errorf("primary key column %q not found", pf.Col.Name)
			}
			f := &lay.Fields[fi]
			keyLen := f.PackLen
			switch {
			case pf.PrefixLen > 0:
				keyLen = int(pf.PrefixLen)
			case f.Col.DDType == ddVarchar || f.Col.DDType == ddString || f.Col.DDType == ddVarString:
				keyLen = int(f.Col.CharLength)
			}
			kp, err := setupKeyPart(f.Col, keyLen, f.PackLen, int(ix.KVVersion), rules)
			if err != nil {
				return nil, err
			}
			kp.FieldIdx = fi
			lay.PK = append(lay.PK, kp)
			// Rdb_converter::get_storage_type: only the first key part
			// naming a column decides its storage.
			if f.KeyPart < 0 {
				f.KeyPart = kpNo
				f.Storage = kp.Storage
				if kp.Storage == mrStoreSome {
					lay.MaybeUnpack = true
				}
			}
		}
	}
	return lay, nil
}

// decodeRow turns one RocksDB key/value pair into a row.
func (lay *mrLayout) decodeRow(key, value []byte, outCols []*Column, format outputFormat, now uint64) (row *Row, expired bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			row, err = nil, fmt.Errorf("panic decoding row: %v", r)
		}
	}()
	if !bytes.HasPrefix(key, lay.Prefix) {
		return nil, false, errors.New("key doesn't start with the index number")
	}
	vr := &mrReader{b: value}
	if lay.TTL {
		ts, ok := vr.read(8)
		if !ok {
			return nil, false, errors.New("value too short for its TTL timestamp")
		}
		if lay.Index.TTL > 0 && now > 0 && binary.BigEndian.Uint64(ts)+lay.Index.TTL <= now {
			expired = true
		}
	}
	nulls, ok := vr.read(lay.NullBytes)
	if !ok {
		return nil, false, errors.New("value too short for its NULL bitmap")
	}
	var unp *mrReader
	if lay.MaybeUnpack {
		hdr, ok := vr.read(3)
		if !ok || (hdr[0] != 0x02 && hdr[0] != 0x03) {
			return nil, false, errors.New("value has no unpack_info header where the table's layout expects one")
		}
		n := int(binary.BigEndian.Uint16(hdr[1:]))
		body, ok := vr.read(n - 3)
		if n < 3 || !ok {
			return nil, false, fmt.Errorf("unpack_info length %d runs past the value", n)
		}
		unp = &mrReader{b: body}
	}

	raws := make([][]byte, len(lay.Fields))
	isNull := make([]bool, len(lay.Fields))
	have := make([]bool, len(lay.Fields))

	kr := &mrReader{b: key[len(lay.Prefix):]}
	if lay.HiddenPK {
		if _, ok := kr.read(8); !ok {
			return nil, false, errors.New("hidden primary key shorter than 8 bytes")
		}
	}
	for i := range lay.PK {
		kp := &lay.PK[i]
		f := &lay.Fields[kp.FieldIdx]
		if kp.Storage == mrStoreAll || f.KeyPart != i {
			if err := kp.skipPart(kr, unp); err != nil {
				return nil, false, fmt.Errorf("key column %q: %w", kp.Col.Name, err)
			}
			continue
		}
		raw, null, err := kp.decodePart(kr, unp)
		if err != nil {
			return nil, false, fmt.Errorf("key column %q: %w", kp.Col.Name, err)
		}
		raws[kp.FieldIdx], isNull[kp.FieldIdx], have[kp.FieldIdx] = raw, null, true
	}
	if kr.remaining() != 0 {
		return nil, false, fmt.Errorf("%d unexpected byte(s) after the last key column", kr.remaining())
	}

	for i := range lay.Fields {
		f := &lay.Fields[i]
		if f.Storage != mrStoreAll || f.Virtual {
			continue
		}
		if f.Instant && vr.remaining() == 0 {
			// A row written before INSTANT ADD COLUMN: the column's
			// default applies.
			if f.Col.InstantDefaultIsNull || f.Col.InstantDefault == nil {
				isNull[i], have[i] = true, true
			} else {
				raws[i], have[i] = f.Col.InstantDefault, true
			}
			continue
		}
		if f.NullBit >= 0 && nulls[f.NullBit/8]&(1<<(uint(f.NullBit)%8)) != 0 {
			isNull[i], have[i] = true, true
			continue
		}
		var b []byte
		switch {
		case f.LenBytes > 0:
			lb, ok := vr.read(f.LenBytes)
			if !ok {
				return nil, false, fmt.Errorf("column %q: value ends inside its length prefix", f.Col.Name)
			}
			n := int(decodeUnsignedLE(lb))
			if f.Col.DDType == ddVarchar && n > int(f.Col.CharLength) {
				return nil, false, fmt.Errorf("column %q: length %d exceeds the column's %d bytes", f.Col.Name, n, f.Col.CharLength)
			}
			if b, ok = vr.read(n); !ok {
				return nil, false, fmt.Errorf("column %q: value ends inside its %d data bytes", f.Col.Name, n)
			}
		default:
			if b, ok = vr.read(f.PackLen); !ok {
				return nil, false, fmt.Errorf("column %q: value ends inside its %d bytes", f.Col.Name, f.PackLen)
			}
		}
		raws[i], have[i] = b, true
	}

	if lay.strict {
		// Only a row checksum (rocksdb_store_row_debug_checksums: a 0x01
		// tag and two CRC-32s) may follow the last column.
		if r := vr.remaining(); r != 0 && !(r == 9 && value[len(value)-9] == 0x01) {
			return nil, false, fmt.Errorf("%d byte(s) left over after the last column", r)
		}
	}
	values := make([]string, len(outCols))
	for i, c := range outCols {
		fi, ok := lay.fieldIdx[c]
		if !ok || !have[fi] {
			return nil, false, fmt.Errorf("column %q wasn't decoded (internal error)", c.Name)
		}
		if isNull[fi] {
			values[i] = nullLiteral(format)
			continue
		}
		lit, err := decodeMyRocksField(c, raws[fi], format)
		if err != nil {
			return nil, false, err
		}
		values[i] = lit
	}
	return &Row{Values: values}, expired, nil
}

// decodeMyRocksField renders one column's MySQL-record-format bytes - the
// format MyRocks stores (and myrocks_key.go reconstructs) - as a SQL
// literal or TSV field: the same format MyISAM uses, apart from BIT.
func decodeMyRocksField(c *Column, raw []byte, format outputFormat) (string, error) {
	if c.DDType == ddBit {
		if format == formatTSV {
			return base64Field(raw), nil
		}
		return fmt.Sprintf("b'%s'", strconv.FormatUint(decodeUnsignedBE(raw), 2)), nil
	}
	return decodeMyISAMField(c, raw, format)
}

// --- walking ---

// mrWalkStats are the extra counts a MyRocks walk reports.
type mrWalkStats struct {
	Expired int // TTL-expired rows left out (the server hides them too)
	// Stale counts rows in the key range of a damaged block or missing
	// file, which may show an older version than the server had.
	Stale int
}

// myrocksRowWalker streams every live row (or, with deletedOnly, every
// deleted row still recoverable) of the table whose partitions' layouts
// are lays, in primary key order, partition by partition.
func myrocksRowWalker(db *rdbDB, lays []*mrLayout, outCols []*Column, format outputFormat, skipCorrupted, deletedOnly bool, stats *mrWalkStats) RowWalker {
	return RowWalker{
		ProgressTotal: 0,
		Walk: func(onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
			// The merge itself (scanCF) runs sequentially, which keeps the
			// row order - and where each damaged-block report falls among
			// the rows - deterministic. Decoding and rendering rows, the
			// CPU-heavy part, runs in parallel batches, handed to fn in the
			// original order (as WalkRows does for InnoDB).
			var batch []mrPending
			stopped := false
			now := uint64(time.Now().Unix())
			flush := func() {
				if len(batch) == 0 {
					return
				}
				if !stopped {
					decodeMyRocksBatch(batch, outCols, format, now)
					for i := range batch {
						p := &batch[i]
						var roe RowOrError
						switch {
						case p.err != nil:
							roe = RowOrError{PageNo: uint32(p.n), Err: p.err, Label: fmt.Sprintf("key 0x%X (%s)", p.key, p.origin.String())}
						case p.expired && !deletedOnly:
							stats.Expired++
							continue
						default:
							roe = RowOrError{PageNo: uint32(p.n), Row: p.row}
						}
						if !fn(roe) {
							stopped, db.stop = true, true
							break
						}
					}
				}
				batch = batch[:0]
			}

			var fatal error
			db.onCorrupt = func(c rdbCorruption) {
				flush() // every row before the damage goes out first
				if !skipCorrupted && !c.Info {
					if fatal == nil {
						fatal = fmt.Errorf("%s: %s (pass --skip-corrupted to skip damaged blocks and carry on)", c.File, c.Reason)
						db.stop = true
					}
					return
				}
				if onCorrupt != nil {
					onCorrupt(CorruptPage{Label: "RocksDB file " + c.File, Reason: c.Reason, Info: c.Info, Truncated: c.Truncated})
				}
			}
			db.damaged = nil
			defer func() { db.onCorrupt, db.stop, db.damaged = nil, false, nil }()

			n := 0
			for _, lay := range lays {
				db.scanCF(lay.CF, lay.Prefix, func(kv *rdbKeyVersions) bool {
					var e rdbEntry
					var ok bool
					if deletedOnly {
						e, ok = kv.Deleted(lay.CF)
					} else {
						e, ok = kv.Current(lay.CF)
					}
					if !ok {
						return true
					}
					n++
					if len(db.damaged) > 0 && db.possiblyStale(lay.CF, kv.UserKey, e.Seq) {
						stats.Stale++
					}
					batch = append(batch, mrPending{lay: lay, key: kv.UserKey, val: e.Value, origin: e.Origin, n: n})
					if len(batch) >= mrBatchRows {
						flush()
					}
					return !stopped
				})
				flush()
				if fatal != nil {
					return fatal
				}
				if stopped {
					break
				}
			}
			return fatal
		},
	}
}

const mrBatchRows = 2048

// mrPending is one row waiting to be decoded, and then its result.
type mrPending struct {
	lay      *mrLayout
	key, val []byte
	origin   entryOrigin
	n        int

	row     *Row
	expired bool
	err     error
}

// decodeMyRocksBatch decodes batch's rows across every available core.
func decodeMyRocksBatch(batch []mrPending, outCols []*Column, format outputFormat, now uint64) {
	workers := runtime.GOMAXPROCS(0)
	if workers > len(batch)/64 {
		workers = len(batch)/64 + 1
	}
	var wg sync.WaitGroup
	chunk := (len(batch) + workers - 1) / workers
	for start := 0; start < len(batch); start += chunk {
		end := start + chunk
		if end > len(batch) {
			end = len(batch)
		}
		wg.Add(1)
		go func(part []mrPending) {
			defer wg.Done()
			for i := range part {
				p := &part[i]
				p.row, p.expired, p.err = p.lay.decodeRow(p.key, p.val, outCols, format, now)
			}
		}(batch[start:end])
	}
	wg.Wait()
}
