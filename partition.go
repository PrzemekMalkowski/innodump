// partition.go: partitioned InnoDB tables. Every partition (or, for a
// subpartitioned table, every subpartition) of a partitioned InnoDB table
// is its own file-per-table tablespace holding its own clustered-index
// B+tree - "<table>#p#<partition>.ibd", or
// "<table>#p#<partition>#sp#<subpartition>.ibd" - so extracting the table
// means walking each of those files in turn and writing all their rows out
// as one table, with one CREATE TABLE ... PARTITION BY ... for all of them.
//
// Where the table definition comes from depends on the server that wrote
// the files - the same split as for an unpartitioned table:
//
//   - MySQL 8.0+ (SDI): the table's SDI lives in only ONE of its partition
//     files - sdi_tablespace::store_tbl_sdi stores it in the first
//     tablespace fetch_first_tablespace_id finds, i.e. the first partition's
//     (sql/dd/impl/sdi_tablespace.cc) - and it lists every partition and
//     subpartition (Partition_impl) along with its own copy of each index
//     (Partition_index_impl), whose se_private_data gives that partition's
//     own root page, index id and space_id. The space_id is what pairs a
//     partition with its file here (every tablespace records its own id in
//     its header), not the file's name, so a partition renamed or
//     filename-encoded in some unexpected way still resolves. The PARTITION
//     BY clause itself is rendered back from the SDI the way SHOW CREATE
//     TABLE's generate_partition_syntax (sql/sql_partition.cc) does.
//
//   - MySQL 5.6/5.7 (.frm): a single "<table>.frm" describes the whole
//     table, and its "extra data segment" holds the server's own
//     partition clause text (frmPartitionClause), used verbatim. 5.6's
//     generic partitioning engine (ha_partition) also writes a
//     "<table>.par" file listing every partition's filename-encoded name in
//     order (parsePARFile); 5.7's native InnoDB partitioning doesn't, so
//     the partition order then comes from the clause itself. Each
//     partition file carries no dictionary of its own, and - like any
//     5.6/5.7 file-per-table tablespace - its clustered index is found at
//     the conventional root page 3 (frm.go's guessLegacyRootPage).
//
// Separators: 8.0+ writes "#p#"/"#sp#" with lower-cased partition names
// (dict_name::build_partition in storage/innobase/dict/dict0dd.cc); 5.7
// wrote "#P#"/"#SP#" with names as given, and some 5.6 builds lower-case
// the whole name - so every match here is case-insensitive.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// partitionFileRe matches a partition's tablespace file name, capturing the
// table's own base name and the partition suffix ("p0", "p0#sp#s0").
var partitionFileRe = regexp.MustCompile(`(?i)^(.+?)#p#(.+)\.ibd$`)

// partitionFileBase reports whether path is one partition of a partitioned
// table's tablespace files, and if so returns the table's own base path
// (directory plus table file name, no extension - e.g. ".../db/members")
// that every sibling partition file shares. ALTER TABLE's own temporary
// "#tmp" copies are never a live partition, and are left out.
func partitionFileBase(path string) (base string, ok bool) {
	m := partitionFileRe.FindStringSubmatch(filepath.Base(path))
	if m == nil || strings.Contains(strings.ToLower(m[2]), "#tmp") {
		return "", false
	}
	return filepath.Join(filepath.Dir(path), m[1]), true
}

// partitionSuffix returns the "p0" / "p0#sp#s0" part of a partition file's
// name, lower-cased and with 5.7's "#SP#" normalized to "#sp#".
func partitionSuffix(path string) string {
	m := partitionFileRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[2])
}

// findPartitionFiles lists every partition file sitting next to one
// partition file (or sharing the given base path), sorted by name.
func findPartitionFiles(base string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(base))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(filepath.Dir(base), e.Name())
		if b, ok := partitionFileBase(p); ok && b == base {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// partitionLeaf is one partition (or subpartition) that holds rows - one
// tablespace file's worth of the table.
type partitionLeaf struct {
	Name string // "p0", or "p0.s0" for a subpartition - for messages only
	Path string // its tablespace file; "" if it wasn't found
	// Raw is this leaf's own copy of the table definition (8.0+ only): the
	// table's SDI with the clustered index's se_private_data swapped for
	// this partition's own. nil for a .frm-described leaf, whose definition
	// is built from the .frm once its file is open (see openPartitionLeaf).
	Raw *ddTableJSON
}

// partitionedTable is everything resolvePartitionedTable learned about one
// partitioned table from its partition files.
type partitionedTable struct {
	Schema, Table  string
	Leaves         []partitionLeaf // in partition order
	Clause         string          // the DDL's own PARTITION BY clause, ready to append
	FRMPath        string          // the .frm (pre-8.0 only)
	MySQLVersionID uint32          // SDI's mysqld_version_id (0 for .frm)
	Extra          []string        // partition files no partition of the table's own definition claims
}

// resolvePartitionedTable works out the table that the partition files
// (all sharing one base path - see partitionFileBase) belong to.
func resolvePartitionedTable(base string, files []string) (*partitionedTable, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no partition files found for %s", filepath.Base(base))
	}
	// A pre-8.0 file carries no SDI at all; an 8.0+ one always has an SDI
	// index (empty in every partition but the first - see this file's
	// package comment).
	sp, err := OpenSpace(files[0])
	if err != nil {
		return nil, err
	}
	hasSDI := sp.Flags.sdi
	sp.Close()
	if hasSDI {
		return resolveSDIPartitionedTable(base, files)
	}
	return resolveFRMPartitionedTable(base, files)
}

// --- MySQL 8.0+ (SDI) ---

func resolveSDIPartitionedTable(base string, files []string) (*partitionedTable, error) {
	var raw *ddTableJSON
	var versionID uint32
	spaceFile := map[uint32]string{}
	for _, f := range files {
		sp, err := OpenSpace(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		spaceFile[sp.SpaceID] = f
		if raw == nil && sp.Flags.sdi {
			if tables, info, err := LoadSDITables(sp); err == nil {
				for _, js := range tables {
					var dt ddTableJSON
					if err := json.Unmarshal(js, &dt); err == nil && dt.PartitionType != 0 {
						raw, versionID = &dt, info.MySQLVersionID
						break
					}
				}
			}
		}
		sp.Close()
	}
	if raw == nil {
		return nil, fmt.Errorf("none of %s's %d partition file(s) holds the table's SDI - MySQL keeps it only in the first partition's "+
			"tablespace, so that file must be present to know the table's columns", filepath.Base(base), len(files))
	}
	clause, err := sdiPartitionClause(raw)
	if err != nil {
		return nil, fmt.Errorf("table %q: %w", raw.Name, err)
	}
	pt := &partitionedTable{Schema: raw.SchemaRef, Table: raw.Name, Clause: clause, MySQLVersionID: versionID}

	claimed := map[string]bool{}
	addLeaf := func(name string, p *ddPartitionJSON) {
		leaf := partitionLeaf{Name: name, Raw: sdiLeafTable(raw, p)}
		for _, pi := range p.Indexes {
			if pi.IndexOpx != 0 {
				continue
			}
			if v := sePropString(pi.SePrivateData)["space_id"]; v != "" {
				if id, err := strconv.ParseUint(v, 10, 32); err == nil {
					leaf.Path = spaceFile[uint32(id)]
				}
			}
		}
		if leaf.Path != "" {
			claimed[leaf.Path] = true
		}
		pt.Leaves = append(pt.Leaves, leaf)
	}
	for i := range raw.Partitions {
		p := &raw.Partitions[i]
		if len(p.Subpartitions) == 0 {
			addLeaf(p.Name, p)
			continue
		}
		for j := range p.Subpartitions {
			addLeaf(p.Name+"."+p.Subpartitions[j].Name, &p.Subpartitions[j])
		}
	}
	for _, f := range files {
		if !claimed[f] {
			pt.Extra = append(pt.Extra, f)
		}
	}
	return pt, nil
}

// sdiLeafTable returns a copy of raw describing just one partition p's
// own B+tree, as BuildTable expects an unpartitioned table's SDI: every
// index's se_private_data (root page, index id) replaced by p's own copy
// of it, and p's own se_private_data in place of the table's where it
// carries INSTANT ADD COLUMN state (8.0.12-8.0.28 keep "instant_col" per
// partition, not per table - see instant.go).
func sdiLeafTable(raw *ddTableJSON, p *ddPartitionJSON) *ddTableJSON {
	leaf := *raw
	leaf.PartitionType = 0
	leaf.Partitions = nil
	leaf.Indexes = append([]ddIndexJSON(nil), raw.Indexes...)
	for _, pi := range p.Indexes {
		if int(pi.IndexOpx) < len(leaf.Indexes) {
			leaf.Indexes[pi.IndexOpx].SePrivateData = pi.SePrivateData
		}
	}
	if sePropString(p.SePrivateData)["instant_col"] != "" {
		leaf.SePrivateData = p.SePrivateData
	}
	return &leaf
}

// dd::Table's enum_partition_type / enum_subpartition_type /
// enum_default_partitioning (sql/dd/types/table.h).
const (
	ptHash          = 1
	ptKey51         = 2
	ptKey55         = 3
	ptLinearHash    = 4
	ptLinearKey51   = 5
	ptLinearKey55   = 6
	ptRange         = 7
	ptList          = 8
	ptRangeColumns  = 9
	ptListColumns   = 10
	ptAuto          = 11
	ptAutoLinear    = 12
	stHash          = 1
	stKey51         = 2
	stKey55         = 3
	stLinearHash    = 4
	stLinearKey51   = 5
	stLinearKey55   = 6
	dpNo            = 1 // every partition listed explicitly
	dpYes           = 2 // PARTITION BY HASH (x) - one default partition, nothing listed
	dpNumber        = 3 // PARTITIONS N - default-named partitions, not listed
	keyAlgorithm51  = "/*!50611 ALGORITHM = 1 */ "
	partVersionBase = "50100"
	partVersionCols = "50500"
)

// sdiPartitionClause renders raw's PARTITION BY clause the way SHOW CREATE
// TABLE does - generate_partition_syntax in sql/sql_partition.cc, wrapped
// in partition_info::set_show_version_string's "/*!50100 ... */" (or
// "/*!50500" for COLUMNS partitioning) - so the generated DDL recreates
// the same partitions. The one deliberate difference: partition names and
// expression columns are always backquoted, which is valid everywhere.
func sdiPartitionClause(raw *ddTableJSON) (string, error) {
	var b strings.Builder
	version := partVersionBase
	algorithm51 := false // KEY ALGORITHM = 1 carries its own /*!50611 */, which can't nest in the wrapper
	b.WriteString("PARTITION BY ")
	switch raw.PartitionType {
	case ptHash:
		fmt.Fprintf(&b, "HASH (%s)", raw.PartitionExpressionUtf8)
	case ptLinearHash:
		fmt.Fprintf(&b, "LINEAR HASH (%s)", raw.PartitionExpressionUtf8)
	case ptKey55, ptAuto:
		fmt.Fprintf(&b, "KEY (%s)", raw.PartitionExpressionUtf8)
	case ptLinearKey55, ptAutoLinear:
		fmt.Fprintf(&b, "LINEAR KEY (%s)", raw.PartitionExpressionUtf8)
	case ptKey51:
		fmt.Fprintf(&b, "KEY %s(%s)", keyAlgorithm51, raw.PartitionExpressionUtf8)
		algorithm51 = true
	case ptLinearKey51:
		fmt.Fprintf(&b, "LINEAR KEY %s(%s)", keyAlgorithm51, raw.PartitionExpressionUtf8)
		algorithm51 = true
	case ptRange:
		fmt.Fprintf(&b, "RANGE (%s)", raw.PartitionExpressionUtf8)
	case ptList:
		fmt.Fprintf(&b, "LIST (%s)", raw.PartitionExpressionUtf8)
	case ptRangeColumns:
		fmt.Fprintf(&b, "RANGE COLUMNS(%s)", raw.PartitionExpressionUtf8)
		version = partVersionCols
	case ptListColumns:
		fmt.Fprintf(&b, "LIST COLUMNS(%s)", raw.PartitionExpressionUtf8)
		version = partVersionCols
	default:
		return "", fmt.Errorf("unrecognized partition_type %d", raw.PartitionType)
	}
	if raw.DefaultPartitioning == dpNumber {
		fmt.Fprintf(&b, "\nPARTITIONS %d", len(raw.Partitions))
	}
	subDefault := false
	if raw.SubpartitionType != 0 {
		b.WriteString("\nSUBPARTITION BY ")
		switch raw.SubpartitionType {
		case stHash:
			fmt.Fprintf(&b, "HASH (%s)", raw.SubpartitionExpressionUtf8)
		case stLinearHash:
			fmt.Fprintf(&b, "LINEAR HASH (%s)", raw.SubpartitionExpressionUtf8)
		case stKey55:
			fmt.Fprintf(&b, "KEY (%s)", raw.SubpartitionExpressionUtf8)
		case stLinearKey55:
			fmt.Fprintf(&b, "LINEAR KEY (%s)", raw.SubpartitionExpressionUtf8)
		case stKey51:
			fmt.Fprintf(&b, "KEY %s(%s)", keyAlgorithm51, raw.SubpartitionExpressionUtf8)
			algorithm51 = true
		case stLinearKey51:
			fmt.Fprintf(&b, "LINEAR KEY %s(%s)", keyAlgorithm51, raw.SubpartitionExpressionUtf8)
			algorithm51 = true
		default:
			return "", fmt.Errorf("unrecognized subpartition_type %d", raw.SubpartitionType)
		}
		subDefault = raw.DefaultSubpartitioning == dpNumber || raw.DefaultSubpartitioning == dpYes
		if raw.DefaultSubpartitioning == dpNumber && len(raw.Partitions) > 0 {
			fmt.Fprintf(&b, "\nSUBPARTITIONS %d", len(raw.Partitions[0].Subpartitions))
		}
	}
	if raw.DefaultPartitioning != dpNumber && raw.DefaultPartitioning != dpYes {
		b.WriteString("\n(")
		for i := range raw.Partitions {
			p := &raw.Partitions[i]
			if i > 0 {
				b.WriteString(",\n ")
			}
			b.WriteString("PARTITION " + backquote(p.Name) + sdiPartitionValues(raw.PartitionType, p))
			if raw.SubpartitionType == 0 || subDefault {
				b.WriteString(sdiPartitionOptions(p))
				continue
			}
			b.WriteString("\n (")
			for j := range p.Subpartitions {
				if j > 0 {
					b.WriteString(",\n  ")
				}
				b.WriteString("SUBPARTITION " + backquote(p.Subpartitions[j].Name) + sdiPartitionOptions(&p.Subpartitions[j]))
			}
			b.WriteString(")")
		}
		b.WriteString(")")
	}
	if algorithm51 {
		return "\n" + b.String(), nil
	}
	return "\n/*!" + version + " " + b.String() + " */", nil
}

// sdiPartitionValues renders one partition's " VALUES LESS THAN (...)" /
// " VALUES IN (...)" - add_partition_values in sql/sql_partition.cc - from
// its SDI values (nothing for HASH/KEY).
func sdiPartitionValues(ptype uint32, p *ddPartitionJSON) string {
	vals := append([]ddPartitionValueJSON(nil), p.Values...)
	sort.SliceStable(vals, func(i, j int) bool {
		if vals[i].ListNum != vals[j].ListNum {
			return vals[i].ListNum < vals[j].ListNum
		}
		return vals[i].ColumnNum < vals[j].ColumnNum
	})
	one := func(v ddPartitionValueJSON) string {
		switch {
		case v.MaxValue:
			return "MAXVALUE"
		case v.NullValue:
			return "NULL"
		}
		return v.ValueUtf8
	}
	switch ptype {
	case ptRange:
		if len(vals) == 0 || vals[0].MaxValue {
			return " VALUES LESS THAN MAXVALUE"
		}
		return " VALUES LESS THAN (" + vals[0].ValueUtf8 + ")"
	case ptRangeColumns:
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = one(v)
		}
		return " VALUES LESS THAN (" + strings.Join(parts, ",") + ")"
	case ptList, ptListColumns:
		// Group by list_num; a multi-column LIST COLUMNS item is itself
		// parenthesized, a single-column one isn't (add_column_list_values).
		var items []string
		multi := false
		for i := 0; i < len(vals); {
			j := i
			var cols []string
			for j < len(vals) && vals[j].ListNum == vals[i].ListNum {
				cols = append(cols, one(vals[j]))
				j++
			}
			if len(cols) > 1 {
				multi = true
			}
			items = append(items, strings.Join(cols, ","))
			i = j
		}
		if multi {
			for i := range items {
				items[i] = "(" + items[i] + ")"
			}
		}
		return " VALUES IN (" + strings.Join(items, ",") + ")"
	}
	return ""
}

// sdiPartitionOptions renders a partition's own trailing options -
// add_partition_options in sql/sql_partition.cc, limited to the ones that
// matter for recreating it elsewhere: COMMENT and ENGINE. (DATA DIRECTORY,
// TABLESPACE, MAX_ROWS/MIN_ROWS describe the old server's own layout, and
// are left out on purpose - same as the rest of the generated DDL's table
// options.)
func sdiPartitionOptions(p *ddPartitionJSON) string {
	s := ""
	if p.Comment != "" {
		s += " COMMENT = " + quoteSQLString(p.Comment)
	}
	engine := p.Engine
	if engine == "" {
		engine = "InnoDB"
	}
	return s + " ENGINE = " + engine
}

// --- MySQL 5.6/5.7 (.frm) ---

func resolveFRMPartitionedTable(base string, files []string) (*partitionedTable, error) {
	frmPath := base + ".frm"
	if _, err := os.Stat(frmPath); err != nil {
		return nil, fmt.Errorf("these partition files have no SDI (a pre-8.0 MySQL 5.6/5.7 table), and no %s was found next to them - "+
			"a partitioned table's one .frm file (named after the table itself, not any one partition) is needed to know its columns",
			filepath.Base(frmPath))
	}
	def, err := readFRMDefinition(frmPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(frmPath), err)
	}
	if def.partitionClause == "" {
		return nil, fmt.Errorf("%s doesn't describe a partitioned table, though partition files named after it exist", filepath.Base(frmPath))
	}
	if !def.engineIsInnoDB() {
		return nil, fmt.Errorf("%s describes a partitioned table whose partitions aren't InnoDB (%s) - only partitioned InnoDB tables are supported",
			filepath.Base(frmPath), legacyDBTypeName(def.partDBType))
	}
	pt := &partitionedTable{Schema: def.table.SchemaRef, Table: def.table.Name, FRMPath: frmPath,
		Clause: frmWrapPartitionClause(def.partitionClause)}

	// Partition order: the .par file's own list (5.6), else the clause's.
	var names []string
	if parNames, err := parsePARFile(base + ".par"); err == nil {
		names = parNames
	} else {
		names = frmClausePartitionNames(def.partitionClause)
	}
	bySuffix := map[string]string{}
	for _, f := range files {
		bySuffix[partitionSuffix(f)] = f
	}
	claimed := map[string]bool{}
	for _, n := range names {
		suffix := strings.ToLower(n)
		path := bySuffix[suffix]
		if path != "" {
			claimed[path] = true
		}
		pt.Leaves = append(pt.Leaves, partitionLeaf{Name: decodeMySQLFilename(strings.ReplaceAll(suffix, "#sp#", ".")), Path: path})
	}
	// Any partition file neither list names (a clause this tool couldn't
	// read the names out of, say) still belongs to this table: its rows are
	// included too, rather than silently dropped.
	for _, f := range files {
		if !claimed[f] {
			pt.Leaves = append(pt.Leaves, partitionLeaf{Name: decodeMySQLFilename(strings.ReplaceAll(partitionSuffix(f), "#sp#", ".")), Path: f})
		}
	}
	return pt, nil
}

// frmWrapPartitionClause wraps a .frm's own clause text the way SHOW
// CREATE TABLE does (see sdiPartitionClause) - unless it already holds a
// /*!...*/ comment of its own (a KEY partition's ALGORITHM), which can't
// nest inside another one, in which case it's used bare.
func frmWrapPartitionClause(clause string) string {
	clause = strings.TrimSpace(clause)
	if strings.Contains(clause, "/*") {
		return "\n" + clause
	}
	version := partVersionBase
	if regexp.MustCompile(`(?i)^PARTITION\s+BY\s+(RANGE|LIST)\s+COLUMNS`).MatchString(clause) {
		version = partVersionCols
	}
	return "\n/*!" + version + " " + clause + " */"
}

// parsePARFile reads a 5.6-style ha_partition ".par" file's partition list
// - ha_partition::create_handler_file in storage/partition/ha_partition.cc
// (5.7): little-endian 4-byte words - total length in words, checksum,
// partition count, then one engine byte per partition padded to a whole
// word, a name-area length in bytes, then the NUL-separated names in
// filename encoding ("p0", or "p0#SP#sp0" for a subpartition).
func parsePARFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("%s is too short", filepath.Base(path))
	}
	words := binary.LittleEndian.Uint32(data[0:4])
	if int(words)*4 > len(data) {
		return nil, fmt.Errorf("%s is truncated", filepath.Base(path))
	}
	nParts := int(binary.LittleEndian.Uint32(data[8:12]))
	nameLenOff := 12 + (nParts+3)/4*4
	if nameLenOff+4 > len(data) {
		return nil, fmt.Errorf("%s is truncated", filepath.Base(path))
	}
	nameLen := int(binary.LittleEndian.Uint32(data[nameLenOff : nameLenOff+4]))
	names := data[nameLenOff+4:]
	if nameLen > len(names) {
		return nil, fmt.Errorf("%s is truncated", filepath.Base(path))
	}
	var out []string
	for _, n := range strings.Split(string(names[:nameLen]), "\x00") {
		if n != "" {
			out = append(out, n)
		}
	}
	if len(out) != nParts {
		return nil, fmt.Errorf("%s lists %d partition name(s) for %d partition(s)", filepath.Base(path), len(out), nParts)
	}
	return out, nil
}

var (
	frmClauseNameRe  = regexp.MustCompile("(?i)\\b(SUB)?PARTITION\\s+(`(?:[^`]|``)+`|[A-Za-z0-9_$]+)")
	frmPartitionsRe  = regexp.MustCompile(`(?i)\bPARTITIONS\s+(\d+)`)
	frmSubpartsRe    = regexp.MustCompile(`(?i)\bSUBPARTITIONS\s+(\d+)`)
	frmSubpartByRe   = regexp.MustCompile(`(?i)\bSUBPARTITION\s+BY\b`)
	frmDefaultNumber = func(re *regexp.Regexp, clause string) int {
		if m := re.FindStringSubmatch(clause); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
		return 1
	}
)

// frmClausePartitionNames lists every leaf partition's file-name suffix
// ("p0", "p0#sp#s0") in order, from a .frm's own partition clause - for a
// 5.7 table, which has no .par file to list them. Partitions or
// subpartitions the clause doesn't name individually (PARTITIONS N /
// SUBPARTITIONS N) get the server's own default names: "p0", "p1", ...
// (partition_info::create_default_partition_names) and "<partition>sp0",
// "<partition>sp1", ... (create_default_subpartition_name).
func frmClausePartitionNames(clause string) []string {
	type part struct {
		name string
		subs []string
	}
	var parts []part
	for _, m := range frmClauseNameRe.FindAllStringSubmatch(clause, -1) {
		name := m[2]
		if strings.EqualFold(name, "BY") {
			continue
		}
		if strings.HasPrefix(name, "`") {
			name = strings.ReplaceAll(name[1:len(name)-1], "``", "`")
		}
		name = mysqlFilenameEncode(name)
		if m[1] == "" {
			parts = append(parts, part{name: name})
		} else if len(parts) > 0 {
			parts[len(parts)-1].subs = append(parts[len(parts)-1].subs, name)
		}
	}
	if len(parts) == 0 {
		for i := 0; i < frmDefaultNumber(frmPartitionsRe, clause); i++ {
			parts = append(parts, part{name: fmt.Sprintf("p%d", i)})
		}
	}
	subpartitioned := frmSubpartByRe.MatchString(clause)
	nSub := frmDefaultNumber(frmSubpartsRe, clause)
	var out []string
	for _, p := range parts {
		if !subpartitioned {
			out = append(out, p.name)
			continue
		}
		subs := p.subs
		if len(subs) == 0 {
			for i := 0; i < nSub; i++ {
				subs = append(subs, fmt.Sprintf("%ssp%d", p.name, i))
			}
		}
		for _, sub := range subs {
			out = append(out, p.name+"#sp#"+sub)
		}
	}
	return out
}

// mysqlFilenameEncode is decodeMySQLFilename's inverse, for the one form
// that covers ASCII: every byte outside [0-9A-Za-z_] becomes "@" plus its
// code point as four hex digits (my_charset_filename). Non-ASCII names are
// left as-is (see decodeMySQLFilename).
func mysqlFilenameEncode(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r > 0x7f:
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "@%04x", r)
		}
	}
	return b.String()
}

// --- building and walking ---

// openPartitions is a partitioned table ready to extract: one built Table
// per partition file found, sharing the same columns, plus the open
// tablespaces they read from (Close them when done).
type openPartitions struct {
	Main    *Table // the first partition's - used for DDL and output columns
	Leaves  []partitionLeaf
	Tables  []*Table // parallel to Spaces, one per present leaf
	Names   []string
	Paths   []string
	Spaces  []*Space
	Missing []string // leaves with no file
}

func (op *openPartitions) Close() {
	for _, sp := range op.Spaces {
		sp.Close()
	}
}

// openPartitionedTable opens every present partition of pt and builds its
// Table. A partition whose file is missing is reported in Missing (its
// rows can't be recovered from here) rather than failing the whole table.
func openPartitionedTable(pt *partitionedTable) (*openPartitions, error) {
	op := &openPartitions{Leaves: pt.Leaves}
	for _, leaf := range pt.Leaves {
		if leaf.Path == "" {
			op.Missing = append(op.Missing, leaf.Name)
			continue
		}
		sp, err := OpenSpace(leaf.Path)
		if err != nil {
			op.Close()
			return nil, fmt.Errorf("partition %s (%s): %w", leaf.Name, filepath.Base(leaf.Path), err)
		}
		raw := leaf.Raw
		if raw == nil {
			if raw, err = parseFRM(pt.FRMPath, sp, nil); err != nil {
				sp.Close()
				op.Close()
				return nil, fmt.Errorf("partition %s (%s): %w", leaf.Name, filepath.Base(leaf.Path), err)
			}
		}
		t, err := BuildTable(raw)
		if err != nil {
			sp.Close()
			op.Close()
			return nil, fmt.Errorf("partition %s (%s): %w", leaf.Name, filepath.Base(leaf.Path), err)
		}
		t.PartitionClause = pt.Clause
		op.Spaces = append(op.Spaces, sp)
		op.Tables = append(op.Tables, t)
		op.Names = append(op.Names, leaf.Name)
		op.Paths = append(op.Paths, leaf.Path)
	}
	if len(op.Tables) == 0 {
		return nil, fmt.Errorf("none of the table's %d partition file(s) were found", len(pt.Leaves))
	}
	op.Main = op.Tables[0]
	for i, t := range op.Tables[1:] {
		if len(OutputColumns(t)) != len(OutputColumns(op.Main)) {
			op.Close()
			return nil, fmt.Errorf("partition %s has a different column count than partition %s", op.Names[i+1], op.Names[0])
		}
	}
	return op, nil
}

// rowWalker walks every present partition in turn, as one RowWalker: each
// partition's rows are decoded with its own Table/outCols (same columns,
// in the same order, as every other partition's - only the B+tree
// differs), corruption reports and row errors are prefixed with the
// partition's name, and progress runs across all partitions' pages.
func (op *openPartitions) rowWalker(format outputFormat, skipCorrupted, deletedOnly bool) RowWalker {
	var total int64
	for _, sp := range op.Spaces {
		total += int64(sp.NumPages)
	}
	return RowWalker{
		ProgressTotal: total,
		Walk: func(onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
			var base int64
			for i, t := range op.Tables {
				name := op.Names[i]
				stopped := false
				err := WalkRows(op.Spaces[i], t, OutputColumns(t), format, skipCorrupted, deletedOnly,
					func(cp CorruptPage) {
						cp.Reason = "partition " + name + ": " + cp.Reason
						onCorrupt(cp)
					},
					func(roe RowOrError) bool {
						roe.ProgressBase = base
						if roe.Err != nil {
							roe.Err = fmt.Errorf("partition %s: %w", name, roe.Err)
						}
						if !fn(roe) {
							stopped = true
							return false
						}
						return true
					})
				if err != nil {
					return fmt.Errorf("partition %s: %w", name, err)
				}
				if stopped {
					return nil
				}
				base += int64(op.Spaces[i].NumPages)
			}
			return nil
		},
	}
}

// describe returns the one-line "N partition(s): p0, p1, ..." summary.
func (op *openPartitions) describe() string {
	s := fmt.Sprintf("%d partition(s): %s", len(op.Names), strings.Join(op.Names, ", "))
	if len(op.Missing) > 0 {
		s += fmt.Sprintf(" - %d missing: %s", len(op.Missing), strings.Join(op.Missing, ", "))
	}
	return s
}
