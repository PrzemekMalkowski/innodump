package main

import (
	"os"
	"strings"
	"testing"
)

// TestColumnDefaultClause checks columnDefaultClause against SDI column
// fragments captured from a real MySQL 8.0.40 server, with the clause its
// SHOW CREATE TABLE printed for each (modulo quoting style: quoteSQLString
// writes \' where the server writes ”).
func TestColumnDefaultClause(t *testing.T) {
	cases := []struct {
		name string
		raw  ddColumnJSON
		want string
	}{
		{"nullable, no default", ddColumnJSON{Type: ddLong, IsNullable: true, DefaultValueUtf8Null: true}, " DEFAULT NULL"},
		{"NOT NULL, no default", ddColumnJSON{Type: ddLong, HasNoDefault: true, DefaultValueUtf8Null: true}, ""},
		{"auto_increment", ddColumnJSON{Type: ddLong, IsAutoIncrement: true, DefaultValueUtf8Null: true}, ""},
		{"int literal", ddColumnJSON{Type: ddLong, DefaultValueUtf8: "-3"}, " DEFAULT '-3'"},
		{"string with quote", ddColumnJSON{Type: ddVarchar, CharLength: 80, CollationID: 255, IsNullable: true, DefaultValueUtf8: "it's"}, ` DEFAULT 'it\'s'`},
		{"empty string", ddColumnJSON{Type: ddVarchar, CharLength: 40, CollationID: 255, DefaultValueUtf8: ""}, " DEFAULT ''"},
		{"bit", ddColumnJSON{Type: ddBit, CharLength: 5, IsNullable: true, DefaultValueUtf8: "b'101'"}, " DEFAULT b'101'"},
		{"text, nullable", ddColumnJSON{Type: ddBlob, CollationID: 255, IsNullable: true, DefaultValueUtf8Null: true}, ""},
		{"json, nullable", ddColumnJSON{Type: ddJSON, CollationID: 63, IsNullable: true, DefaultValueUtf8Null: true}, " DEFAULT NULL"},
		{"geometry, nullable", ddColumnJSON{Type: ddGeometry, CollationID: 63, IsNullable: true, DefaultValueUtf8Null: true}, " DEFAULT NULL"},
		{"binary(4) 'ab' keeps its padding", ddColumnJSON{Type: ddString, CharLength: 4, CollationID: 63, IsNullable: true,
			DefaultValueUtf8: "0x6162", DefaultValue: "YWIAAA=="}, ` DEFAULT 'ab\0\0'`},
		{"varbinary non-UTF-8 as hex", ddColumnJSON{Type: ddVarchar, CharLength: 10, CollationID: 63, IsNullable: true,
			DefaultValueUtf8: "0x", DefaultValue: "AwD/QQAAAAAAAAA="}, " DEFAULT 0x00FF41"},
		{"varbinary(300), 2-byte length", ddColumnJSON{Type: ddVarchar, CharLength: 300, CollationID: 63, IsNullable: true,
			DefaultValueUtf8: "0x6869", DefaultValue: "AgBoaQAAAA=="}, " DEFAULT 'hi'"},
		{"varbinary, no raw image", ddColumnJSON{Type: ddVarchar, CharLength: 10, CollationID: 63, IsNullable: true,
			DefaultValueUtf8: "0x"}, ""},
		{"CURRENT_TIMESTAMP", ddColumnJSON{Type: ddTimestamp2, DefaultValueUtf8: "CURRENT_TIMESTAMP", DefaultOption: "CURRENT_TIMESTAMP"},
			" DEFAULT CURRENT_TIMESTAMP"},
		{"CURRENT_TIMESTAMP(6) ON UPDATE", ddColumnJSON{Type: ddDatetime2, DatetimePrecision: 6, IsNullable: true,
			DefaultValueUtf8: "CURRENT_TIMESTAMP(6)", DefaultOption: "CURRENT_TIMESTAMP(6)", UpdateOption: "CURRENT_TIMESTAMP(6)"},
			" DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)"},
		{"ON UPDATE only", ddColumnJSON{Type: ddDatetime2, IsNullable: true, DefaultValueUtf8Null: true, UpdateOption: "CURRENT_TIMESTAMP"},
			" DEFAULT NULL ON UPDATE CURRENT_TIMESTAMP"},
		{"literal ON UPDATE", ddColumnJSON{Type: ddTimestamp2, DefaultValueUtf8: "2001-01-01 00:00:00", UpdateOption: "CURRENT_TIMESTAMP"},
			" DEFAULT '2001-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP"},
		{"expression", ddColumnJSON{Type: ddLong, IsNullable: true, DefaultValueUtf8: "(`i3` + 1)", DefaultOption: "(`i3` + 1)"},
			" DEFAULT ((`i3` + 1))"},
		{"expression on TEXT", ddColumnJSON{Type: ddBlob, CollationID: 255, IsNullable: true,
			DefaultValueUtf8: `_utf8mb4\'hello\'`, DefaultOption: "_utf8mb4'hello'"}, " DEFAULT (_utf8mb4'hello')"},
		{"now() is an expression", ddColumnJSON{Type: ddDatetime2, IsNullable: true, DefaultValueUtf8: "now()", DefaultOption: "now()"},
			" DEFAULT (now())"},
		{"generated column", ddColumnJSON{Type: ddLong, IsNullable: true, IsVirtual: true, DefaultValueUtf8Null: true,
			GenerationExpressionUtf8: "(`i3` * 2)"}, ""},
	}
	for _, tc := range cases {
		tc.raw.Name = "c"
		c, err := buildColumn(tc.raw)
		if err != nil {
			t.Fatalf("%s: buildColumn: %v", tc.name, err)
		}
		if got := columnDefaultClause(c); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestGenerateDDLFromTestFiles checks GenerateDDL's column and key lines
// against SHOW CREATE TABLE for the tables in test/: two MySQL 8.0 tables
// with a generated invisible primary key (sql_generate_invisible_primary_key),
// the second also with an INVISIBLE ordinary column, and a MySQL 8.4.10
// table with an SRID-restricted spatial column and a SPATIAL index.
func TestGenerateDDLFromTestFiles(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"test/ttest1.ibd", []string{
			"  `my_row_id` bigint unsigned NOT NULL AUTO_INCREMENT /*!80023 INVISIBLE */,",
			"  `id` int DEFAULT NULL,",
			"  `a` int DEFAULT NULL,",
			"  `b` datetime DEFAULT NULL,",
			"  PRIMARY KEY (`my_row_id`)",
		}},
		{"test/ttest2.ibd", []string{
			"  `my_row_id` bigint unsigned NOT NULL AUTO_INCREMENT /*!80023 INVISIBLE */,",
			"  `id` int DEFAULT NULL,",
			"  `a` varchar(1000) DEFAULT NULL,",
			"  `b` datetime DEFAULT NULL /*!80023 INVISIBLE */,",
			"  PRIMARY KEY (`my_row_id`)",
		}},
		{"test/geom1.ibd", []string{
			"  `a` int DEFAULT NULL,",
			"  `b` datetime DEFAULT NULL,",
			"  `g` geometry NOT NULL /*!80003 SRID 4326 */,",
			"  `id` int NOT NULL AUTO_INCREMENT,",
			"  PRIMARY KEY (`id`),",
			"  SPATIAL KEY `g` (`g`)",
		}},
	}
	for _, tc := range cases {
		if _, err := os.Stat(tc.file); err != nil {
			t.Skipf("%s not present", tc.file)
		}
		sp, err := OpenSpace(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		targets, _, err := loadTargets(sp, tc.file)
		if err != nil || len(targets) != 1 {
			t.Fatalf("%s: loadTargets: %d tables, %v", tc.file, len(targets), err)
		}
		tbl, err := BuildTable(targets[0])
		if err != nil {
			t.Fatal(err)
		}
		ddl := GenerateDDL(tbl)
		start := strings.Index(ddl, "(\n")
		end := strings.LastIndex(ddl, "\n)")
		if start < 0 || end < start {
			t.Fatalf("%s: unexpected DDL shape:\n%s", tc.file, ddl)
		}
		if got := strings.Split(ddl[start+2:end], "\n"); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: got\n%s\nwant\n%s", tc.file, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
}

// TestFunctionalKeyPart checks that a functional index's hidden generated
// column is written as its expression, as SHOW CREATE TABLE does
// (KEY `kfunc` (((`c` + `a`))) for KEY kfunc ((c+a))).
func TestFunctionalKeyPart(t *testing.T) {
	hidden := &Column{Name: "!hidden!kfunc!0!0", Hidden: hiddenSQL, GenExpr: "(`c` + `a`)"}
	plain := &Column{Name: "b"}
	got := secondaryIndexColumnList([]SecondaryIndexColumn{{Col: hidden, Desc: true}, {Col: plain}})
	if want := "((`c` + `a`)) DESC,`b`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCheckConstraints checks buildCheckConstraints and their DDL lines
// against a real MySQL 8.0.40 table: SDI-escaped clause text, the
// case-insensitive name order SHOW CREATE TABLE lists them in, and the
// versioned NOT ENFORCED.
func TestCheckConstraints(t *testing.T) {
	raw := &ddTableJSON{CheckConstraints: []ddCheckJSON{
		{Name: "Zed", State: 1, CheckClauseUtf8: "(`v` <> 7)"},
		{Name: "alpha", State: 2, CheckClauseUtf8: "(`c` like _latin1\\'x\\\\\\'%\\')"},
		{Name: "Bee", State: 2, CheckClauseUtf8: "(`v` > 1)"},
		{Name: "_u", State: 2, CheckClauseUtf8: "(`v` <> 50)"},
	}}
	tbl := &Table{Name: "t", SchemaRef: "s", Engine: "InnoDB", CheckConstraints: buildCheckConstraints(raw)}
	ddl := GenerateDDL(tbl)
	want := strings.Join([]string{
		"  CONSTRAINT `_u` CHECK ((`v` <> 50)),",
		"  CONSTRAINT `alpha` CHECK ((`c` like _latin1'x\\'%')),",
		"  CONSTRAINT `Bee` CHECK ((`v` > 1)),",
		"  CONSTRAINT `Zed` CHECK ((`v` <> 7)) /*!80016 NOT ENFORCED */",
	}, "\n")
	if !strings.Contains(ddl, want) {
		t.Errorf("got:\n%s\nwant it to contain:\n%s", ddl, want)
	}
}

// TestFRMFloatTypeText checks FLOAT/DOUBLE type text from a .frm against
// MySQL 5.7.44's SHOW CREATE TABLE: (M,D) is kept, even (M,0), and only a
// column without a fixed scale (decimals = NOT_FIXED_DEC) is bare.
func TestFRMFloatTypeText(t *testing.T) {
	cases := []struct {
		typ            uint32
		length, scale  uint32
		unsigned, zero bool
		want           string
	}{
		{ddFloat, 12, frmNotFixedDec, false, false, "float"},
		{ddFloat, 7, 3, false, false, "float(7,3)"},
		{ddFloat, 8, 4, true, true, "float(8,4) unsigned zerofill"},
		{ddDouble, 22, frmNotFixedDec, false, false, "double"},
		{ddDouble, 10, 4, false, false, "double(10,4)"},
		{ddDouble, 20, 0, false, false, "double(20,0)"},
	}
	for _, tc := range cases {
		c := &ddColumnJSON{Type: tc.typ, NumericScale: tc.scale, IsUnsigned: tc.unsigned, IsZerofill: tc.zero}
		if got := frmColumnTypeText(c, frmField{length: tc.length}); got != tc.want {
			t.Errorf("type %d (%d,%d): got %q, want %q", tc.typ, tc.length, tc.scale, got, tc.want)
		}
	}
}
