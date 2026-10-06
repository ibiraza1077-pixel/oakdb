package sql

import (
	"math"
	"testing"

	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

func mustParse(t *testing.T, src string) Stmt {
	t.Helper()
	s, err := ParseOne(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return s
}

func TestPrecedence(t *testing.T) {
	cases := map[string]string{
		"SELECT 1 + 2 * 3":                  "(1 + (2 * 3))",
		"SELECT a OR b AND c":               "(a OR (b AND c))",
		"SELECT NOT a = 1":                  "NOT (a = 1)",
		"SELECT 'x' || 1 + 2":               "('x' || (1 + 2))",
		"SELECT -2 * 3":                     "(-2 * 3)",
		"SELECT a - b - c":                  "((a - b) - c)",
		"SELECT x BETWEEN 1 AND 2 AND y":    "(x BETWEEN 1 AND 2 AND y)",
		"SELECT a NOT IN (1, 2)":            "a NOT IN (1, 2)",
		"SELECT t.a IS NOT NULL":            "t.a IS NOT NULL",
		"SELECT name NOT LIKE 'a%'":         "name NOT LIKE 'a%'",
		"SELECT COUNT(DISTINCT x)":          "count(DISTINCT x)",
		"SELECT CASE WHEN a THEN 1 END":     "CASE WHEN a THEN 1 END",
		"SELECT x::text":                    "CAST(x AS TEXT)",
		"SELECT -9223372036854775808":       "-9223372036854775808",
		"SELECT \"Mixed Case\" FROM t":      "Mixed Case",
		"SELECT a = 1 OR b <> 2 AND c != 3": "((a = 1) OR ((b <> 2) AND (c <> 3)))",
	}
	for src, want := range cases {
		s := mustParse(t, src).(*Select)
		if got := Format(s.Items[0].Expr); got != want {
			t.Errorf("%s\n got  %s\n want %s", src, got, want)
		}
	}
}

func TestMinIntLiteral(t *testing.T) {
	s := mustParse(t, "SELECT -9223372036854775808").(*Select)
	lit := s.Items[0].Expr.(*Literal)
	if lit.Val.K != record.Int || lit.Val.I != math.MinInt64 {
		t.Fatalf("got %#v", lit.Val)
	}
}

func TestSelectClauses(t *testing.T) {
	s := mustParse(t, `
		SELECT DISTINCT u.name AS n, COUNT(*) total, o.*
		FROM users u
		JOIN orders o ON o.user_id = u.id
		LEFT JOIN items AS i ON i.id = o.item_id, extra
		WHERE u.age >= 18 -- adults
		GROUP BY u.name
		HAVING COUNT(*) > 1
		ORDER BY total DESC, 1
		LIMIT 10 OFFSET 5;`).(*Select)
	if !s.Distinct || len(s.Items) != 3 || s.Items[0].Alias != "n" || s.Items[1].Alias != "total" || s.Items[2].StarTable != "o" {
		t.Fatalf("items: %+v", s.Items)
	}
	joins := []string{"", "INNER", "LEFT", "CROSS"}
	if len(s.From) != 4 {
		t.Fatalf("from: %+v", s.From)
	}
	for i, j := range joins {
		if s.From[i].Join != j {
			t.Errorf("from[%d].Join = %q want %q", i, s.From[i].Join, j)
		}
	}
	if s.From[0].Alias != "u" || s.From[2].Alias != "i" {
		t.Errorf("aliases: %+v", s.From)
	}
	if s.Where == nil || len(s.GroupBy) != 1 || s.Having == nil || len(s.OrderBy) != 2 || !s.OrderBy[0].Desc || s.Limit == nil || s.Offset == nil {
		t.Fatalf("clauses: %+v", s)
	}
}

func TestDDLAndDML(t *testing.T) {
	ct := mustParse(t, "CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name VARCHAR(50) NOT NULL, score DOUBLE PRECISION, ok BOOLEAN UNIQUE)").(*CreateTable)
	if !ct.IfNotExists || len(ct.Columns) != 4 || !ct.Columns[0].PrimaryKey || !ct.Columns[1].NotNull ||
		ct.Columns[1].Type != record.Text || ct.Columns[2].Type != record.Real || !ct.Columns[3].Unique {
		t.Fatalf("%+v", ct)
	}
	ct = mustParse(t, "CREATE TABLE t (a TEXT, b INT, PRIMARY KEY (a))").(*CreateTable)
	if !ct.Columns[0].PrimaryKey {
		t.Fatal("table-level primary key not applied")
	}
	ci := mustParse(t, "CREATE UNIQUE INDEX idx ON t (a, b)").(*CreateIndex)
	if !ci.Unique || ci.Table != "t" || len(ci.Columns) != 2 {
		t.Fatalf("%+v", ci)
	}
	ins := mustParse(t, "INSERT INTO t (a, b) VALUES ('x', 1), ('y', $1)").(*Insert)
	if len(ins.Rows) != 2 || len(ins.Columns) != 2 {
		t.Fatalf("%+v", ins)
	}
	if _, ok := ins.Rows[1][1].(*Param); !ok {
		t.Fatal("expected a parameter")
	}
	ins = mustParse(t, "INSERT INTO t SELECT * FROM u").(*Insert)
	if ins.Select == nil {
		t.Fatal("INSERT ... SELECT not parsed")
	}
	up := mustParse(t, "UPDATE t SET a = a || '!', b = b + 1 WHERE b < 10").(*Update)
	if len(up.Sets) != 2 || up.Where == nil {
		t.Fatalf("%+v", up)
	}
	if _, ok := mustParse(t, "DELETE FROM t").(*Delete); !ok {
		t.Fatal("delete")
	}
	if _, ok := mustParse(t, "EXPLAIN SELECT * FROM t").(*Explain); !ok {
		t.Fatal("explain")
	}
	for _, src := range []string{"BEGIN", "START TRANSACTION ISOLATION LEVEL SERIALIZABLE", "COMMIT", "END", "ROLLBACK", "SET extra_float_digits = 3", "SET search_path TO public"} {
		mustParse(t, src)
	}
}

func TestMultipleStatements(t *testing.T) {
	stmts, err := Parse("BEGIN; INSERT INTO t VALUES (1);; COMMIT;")
	if err != nil || len(stmts) != 3 {
		t.Fatalf("got %d statements, %v", len(stmts), err)
	}
	if s, err := ParseOne("  -- nothing\n"); s != nil || err != nil {
		t.Fatalf("empty query: %v %v", s, err)
	}
}

func TestSyntaxErrors(t *testing.T) {
	bad := []string{
		"SELECT", "SELECT FROM t", "SELECT * FROM", "INSERT INTO t VALUES (1",
		"SELECT 'unterminated", "SELECT * FROM t WHERE", "CREATE TABLE t (a BLOB)",
		"SELECT 1 2", "SELECT * FROM t WHERE a IN (SELECT 1)", "UPDATE t SET",
		"SELECT a FROM t ORDER", "SELECT @",
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("expected a syntax error for %q", src)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"SELECT a, b FROM t WHERE a = 1 AND b LIKE 'x%' ORDER BY a DESC LIMIT 3",
		"INSERT INTO t VALUES (1, 'a', 2.5, TRUE, NULL)",
		"CREATE TABLE t (a INT PRIMARY KEY, b TEXT)",
		"SELECT CASE WHEN a > 1 THEN 'x' ELSE 'y' END, CAST(b AS INT) FROM t",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		// Must never panic; errors are fine.
		stmts, err := Parse(src)
		if err == nil {
			for _, s := range stmts {
				if sel, ok := s.(*Select); ok {
					for _, it := range sel.Items {
						if it.Expr != nil {
							Format(it.Expr)
						}
					}
				}
			}
		}
	})
}
