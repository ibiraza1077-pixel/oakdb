// Package tests holds whole-system tests: differential testing against
// SQLite and crash-recovery testing.
package tests

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// Differential testing: generate thousands of random statements, run each
// on both OakDB and SQLite, and require identical results. SQLite is one of
// the most heavily tested programs ever written, so it makes an excellent
// oracle. The generator sticks to the large subset of SQL where the two
// databases define the same semantics (for example it never divides by
// zero or compares text with numbers, where they deliberately differ).

type gen struct {
	r *rand.Rand
}

func (g *gen) pick(options ...string) string { return options[g.r.Intn(len(options))] }

func (g *gen) intLit() string {
	if g.r.Intn(12) == 0 {
		return "NULL"
	}
	return strconv.Itoa(g.r.Intn(25) - 5)
}

func (g *gen) textLit() string {
	if g.r.Intn(12) == 0 {
		return "NULL"
	}
	return "'" + g.pick("a", "b", "ab", "ba", "abc", "bca", "", "zz", "a_b", "c") + "'"
}

func (g *gen) realLit() string {
	if g.r.Intn(12) == 0 {
		return "NULL"
	}
	return strconv.FormatFloat(float64(g.r.Intn(41)-10)/2, 'f', -1, 64)
}

// Columns by type, for each table alias.
var (
	intCols  = map[string][]string{"t1": {"id", "a"}, "t2": {"id", "t1_id", "d"}}
	textCols = map[string][]string{"t1": {"b"}, "t2": {"e"}}
	realCols = map[string][]string{"t1": {"c"}, "t2": {}}
)

func (g *gen) col(tables []string, kind string) (string, bool) {
	var all []string
	for _, t := range tables {
		var cols []string
		switch kind {
		case "int":
			cols = intCols[t]
		case "text":
			cols = textCols[t]
		case "real":
			cols = realCols[t]
		}
		for _, c := range cols {
			if len(tables) > 1 {
				all = append(all, t+"."+c)
			} else {
				all = append(all, c)
			}
		}
	}
	if len(all) == 0 {
		return "", false
	}
	return all[g.r.Intn(len(all))], true
}

func (g *gen) pred(tables []string, depth int) string {
	if depth < 2 && g.r.Intn(3) == 0 {
		switch g.r.Intn(3) {
		case 0:
			return "(" + g.pred(tables, depth+1) + " AND " + g.pred(tables, depth+1) + ")"
		case 1:
			return "(" + g.pred(tables, depth+1) + " OR " + g.pred(tables, depth+1) + ")"
		default:
			return "NOT (" + g.pred(tables, depth+1) + ")"
		}
	}
	op := g.pick("=", "<>", "<", "<=", ">", ">=")
	switch g.r.Intn(9) {
	case 0, 1, 2:
		c, _ := g.col(tables, "int")
		if g.r.Intn(4) == 0 {
			c2, _ := g.col(tables, "int")
			return c + " " + op + " " + c2
		}
		return c + " " + op + " " + g.intLit()
	case 3:
		c, _ := g.col(tables, "text")
		return c + " " + op + " " + g.textLit()
	case 4:
		if c, ok := g.col(tables, "real"); ok {
			return c + " " + op + " " + g.realLit()
		}
		c, _ := g.col(tables, "int")
		return c + " " + op + " " + g.realLit()
	case 5:
		c, _ := g.col(tables, g.pick("int", "text"))
		return c + " IS " + g.pick("", "NOT ") + "NULL"
	case 6:
		c, _ := g.col(tables, "int")
		n := 1 + g.r.Intn(4)
		var items []string
		for i := 0; i < n; i++ {
			items = append(items, g.intLit())
		}
		return c + g.pick(" ", " NOT ") + "IN (" + strings.Join(items, ", ") + ")"
	case 7:
		c, _ := g.col(tables, "int")
		return c + g.pick(" ", " NOT ") + "BETWEEN " + g.intLit() + " AND " + g.intLit()
	default:
		c, _ := g.col(tables, "text")
		return c + g.pick(" ", " NOT ") + "LIKE '" + g.pick("a%", "%b", "%a%", "_", "a_", "%", "abc", "b_") + "'"
	}
}

func (g *gen) valueExpr(tables []string) string {
	switch g.r.Intn(8) {
	case 0:
		c, _ := g.col(tables, "int")
		return c + " + " + strconv.Itoa(g.r.Intn(5))
	case 1:
		c, _ := g.col(tables, "int")
		c2, _ := g.col(tables, "int")
		return c + " * " + c2
	case 2:
		c, _ := g.col(tables, "text")
		return c + " || 'x'"
	case 3:
		c, _ := g.col(tables, "text")
		return g.pick("upper", "lower", "length") + "(" + c + ")"
	case 4:
		c, _ := g.col(tables, "int")
		return "coalesce(" + c + ", -1)"
	case 5:
		c, _ := g.col(tables, "int")
		return "CASE WHEN " + c + " > 3 THEN 'big' WHEN " + c + " IS NULL THEN 'none' ELSE 'small' END"
	case 6:
		c, _ := g.col(tables, "int")
		return "abs(" + c + ")"
	}
	c, _ := g.col(tables, g.pick("int", "text", "real"))
	if c == "" {
		c, _ = g.col(tables, "int")
	}
	return c
}

func (g *gen) statement() (sqlText string, ordered bool) {
	switch g.r.Intn(20) {
	case 0, 1, 2, 3:
		return fmt.Sprintf("INSERT INTO t1 (a, b, c) VALUES (%s, %s, %s)", g.intLit(), g.textLit(), g.realLit()), false
	case 4, 5, 6:
		return fmt.Sprintf("INSERT INTO t2 (t1_id, d, e) VALUES (%s, %s, %s)", g.intLit(), g.intLit(), g.textLit()), false
	case 7:
		return fmt.Sprintf("UPDATE t1 SET a = %s, b = %s WHERE %s", g.pick("a + 1", "a - 2", g.intLit()), g.pick("b", g.textLit(), "b || 'q'"), g.pred([]string{"t1"}, 0)), false
	case 8:
		return fmt.Sprintf("UPDATE t2 SET d = %s WHERE %s", g.pick("d * 2", "t1_id", g.intLit()), g.pred([]string{"t2"}, 0)), false
	case 9:
		if g.r.Intn(3) == 0 {
			tbl := g.pick("t1", "t2")
			return "DELETE FROM " + tbl + " WHERE " + g.pred([]string{tbl}, 0), false
		}
		return "", false
	case 10, 11:
		// Single-table select with a total order.
		var exprs []string
		for i := 0; i < 1+g.r.Intn(3); i++ {
			exprs = append(exprs, g.valueExpr([]string{"t1"}))
		}
		q := "SELECT id, " + strings.Join(exprs, ", ") + " FROM t1 WHERE " + g.pred([]string{"t1"}, 0) + " ORDER BY "
		if g.r.Intn(2) == 0 {
			q += g.pick("a", "b", "c", "a DESC", "b DESC") + ", "
		}
		q += "id"
		if g.r.Intn(3) == 0 {
			q += fmt.Sprintf(" LIMIT %d OFFSET %d", g.r.Intn(10), g.r.Intn(5))
		}
		return q, true
	case 12, 13:
		// Aggregation.
		group := g.pick("a", "b", "a % 3")
		return fmt.Sprintf("SELECT %s, count(*), count(c), sum(a), min(b), max(c), count(DISTINCT b) FROM t1 WHERE %s GROUP BY %s",
			group, g.pred([]string{"t1"}, 0), group), false
	case 14:
		return "SELECT count(*), sum(a), avg(a), min(a), max(b), total(c) FROM t1 WHERE " + g.pred([]string{"t1"}, 0), false
	case 15, 16:
		tables := []string{"t1", "t2"}
		join := g.pick("JOIN", "LEFT JOIN")
		on := g.pick("t2.t1_id = t1.id", "t2.t1_id = t1.a", "t2.d = t1.a AND t2.e = t1.b", "t2.d < t1.a")
		return fmt.Sprintf("SELECT t1.id, t2.id, %s FROM t1 %s t2 ON %s WHERE %s",
			g.valueExpr(tables), join, on, g.pred(tables, 1)), false
	case 17:
		return fmt.Sprintf("SELECT t1.b, count(t2.id), sum(t2.d) FROM t1 LEFT JOIN t2 ON t2.t1_id = t1.id GROUP BY t1.b HAVING count(*) %s %d",
			g.pick(">", ">=", "<"), g.r.Intn(4)), false
	case 18:
		return "SELECT DISTINCT " + g.pick("a", "b", "a, b", "c") + " FROM t1 WHERE " + g.pred([]string{"t1"}, 0), false
	}
	return "SELECT id, a, b, c FROM t1 WHERE id IN (" + g.intLit() + ", " + g.intLit() + ") OR id BETWEEN " + g.intLit() + " AND " + g.intLit() + " ORDER BY id", true
}

// normalize renders a value so that equal SQL values compare equal across
// the two engines (SQLite prints 2.0 for a REAL two; OakDB prints 2).
func normalize(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatFloat(float64(x), 'g', 12, 64)
	case float64:
		return strconv.FormatFloat(x, 'g', 12, 64)
	case []byte:
		return string(x)
	case string:
		return x
	case bool:
		if x {
			return "1"
		}
		return "0"
	}
	return fmt.Sprint(v)
}

func oakRows(res *engine.Result) []string {
	var out []string
	for _, row := range res.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			switch v.K {
			case record.Null:
				parts[i] = "NULL"
			case record.Int:
				parts[i] = normalize(v.I)
			case record.Real:
				parts[i] = normalize(v.F)
			case record.Bool:
				parts[i] = normalize(v.B)
			default:
				parts[i] = v.S
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func liteRows(t *testing.T, db *sql.DB, q string) ([]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			parts[i] = normalize(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out, rows.Err()
}

const schema = `
CREATE TABLE t1 (id INTEGER PRIMARY KEY, a INT, b TEXT, c REAL);
CREATE TABLE t2 (id INTEGER PRIMARY KEY, t1_id INT, d INT, e TEXT);
CREATE INDEX t1_a ON t1 (a);
CREATE INDEX t1_b_a ON t1 (b, a);
CREATE INDEX t2_t1 ON t2 (t1_id);
`

func TestDifferentialAgainstSQLite(t *testing.T) {
	iters := 1500
	if s := os.Getenv("OAKDB_DIFF_ITERS"); s != "" {
		iters, _ = strconv.Atoi(s)
	}
	if testing.Short() {
		iters = 300
	}
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			dir := t.TempDir()
			oak, err := engine.Open(filepath.Join(dir, "oak.db"), engine.Options{NoSync: true, CheckpointFrames: 200})
			if err != nil {
				t.Fatal(err)
			}
			defer oak.Close()
			sess := oak.NewSession()
			defer sess.Close()
			lite, err := sql.Open("sqlite", filepath.Join(dir, "lite.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer lite.Close()
			lite.SetMaxOpenConns(1)
			// SQLite's LIKE is case-insensitive by default; OakDB's follows
			// PostgreSQL and is case-sensitive. Make them agree.
			if _, err := lite.Exec("PRAGMA case_sensitive_like = ON"); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range strings.Split(strings.TrimSpace(schema), ";") {
				if strings.TrimSpace(stmt) == "" {
					continue
				}
				if _, err := sess.Exec(stmt); err != nil {
					t.Fatal(err)
				}
				if _, err := lite.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			g := &gen{r: rand.New(rand.NewSource(seed))}
			compared := 0
			for i := 0; i < iters; i++ {
				q, ordered := g.statement()
				if q == "" {
					continue
				}
				isSelect := strings.HasPrefix(q, "SELECT")
				var want []string
				var liteErr error
				if isSelect {
					want, liteErr = liteRows(t, lite, q)
				} else {
					_, liteErr = lite.Exec(q)
				}
				res, oakErr := sess.Exec(q)
				if liteErr != nil {
					// The generator should only produce statements SQLite accepts.
					t.Fatalf("SQLite rejected %q: %v", q, liteErr)
				}
				if oakErr != nil {
					t.Fatalf("statement %d: OakDB failed where SQLite succeeded\n  %s\n  error: %v", i, q, oakErr)
				}
				if !isSelect {
					// Compare full contents after every write, so a
					// divergence is reported at the statement that caused it.
					for _, tbl := range []string{"t1", "t2"} {
						cq := "SELECT * FROM " + tbl + " ORDER BY id"
						want, _ := liteRows(t, lite, cq)
						res, err := sess.Exec(cq)
						if err != nil {
							t.Fatal(err)
						}
						if got := oakRows(res[0]); strings.Join(got, "\n") != strings.Join(want, "\n") {
							t.Fatalf("statement %d left %s different\n  %s\n  OakDB:\n    %s\n  SQLite:\n    %s",
								i, tbl, q, strings.Join(got, "\n    "), strings.Join(want, "\n    "))
						}
					}
					continue
				}
				got := oakRows(res[0])
				if !ordered {
					sort.Strings(got)
					sort.Strings(want)
				}
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("statement %d: results differ\n  %s\n  OakDB (%d rows):\n    %s\n  SQLite (%d rows):\n    %s",
						i, q, len(got), strings.Join(got, "\n    "), len(want), strings.Join(want, "\n    "))
				}
				compared++
			}
			// Finally the full contents of both tables must match.
			for _, tbl := range []string{"t1", "t2"} {
				q := "SELECT * FROM " + tbl + " ORDER BY id"
				want, _ := liteRows(t, lite, q)
				res, err := sess.Exec(q)
				if err != nil {
					t.Fatal(err)
				}
				if got := oakRows(res[0]); strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("final contents of %s differ", tbl)
				}
			}
			if err := oak.Check(); err != nil {
				t.Fatalf("integrity check: %v", err)
			}
			t.Logf("compared %d queries", compared)
		})
	}
}
