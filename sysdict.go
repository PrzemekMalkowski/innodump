// sysdict.go: reads just enough of InnoDB's own internal data dictionary
// (SYS_TABLES, SYS_INDEXES) to answer one question this tool needs when a
// pre-8.0 table's data isn't in its own file-per-table .ibd: which
// tablespace, and which root page, does its clustered index actually live
// at? This is the classic InnoDB dictionary every MariaDB server still
// maintains for every table regardless of innodb_file_per_table (MySQL
// 8.0 replaced it with the SDI-based data dictionary this tool otherwise
// relies on - see schema.go/sdi.go - which is why this file's own path
// only ever runs for a pre-8.0-style, SDI-less tablespace).
//
// A user table's own SQL-level schema (column names/types/nullability,
// secondary indexes) still comes entirely from its .frm file exactly as
// for any other pre-8.0 table (see frm.go) - .frm doesn't care where the
// table's InnoDB data physically lives, file-per-table or shared. This
// file supplies only the two physical facts a .frm can't carry:
// SYS_TABLES.SPACE (which tablespace) and SYS_INDEXES.PAGE_NO (the
// clustered index's root page within it) - see FindTableLocation, and
// parseFRM's loc parameter (frm.go) for where they replace the "root page
// 3" convention a table's own dedicated file always follows instead.
//
// Cross-checked against MariaDB 10.6's own storage/innobase/include/
// dict0boot.h (the dictionary header page layout, and every system
// table's exact field order/count) and dict0crea.cc (confirming each
// field's on-disk width/encoding - a plain big-endian mach_write_to_N
// integer, not the sign-flipped encoding a user table's own DATA_INT
// columns use), and validated against a real MariaDB 10.6.17 ibdata1
// with innodb_file_per_table=0. These dictionary tables have always been
// stored in InnoDB's original REDUNDANT ("old-style") record format,
// regardless of the server's own default row format, so redundant.go's
// generic field-offset decoder applies unchanged; leftmostLeaf/
// nonLeafChildPage (btree.go) only need a field *count* for that format,
// so fakeSysIndexTable below fakes only that much of a *Table for them -
// never anything touching a real Column.
//
// Only the system tablespace itself (space id 0 - DICT_HDR_SPACE) is
// supported: a table recorded in SYS_TABLES with a different SPACE lives
// in its own file-per-table .ibd (root page 3 applies there as usual) or
// in a separate general tablespace file this tool has no way to reach
// from here (the dictionary itself only ever lives in the system
// tablespace) - both are detected and reported rather than mis-decoded.
package main

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Dictionary header (page dictHdrPageNo of the system tablespace) field
// offsets - dict0boot.h's DICT_HDR_* constants, relative to filPageData
// (DICT_HDR itself sits at FSEG_PAGE_DATA == FIL_PAGE_DATA == filPageData).
const (
	dictHdrPageNo     = 7                // FSP_DICT_HDR_PAGE_NO
	dictHdrTablesOff  = filPageData + 32 // root of SYS_TABLES clustered index
	dictHdrIndexesOff = filPageData + 44 // root of SYS_INDEXES clustered index
)

// SYS_TABLES/SYS_INDEXES field counts and positions - dict0boot.h's
// dict_fld_sys_tables_enum/dict_fld_sys_indexes_enum (each includes the
// two hidden DB_TRX_ID/DB_ROLL_PTR system columns every InnoDB table has,
// alongside NAME/ID/etc.).
const (
	dictTablesID  = 1 // DICT_TABLES_ID, for fakeSysIndexTable's error messages only
	dictIndexesID = 3 // DICT_INDEXES_ID, ditto

	sysTablesNFields = 10
	sysTablesFldName = 0
	sysTablesFldID   = 3
	// SYS_TABLES.SPACE (field 9) isn't read: SYS_INDEXES.SPACE (below) is
	// authoritative for where the clustered index's own PAGE_NO actually
	// is, and the two are always consistent for a real table.

	sysIndexesNFields    = 10
	sysIndexesFldTableID = 0
	sysIndexesFldID      = 1 // unlike SYS_TABLES, ID comes right after TABLE_ID here, before DB_TRX_ID/DB_ROLL_PTR
	sysIndexesFldName    = 4
	sysIndexesFldType    = 6
	sysIndexesFldSpace   = 7
	sysIndexesFldPageNo  = 8
)

// dictIndexClustered is SYS_INDEXES.TYPE's clustered-index bit
// (data0type.h's DICT_CLUSTERED).
const dictIndexClustered = 1

// TableLocation is what FindTableLocation looks up for one "schema/table":
// which tablespace its clustered index (and so its row data) lives in,
// and that index's own id/root page.
type TableLocation struct {
	TableID  uint64
	SpaceID  uint32
	IndexID  uint64
	RootPage uint32
}

// fakeSysIndexTable builds just enough of a *Table for leftmostLeaf/
// nonLeafChildPage (btree.go) to walk one of InnoDB's own internal
// dictionary indexes - both are always REDUNDANT-format, and that format's
// decoder only ever needs a field *count*, never a real Column (see
// redundant.go's decodeFieldRangesOld) - nUniq is that index's own key
// field count (1 for SYS_TABLES, keyed on NAME; 2 for SYS_INDEXES, keyed
// on TABLE_ID+ID).
func fakeSysIndexTable(name string, indexID uint64, rootPage uint32, nUniq int) *Table {
	dummy := make([]*IndexField, nUniq)
	for i := range dummy {
		dummy[i] = &IndexField{Col: &Column{}}
	}
	return &Table{
		IndexName:      name,
		IndexID:        indexID,
		RootPage:       rootPage,
		HasExplicitPK:  true,
		PKFields:       dummy,
		PhysicalFields: dummy,
	}
}

// decodeFieldRangesOldN is decodeFieldRangesOld's field-count-only form,
// for a record with no *IndexField model of its own to hand it - it only
// ever reads len(fields) (see its own comment), so nFields dummy entries
// serve exactly as well as the real thing.
func decodeFieldRangesOldN(page []byte, recOff uint32, nFields int) []fieldRange {
	return decodeFieldRangesOld(page, recOff, make([]*IndexField, nFields))
}

// dictHeaderRootPages reads the dictionary header (page dictHdrPageNo of
// the system tablespace) and returns SYS_TABLES' and SYS_INDEXES' own
// clustered index root pages.
func dictHeaderRootPages(sp *Space) (sysTablesRoot, sysIndexesRoot uint32, err error) {
	raw, err := sp.ReadPage(dictHdrPageNo)
	if err != nil {
		return 0, 0, err
	}
	if pc := checkPage(raw, sp.Compressed, sp.Flags.fullCRC32); !pc.OK {
		return 0, 0, fmt.Errorf("dictionary header page %d: %s", dictHdrPageNo, pc.Reason)
	}
	return binary.BigEndian.Uint32(raw[dictHdrTablesOff:]), binary.BigEndian.Uint32(raw[dictHdrIndexesOff:]), nil
}

// walkOldIndexLeaves calls fn for every non-deleted record on every leaf
// page of a REDUNDANT-format internal dictionary index (SYS_TABLES or
// SYS_INDEXES), in key order, stopping early if fn returns false. fn
// receives the page bytes and the record's raw field byte ranges (see
// decodeFieldRangesOldN) - the caller already knows exactly what each one
// means (this file's own *NFields/Fld* constants). The system tablespace
// is never ROW_FORMAT=COMPRESSED, so - unlike WalkRows - there's no
// decompression step here at all.
func walkOldIndexLeaves(sp *Space, t *Table, nFields int, fn func(page []byte, ranges []fieldRange) bool) error {
	leaf, err := leftmostLeaf(sp, t)
	if err != nil {
		return fmt.Errorf("walking %s: %w", t.IndexName, err)
	}
	pageNo := leaf
	for pageNo != filNull {
		raw, err := sp.ReadPage(pageNo)
		if err != nil {
			return err
		}
		if pc := checkPage(raw, sp.Compressed, sp.Flags.fullCRC32); !pc.OK {
			return fmt.Errorf("%s page %d: %s", t.IndexName, pageNo, pc.Reason)
		}
		if filType(raw) != filPageIndex {
			return fmt.Errorf("%s page %d: expected an INDEX page, got type %d", t.IndexName, pageNo, filType(raw))
		}
		cont := true
		walkRecords(raw, sp.PageSize, func(recOff uint32) bool {
			if recDeleted(raw, recOff) {
				return true
			}
			if !fn(raw, decodeFieldRangesOldN(raw, recOff, nFields)) {
				cont = false
				return false
			}
			return true
		})
		if !cont {
			return nil
		}
		pageNo = filNextPage(raw)
	}
	return nil
}

// staleDictHint is appended to FindTableLocation's "not found"/listing
// errors: a table missing from this scan is at least as often a *stale*
// SYS_TABLES/SYS_INDEXES page as a genuinely nonexistent table - a
// server's own page cleaner can leave a just-created table's dictionary
// rows dirty (only in the buffer pool/redo log, not yet written back to
// the file this tool reads) for a long time on an otherwise-idle
// instance, and FLUSH TABLES does not force that write-back. Confirmed
// against a real MariaDB 11.8.6 instance: a table created moments earlier
// was invisible here (Innodb_buffer_pool_pages_dirty > 0, unmoved by
// FLUSH TABLES) until forcing a flush the way this hint suggests, with no
// server restart - see the README's "Shared/system tablespace" section.
const staleDictHint = " If a table you know exists isn't listed, its dictionary rows may simply " +
	"not be flushed to disk yet, rather than not existing: stop the server cleanly first if you can " +
	"(a normal shutdown always flushes), or - without stopping it - note the current " +
	"innodb_max_dirty_pages_pct(_lwm), set both to 0, and wait for " +
	"SHOW GLOBAL STATUS LIKE 'Innodb_buffer_pool_pages_dirty' to reach 0 before copying/reading the file again " +
	"(restore the original settings afterward - leaving them at 0 forces the server to flush far more " +
	"aggressively than normal from then on)."

// FindTableLocation looks up "schema/table" (InnoDB's own dictionary name
// form - a dot is also accepted and converted) in sp's internal data
// dictionary (sp must be the system tablespace itself, space id 0 - see
// this file's package comment) and returns where its clustered index -
// and so its row data - actually lives. want == "" (or a name found
// nowhere in SYS_TABLES) returns an error listing every table name found
// in this tablespace's own dictionary, capped for readability.
func FindTableLocation(sp *Space, want string) (*TableLocation, error) {
	sysTablesRoot, sysIndexesRoot, err := dictHeaderRootPages(sp)
	if err != nil {
		return nil, fmt.Errorf("reading dictionary header: %w", err)
	}

	want = strings.Replace(want, ".", "/", 1)

	sysTables := fakeSysIndexTable("SYS_TABLES", dictTablesID, sysTablesRoot, 1)
	var tableID uint64
	found := false
	var names []string
	walkErr := walkOldIndexLeaves(sp, sysTables, sysTablesNFields, func(page []byte, ranges []fieldRange) bool {
		nr := ranges[sysTablesFldName]
		if nr.Null {
			return true
		}
		name := string(page[nr.Start:nr.End])
		if want != "" && name == want {
			tableID = decodeUnsignedBE(fieldBytes(page, ranges[sysTablesFldID]))
			found = true
			return false
		}
		if len(names) < 500 {
			names = append(names, name)
		}
		return true
	})
	if walkErr != nil {
		return nil, fmt.Errorf("reading SYS_TABLES: %w", walkErr)
	}
	if !found {
		if want == "" {
			return nil, fmt.Errorf("this is a shared/system tablespace - pass --table schema.table (InnoDB's own dictionary lists %d table(s) here: %s) to pick one.%s",
				len(names), joinNamesForError(names), staleDictHint)
		}
		return nil, fmt.Errorf("table %q not found in this tablespace's internal data dictionary (SYS_TABLES); it lists %d table(s): %s.%s",
			want, len(names), joinNamesForError(names), staleDictHint)
	}

	sysIndexes := fakeSysIndexTable("SYS_INDEXES", dictIndexesID, sysIndexesRoot, 2)
	var indexID uint64
	var idxSpaceID uint32
	var rootPage uint32
	haveClust := false
	walkErr = walkOldIndexLeaves(sp, sysIndexes, sysIndexesNFields, func(page []byte, ranges []fieldRange) bool {
		// SYS_INDEXES is keyed on (TABLE_ID, ID) in that order: rows for
		// our table (if any) form one contiguous run in this scan.
		rowTableID := decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldTableID]))
		if rowTableID < tableID {
			return true // haven't reached this table's rows yet
		}
		if rowTableID > tableID {
			return false // past them without finding a clustered index
		}
		typ := decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldType]))
		if typ&dictIndexClustered == 0 {
			return true // one of this table's secondary indexes - not what we need
		}
		indexID = decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldID]))
		rootPage = uint32(decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldPageNo])))
		idxSpaceID = uint32(decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldSpace])))
		haveClust = true
		return false
	})
	if walkErr != nil {
		return nil, fmt.Errorf("reading SYS_INDEXES: %w", walkErr)
	}
	if !haveClust {
		return nil, fmt.Errorf("table %q (internal id %d) has no clustered index recorded in SYS_INDEXES (corrupt dictionary?)", want, tableID)
	}

	return &TableLocation{TableID: tableID, SpaceID: idxSpaceID, IndexID: indexID, RootPage: rootPage}, nil
}

// ListTableLocations walks sp's own internal dictionary (SYS_TABLES, then
// SYS_INDEXES - sp must be the system tablespace, space id 0, same
// requirement as FindTableLocation) once and returns every table's own
// "schema/table" name mapped to where its clustered index - and so its
// row data - lives. This is the bulk-scan counterpart to
// FindTableLocation's single "--table schema.table" lookup: --source-dir
// uses it to resolve every .frm file it finds with no .ibd of its own
// (innodb_file_per_table=0) in one pass, rather than re-walking both
// dictionary indexes from scratch for each one.
func ListTableLocations(sp *Space) (map[string]*TableLocation, error) {
	sysTablesRoot, sysIndexesRoot, err := dictHeaderRootPages(sp)
	if err != nil {
		return nil, fmt.Errorf("reading dictionary header: %w", err)
	}

	names := map[uint64]string{} // table id -> its own "schema/table" name
	sysTables := fakeSysIndexTable("SYS_TABLES", dictTablesID, sysTablesRoot, 1)
	if walkErr := walkOldIndexLeaves(sp, sysTables, sysTablesNFields, func(page []byte, ranges []fieldRange) bool {
		nr := ranges[sysTablesFldName]
		if nr.Null {
			return true
		}
		id := decodeUnsignedBE(fieldBytes(page, ranges[sysTablesFldID]))
		names[id] = string(page[nr.Start:nr.End])
		return true
	}); walkErr != nil {
		return nil, fmt.Errorf("reading SYS_TABLES: %w", walkErr)
	}

	locs := make(map[string]*TableLocation, len(names))
	sysIndexes := fakeSysIndexTable("SYS_INDEXES", dictIndexesID, sysIndexesRoot, 2)
	if walkErr := walkOldIndexLeaves(sp, sysIndexes, sysIndexesNFields, func(page []byte, ranges []fieldRange) bool {
		typ := decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldType]))
		if typ&dictIndexClustered == 0 {
			return true // one of some table's secondary indexes - not what we need
		}
		tableID := decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldTableID]))
		name, ok := names[tableID]
		if !ok {
			return true // an orphaned SYS_INDEXES row with no matching SYS_TABLES entry - ignore
		}
		locs[name] = &TableLocation{
			TableID:  tableID,
			SpaceID:  uint32(decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldSpace]))),
			IndexID:  decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldID])),
			RootPage: uint32(decodeUnsignedBE(fieldBytes(page, ranges[sysIndexesFldPageNo]))),
		}
		return true
	}); walkErr != nil {
		return nil, fmt.Errorf("reading SYS_INDEXES: %w", walkErr)
	}
	return locs, nil
}

// fieldBytes is page[fr.Start:fr.End], for a field already known not to be
// NULL (every field this file ever reads - table/index/type ids and the
// like - is NOT NULL by InnoDB's own hardcoded dictionary table schema).
func fieldBytes(page []byte, fr fieldRange) []byte {
	return page[fr.Start:fr.End]
}

// joinNamesForError renders a table-name listing for an error message,
// capped for readability (a real dictionary could hold thousands).
func joinNamesForError(names []string) string {
	const max = 40
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", ... and %d more", len(names)-max)
}
