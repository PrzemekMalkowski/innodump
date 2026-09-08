// format.go: the two output formats a decoded row (or a column's instant
// default) can be rendered as - see sqlout.go for the SQL INSERT dialect
// and tsvdump.go for the tab-separated one, and values.go/instant.go/
// btree.go for where the two actually diverge during decoding.
package main

// outputFormat selects how decodeField (values.go) and decodeOneRow
// (btree.go) render a column value.
type outputFormat int

const (
	// formatSQL renders each value as a standalone SQL literal, for
	// "INSERT INTO ... VALUES (...);" statements (sqlout.go).
	formatSQL outputFormat = iota
	// formatTSV renders each value the way MySQL Shell's dump utility
	// does in its default (un-enclosed, backslash-escaped) TSV dialect -
	// see tsvdump.go's package comment for the exact rules.
	formatTSV
)

// nullLiteral is how each format spells a NULL field: the bare SQL keyword
// for an INSERT statement, or "\N" - the same marker LOAD DATA INFILE (and
// MySQL Shell's dump utility) uses in its default dialect.
func nullLiteral(format outputFormat) string {
	if format == formatTSV {
		return `\N`
	}
	return "NULL"
}
