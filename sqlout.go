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
// exact), NULL-ability, comments, generated-column expressions, and the
// PRIMARY KEY - but deliberately not DEFAULT clauses, AUTO_INCREMENT
// counters, secondary indexes, or table options, which need more of the SDI
// than v1 parses. See the tool's README/--help for the full list.
func GenerateDDL(t *Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Reconstructed by ibd-extractor from the table's embedded SDI.\n")
	fmt.Fprintf(&b, "-- Best-effort DDL: column types, NULL-ability, comments, generated\n")
	fmt.Fprintf(&b, "-- columns and the PRIMARY KEY are reproduced; DEFAULT clauses,\n")
	fmt.Fprintf(&b, "-- AUTO_INCREMENT counters, secondary indexes and table options are not.\n")
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
		if c.Comment != "" {
			fmt.Fprintf(&l, " COMMENT %s", quoteSQLString(c.Comment))
		}
		lines = append(lines, l.String())
	}
	if t.HasExplicitPK {
		var parts []string
		for _, f := range t.PKFields {
			p := backquote(f.Col.Name)
			if f.PrefixLen > 0 {
				p += fmt.Sprintf("(%d)", f.PrefixLen)
			}
			parts = append(parts, p)
		}
		lines = append(lines, "  PRIMARY KEY ("+strings.Join(parts, ",")+")")
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n)")
	fmt.Fprintf(&b, " ENGINE=InnoDB ROW_FORMAT=%s;\n", rowFormatName(t.RowFormat))
	return b.String()
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
