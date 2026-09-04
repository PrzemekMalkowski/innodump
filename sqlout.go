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

func backquote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// GenerateDDL renders a best-effort CREATE TABLE statement. It reproduces
// column types (straight from the SDI's own column_type_utf8, so these are
// exact) plus NULL-ability, AUTO_INCREMENT, comments, generated-column
// expressions, the PRIMARY KEY, secondary indexes, and the table's default
// charset/collation and AUTO_INCREMENT counter - but deliberately not
// DEFAULT clauses or other table options, which need more of the SDI than
// v1 parses (or, for AUTO_INCREMENT, aren't in the SDI at all - see
// Table.AutoIncrementNext's comment in schema.go). See the tool's
// README/--help for the full list.
func GenerateDDL(t *Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Reconstructed by ibd-extractor from the table's embedded SDI.\n")
	fmt.Fprintf(&b, "-- Best-effort DDL: column types, NULL-ability, AUTO_INCREMENT, comments,\n")
	fmt.Fprintf(&b, "-- generated columns, the PRIMARY KEY, secondary indexes, and the default\n")
	fmt.Fprintf(&b, "-- charset/collation are reproduced; DEFAULT clauses and other table\n")
	fmt.Fprintf(&b, "-- options are not. AUTO_INCREMENT (if any) is the highest value seen in\n")
	fmt.Fprintf(&b, "-- the data plus one, not the server's persisted counter (the SDI doesn't\n")
	fmt.Fprintf(&b, "-- carry that) - it may be behind if rows were deleted from the high end.\n")
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", backquote(t.SchemaRef), backquote(t.Name))

	var lines []string
	for _, c := range t.Columns {
		if c.IsSystem || c.Hidden == hiddenSE {
			continue
		}
		var l strings.Builder
		fmt.Fprintf(&l, "  %s %s", backquote(c.Name), c.TypeText)
		if c.GenExpr != "" {
			kind := "VIRTUAL"
			if !c.IsVirtual {
				kind = "STORED"
			}
			fmt.Fprintf(&l, " GENERATED ALWAYS AS (%s) %s", c.GenExpr, kind)
		}
		if !c.IsNullable {
			l.WriteString(" NOT NULL")
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
		lines = append(lines, "  PRIMARY KEY ("+secondaryIndexColumnList(t.PKFieldsAsColumns())+")")
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
		line := fmt.Sprintf("  %s %s (%s)", kind, backquote(si.Name), secondaryIndexColumnList(si.Columns))
		if !si.Visible {
			line += " /*!80000 INVISIBLE */"
		}
		lines = append(lines, line)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n) ENGINE=InnoDB")
	if t.RowFormat == rowFormatCompact { // DYNAMIC is the server default; omit it
		b.WriteString(" ROW_FORMAT=COMPACT")
	}
	if t.AutoIncrementNext != nil {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", *t.AutoIncrementNext)
	}
	if cl, ok := collationTable[t.CollationID]; ok {
		fmt.Fprintf(&b, " DEFAULT CHARSET=%s COLLATE=%s", charsetOf(cl.name), cl.name)
	}
	b.WriteString(";\n")
	return b.String()
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
		if c.PrefixLen > 0 {
			p += fmt.Sprintf("(%d)", c.PrefixLen)
		}
		if c.Desc {
			p += " DESC"
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
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
