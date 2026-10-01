// sqlout.go: renders the resolved Table model as a best-effort CREATE
// TABLE statement, and picks the set of columns that actually appear in
// INSERT statements (everything except InnoDB's own system columns, SE
// -hidden housekeeping columns, and virtual generated columns, none of
// which a user ever supplies a value for).
package main

import (
	"fmt"
	"strings"
)

// OutputColumns returns t's columns in CREATE TABLE order, restricted to
// the ones an INSERT statement actually needs a value for.
func OutputColumns(t *Table) []*Column {
	var out []*Column
	for _, c := range t.Columns {
		if c.IsSystem || c.Hidden == hiddenSE || c.IsVirtual {
			continue
		}
		out = append(out, c)
	}
	return out
}

// convertMyISAMToInnoDB is --myisam-to-innodb (main.go): when set,
// GenerateDDL renders every MyISAM table as ENGINE=InnoDB instead - see
// GenerateDDL and myisamToInnoDBAutoIncKey for what else that changes.
// Only the generated DDL is affected; the rows themselves are decoded and
// written exactly the same either way, and load into the converted table
// unchanged.
var convertMyISAMToInnoDB bool

func backquote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// resolveCollation returns id's charset name and, where this tool can
// resolve it exactly, its full collation name too (collationKnown false
// for one of MariaDB's newer UCA-1400 collations - see uca1400Charset -
// where only the charset can be recovered). ok is false only if id isn't
// recognized at all.
func resolveCollation(id uint64) (charset, collation string, collationKnown, ok bool) {
	if cl, found := collationTable[id]; found {
		return charsetOf(cl.name), cl.name, true, true
	}
	if c, found := uca1400Charset(id); found {
		return c.name, "", false, true
	}
	return "", "", false, false
}

// columnHasCharset reports whether c's type carries a character set/
// collation at all (CHAR/VARCHAR/TEXT-family/ENUM/SET) - every other
// column type still has a "collation_id" in the SDI (every column gets
// one, string or not), but it's a meaningless leftover that must never be
// rendered as CHARACTER SET/COLLATE.
func columnHasCharset(ddType uint32) bool {
	switch ddType {
	case ddVarchar, ddVarString, ddString, ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob, ddEnum, ddSet:
		return true
	default:
		return false
	}
}

// GenerateDDL renders a best-effort CREATE TABLE statement. It reproduces
// column types (straight from the SDI's own column_type_utf8, so these are
// exact) plus NULL-ability, a column's own CHARACTER SET/COLLATE where SHOW
// CREATE TABLE itself would show one (see resolveCollation/
// columnHasCharset), AUTO_INCREMENT, comments, generated-column
// expressions, the PRIMARY KEY, secondary indexes, FOREIGN KEY constraints,
// and the table's default charset/collation and AUTO_INCREMENT counter -
// but deliberately not DEFAULT clauses or other table options, which need
// more of the SDI than v1 parses (or, for AUTO_INCREMENT, aren't in the SDI
// at all - see Table.AutoIncrementNext's comment in schema.go). See the
// tool's README/--help for the full list.
func GenerateDDL(t *Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Reconstructed by %s from the table's own dictionary information (SDI/.frm).\n", appName)
	fmt.Fprintf(&b, "-- Best-effort DDL: column types, NULL-ability, AUTO_INCREMENT, comments,\n")
	fmt.Fprintf(&b, "-- generated columns, the PRIMARY KEY, secondary indexes, FOREIGN KEY\n")
	fmt.Fprintf(&b, "-- constraints, and the default charset/collation are reproduced; DEFAULT\n")
	fmt.Fprintf(&b, "-- clauses and other table options are not. AUTO_INCREMENT (if any) is\n")
	fmt.Fprintf(&b, "-- the highest value seen in the data plus one, not the server's persisted\n")
	fmt.Fprintf(&b, "-- counter (the SDI doesn't carry that) - it may be behind if rows were\n")
	fmt.Fprintf(&b, "-- deleted from the high end.\n")
	engine := t.Engine
	var autoIncKey string
	if t.Engine == "MyISAM" && convertMyISAMToInnoDB {
		engine = "InnoDB"
		autoIncKey = myisamToInnoDBAutoIncKey(t)
		fmt.Fprintf(&b, "-- Converted from ENGINE=MyISAM to ENGINE=InnoDB (--myisam-to-innodb); MyISAM's own\n")
		fmt.Fprintf(&b, "-- ROW_FORMAT (FIXED/DYNAMIC) is dropped, leaving the server's InnoDB default.\n")
		if autoIncKey != "" {
			fmt.Fprintf(&b, "-- KEY %s was added: InnoDB requires an AUTO_INCREMENT column to lead some\n", backquote(autoIncKey))
			fmt.Fprintf(&b, "-- index, which MyISAM doesn't. Note that InnoDB's counter is table-wide, not\n")
			fmt.Fprintf(&b, "-- per-group like MyISAM's for an AUTO_INCREMENT that isn't a key's first column.\n")
		}
	}
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", backquote(t.SchemaRef), backquote(t.Name))

	var lines []string
	for _, c := range t.Columns {
		if c.IsSystem || c.Hidden == hiddenSE {
			continue
		}
		var l strings.Builder
		fmt.Fprintf(&l, "  %s %s", backquote(c.Name), c.TypeText)
		// Matches SHOW CREATE TABLE's own rule (print_create_fields_stmt in
		// sql_show.cc): CHARACTER SET shows whenever the column's charset
		// differs from the table's own default, or the column's collation
		// was named explicitly at CREATE/ALTER TABLE time (SDI's
		// is_explicit_collation) - even if it happens to equal the table's
		// own default. COLLATE shows whenever that collation isn't its
		// charset's own "primary" one (primaryCollationIDs, collations.go),
		// it was named explicitly, or it's the utf8mb4_0900_ai_ci special
		// case (collationUtf8mb40900AiCi's own comment) - but only when the
		// table's own default isn't ALSO utf8mb4_0900_ai_ci (an ordinary
		// column in an ordinary modern utf8mb4 table, the common case,
		// stays unannotated). A binary-charset column (BLOB/BINARY/
		// VARBINARY) never gets either: its type keyword alone already
		// implies that unambiguously, exactly as real DDL does.
		if columnHasCharset(c.DDType) && !c.IsBinary() {
			if charset, collation, collationKnown, ok := resolveCollation(c.CollationID); ok {
				tblCharset, _, _, tblOK := resolveCollation(t.CollationID)
				explicit := c.IsExplicitCollation
				if explicit || !tblOK || charset != tblCharset {
					fmt.Fprintf(&l, " CHARACTER SET %s", charset)
				}
				showCollate := explicit || !primaryCollationIDs[c.CollationID] ||
					(c.CollationID == collationUtf8mb40900AiCi && t.CollationID != collationUtf8mb40900AiCi)
				if collationKnown && showCollate {
					fmt.Fprintf(&l, " COLLATE %s", collation)
				}
			}
		}
		if c.GenExpr != "" {
			kind := "VIRTUAL"
			if !c.IsVirtual {
				kind = "STORED"
			}
			fmt.Fprintf(&l, " GENERATED ALWAYS AS (%s) %s", c.GenExpr, kind)
		}
		if !c.IsNullable {
			l.WriteString(" NOT NULL")
		} else if c.DDType == ddTimestamp || c.DDType == ddTimestamp2 {
			// SHOW CREATE TABLE's own rule: a nullable TIMESTAMP always says
			// so - without it, a server running with
			// explicit_defaults_for_timestamp=OFF (every 5.x default)
			// silently makes the column NOT NULL DEFAULT CURRENT_TIMESTAMP,
			// and every NULL loaded into it turns into the load time.
			l.WriteString(" NULL")
		}
		if c.IsAutoIncrement {
			l.WriteString(" AUTO_INCREMENT")
		}
		if c.Comment != "" {
			fmt.Fprintf(&l, " COMMENT %s", quoteSQLString(c.Comment))
		}
		lines = append(lines, l.String())
	}
	if t.HasExplicitPK {
		if t.PKIsUniqueKey {
			lines = append(lines, "  UNIQUE KEY "+backquote(t.IndexName)+" ("+secondaryIndexColumnList(t.PKFieldsAsColumns())+")")
		} else {
			lines = append(lines, "  PRIMARY KEY ("+secondaryIndexColumnList(t.PKFieldsAsColumns())+")")
		}
	}
	for _, si := range t.SecondaryIndexes {
		kind := "KEY"
		switch {
		case si.Unique:
			kind = "UNIQUE KEY"
		case si.Fulltext:
			kind = "FULLTEXT KEY"
		case si.Spatial:
			kind = "SPATIAL KEY"
		}
		cols := si.Columns
		if si.Fulltext || si.Spatial {
			// Neither takes a prefix length (MySQL rejects one); whatever
			// length the dictionary recorded for their key parts is internal.
			cols = make([]SecondaryIndexColumn, len(si.Columns))
			for i, c := range si.Columns {
				cols[i] = SecondaryIndexColumn{Col: c.Col}
			}
		}
		line := fmt.Sprintf("  %s %s (%s)", kind, backquote(si.Name), secondaryIndexColumnList(cols))
		if !si.Visible {
			line += " /*!80000 INVISIBLE */"
		}
		lines = append(lines, line)
	}
	if autoIncKey != "" {
		lines = append(lines, fmt.Sprintf("  KEY %s (%s)", backquote(autoIncKey), backquote(t.AutoIncrementCol.Name)))
	}
	for _, fk := range t.ForeignKeys {
		refTable := backquote(fk.ReferencedTable)
		if !strings.EqualFold(fk.ReferencedSchema, t.SchemaRef) {
			refTable = backquote(fk.ReferencedSchema) + "." + refTable
		}
		fkCols := make([]string, len(fk.Columns))
		for i, c := range fk.Columns {
			fkCols[i] = backquote(c.Name)
		}
		refCols := make([]string, len(fk.ReferencedColumns))
		for i, name := range fk.ReferencedColumns {
			refCols[i] = backquote(name)
		}
		line := fmt.Sprintf("  CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
			backquote(fk.Name), strings.Join(fkCols, ","), refTable, strings.Join(refCols, ","))
		// SHOW CREATE TABLE only ever prints these when they're not the
		// implicit NO ACTION default (print_foreign_key_info in
		// sql_show.cc) - OnDelete/OnUpdate are already "" in that case.
		if fk.OnDelete != "" {
			line += " ON DELETE " + fk.OnDelete
		}
		if fk.OnUpdate != "" {
			line += " ON UPDATE " + fk.OnUpdate
		}
		lines = append(lines, line)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	fmt.Fprintf(&b, "\n) ENGINE=%s", engine)
	switch engine {
	case "MyISAM":
		// SHOW CREATE TABLE only states ROW_FORMAT= when it differs from
		// what MyISAM would pick on its own: FIXED if every column is
		// fixed-width (no VARCHAR/BLOB/TEXT), DYNAMIC otherwise - see
		// myisamDefaultRowFormat.
		if t.RowFormat != myisamDefaultRowFormat(t) {
			b.WriteString(" ROW_FORMAT=" + rowFormatName(t.RowFormat))
		}
	default: // InnoDB
		if t.RowFormat == rowFormatCompact { // DYNAMIC is the server default; omit it
			b.WriteString(" ROW_FORMAT=COMPACT")
		}
	}
	if t.AutoIncrementNext != nil {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", *t.AutoIncrementNext)
	}
	if charset, collation, collationKnown, ok := resolveCollation(t.CollationID); ok {
		fmt.Fprintf(&b, " DEFAULT CHARSET=%s", charset)
		// Matches SHOW CREATE TABLE's own rule (show_create_table in
		// sql_show.cc): COLLATE shows when the table's collation isn't its
		// charset's own "primary" one (primaryCollationIDs, collations.go),
		// or - unconditionally, even though it IS primary - it's the
		// utf8mb4_0900_ai_ci special case (collationUtf8mb40900AiCi's own
		// comment; unlike the column-level rule, the table-level one always
		// applies, with no "unless the table already defaults to it" carve-out).
		if collationKnown && (!primaryCollationIDs[t.CollationID] || t.CollationID == collationUtf8mb40900AiCi) {
			fmt.Fprintf(&b, " COLLATE=%s", collation)
		}
		// Else (collationKnown false) one of MariaDB's newer UCA-1400
		// collations (id 2048+) - there's no way to recover which of its
		// many language-variant/pad-mode names this specific id is (see
		// uca1400Charset), so this
		// can only state the charset, not COLLATE=<the real collation>.
	}
	// A partitioned table's PARTITION BY clause comes last, after every
	// table option, exactly where SHOW CREATE TABLE puts it (partition.go).
	b.WriteString(t.PartitionClause)
	b.WriteString(";\n")
	return b.String()
}

// myisamToInnoDBAutoIncKey returns the name of the extra KEY a MyISAM
// table converted to InnoDB (--myisam-to-innodb) needs for its
// AUTO_INCREMENT column, or "" if it needs none. MyISAM accepts an
// AUTO_INCREMENT column anywhere in a multi-column key - PRIMARY KEY
// (grp, id) with id AUTO_INCREMENT gives each grp its own sequence - but
// InnoDB refuses the whole CREATE TABLE ("there can be only one auto
// column and it must be defined as a key") unless that column is the
// first part of at least one index, so this adds a plain KEY on it alone.
// The name is the column's own, as MySQL itself would pick for an unnamed
// KEY (col), unless an index already uses it.
func myisamToInnoDBAutoIncKey(t *Table) string {
	col := t.AutoIncrementCol
	if col == nil {
		return ""
	}
	if t.HasExplicitPK && len(t.PKFields) > 0 && t.PKFields[0].Col == col {
		return ""
	}
	taken := map[string]bool{"primary": true}
	for _, si := range t.SecondaryIndexes {
		taken[strings.ToLower(si.Name)] = true
		if !si.Fulltext && !si.Spatial && len(si.Columns) > 0 && si.Columns[0].Col == col {
			return ""
		}
	}
	name := col.Name
	for i := 2; taken[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s_%d", col.Name, i)
	}
	return name
}

// PKFieldsAsColumns adapts t.PKFields to the shape indexColumnList expects.
func (t *Table) PKFieldsAsColumns() []SecondaryIndexColumn {
	out := make([]SecondaryIndexColumn, len(t.PKFields))
	for i, f := range t.PKFields {
		out[i] = SecondaryIndexColumn{Col: f.Col, PrefixLen: f.PrefixLen}
	}
	return out
}

func secondaryIndexColumnList(cols []SecondaryIndexColumn) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		p := backquote(c.Col.Name)
		if n := ddlPrefixLen(c.Col, c.PrefixLen); n > 0 {
			p += fmt.Sprintf("(%d)", n)
		}
		if c.Desc {
			p += " DESC"
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}

// isBlobFamily reports whether ddType is a BLOB/TEXT-family column type -
// one that can only ever be indexed through a prefix.
func isBlobFamily(ddType uint32) bool {
	switch ddType {
	case ddTinyBlob, ddBlob, ddMediumBlob, ddLongBlob:
		return true
	}
	return false
}

// ddlPrefixLen turns a key part's stored prefix length (in bytes, as both
// the SDI and a .frm record it, and as InnoDB's own record decoding needs
// it) into what a CREATE TABLE key definition takes - 0 for "no prefix",
// else a length in characters: a prefix as long as the whole column
// (InnoDB's own PK decoding keeps one for every long column - see
// BuildTable) is no prefix at all, and a multi-byte charset's byte count
// is divided back down by its bytes-per-character, exactly as SHOW CREATE
// TABLE does (key_part->length / charset->mbmaxlen). Printing the raw
// byte count instead would make a utf8mb3 VARCHAR(255) key's full-column
// "prefix" read 765, which MySQL rejects outright ("Incorrect prefix key").
func ddlPrefixLen(col *Column, prefixLen uint32) uint32 {
	if prefixLen == 0 {
		return 0
	}
	if !isBlobFamily(col.DDType) && prefixLen >= col.ColLen {
		return 0
	}
	if columnHasCharset(col.DDType) && !col.IsBinary() {
		if info, ok := collationInfoFor(col.CollationID); ok && info.max > 1 {
			prefixLen /= uint32(info.max)
		}
	}
	return prefixLen
}

// InsertPrefix returns "INSERT INTO `schema`.`table` (`c1`,`c2`,...) VALUES ".
func InsertPrefix(t *Table, outCols []*Column) string {
	names := make([]string, len(outCols))
	for i, c := range outCols {
		names[i] = backquote(c.Name)
	}
	return fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES ", backquote(t.SchemaRef), backquote(t.Name), strings.Join(names, ","))
}

// FormatRow renders one decoded row as "(v1,v2,...)".
func FormatRow(row *Row) string {
	return "(" + strings.Join(row.Values, ",") + ")"
}
