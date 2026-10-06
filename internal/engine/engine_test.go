package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

func openDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.oak")
	db, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// run executes SQL and returns the last result as "a|b\nc|d".
func run(t *testing.T, s *Session, q string, params ...record.Value) string {
	t.Helper()
	res, err := s.Exec(q, params...)
	if err != nil {
		t.Fatalf("%s\n  error: %v", q, err)
	}
	if len(res) == 0 {
		return ""
	}
	return format(res[len(res)-1])
}

func format(r *Result) string {
	var lines []string
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = v.String()
		}
		lines = append(lines, strings.Join(parts, "|"))
	}
	return strings.Join(lines, "\n")
}

func expectErr(t *testing.T, s *Session, q, code string) {
	t.Helper()
	_, err := s.Exec(q)
	if err == nil {
		t.Fatalf("%s: expected error %s, got none", q, code)
	}
	if e := AsError(err); e.Code != code {
		t.Fatalf("%s: expected code %s, got %s (%v)", q, code, e.Code, e)
	}
}

func check(t *testing.T, s *Session, q, want string) {
	t.Helper()
	if got := run(t, s, q); got != want {
		t.Errorf("%s\n got:\n%s\n want:\n%s", q, got, want)
	}
}

func setupShop(t *testing.T) *Session {
	db, _ := openDB(t)
	s := db.NewSession()
	run(t, s, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, city TEXT, age INT);
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT, item TEXT, price REAL);
		CREATE INDEX orders_user ON orders (user_id);
		INSERT INTO users VALUES (1, 'ada', 'london', 36), (2, 'linus', 'helsinki', 28),
			(3, 'grace', 'new york', 45), (4, 'alan', 'london', NULL);
		INSERT INTO orders (user_id, item, price) VALUES
			(1, 'book', 12.5), (1, 'pen', 1.25), (2, 'laptop', 999), (3, 'book', 12.5), (9, 'ghost', 1);
	`)
	return s
}

func TestBasicQueries(t *testing.T) {
	s := setupShop(t)
	check(t, s, "SELECT name FROM users WHERE id = 2", "linus")
	check(t, s, "SELECT name FROM users WHERE age > 30 ORDER BY age DESC", "grace\nada")
	check(t, s, "SELECT name FROM users WHERE age IS NULL", "alan")
	check(t, s, "SELECT name FROM users WHERE city LIKE 'l%' ORDER BY name", "ada\nalan")
	check(t, s, "SELECT name FROM users WHERE name ILIKE 'A%' ORDER BY 1", "ada\nalan")
	check(t, s, "SELECT name FROM users WHERE id IN (3, 1, 99) ORDER BY id", "ada\ngrace")
	check(t, s, "SELECT name FROM users WHERE id NOT IN (1, 2) ORDER BY id", "grace\nalan")
	check(t, s, "SELECT name FROM users WHERE age BETWEEN 28 AND 36 ORDER BY id", "ada\nlinus")
	check(t, s, "SELECT id, name FROM users ORDER BY id LIMIT 2 OFFSET 1", "2|linus\n3|grace")
	check(t, s, "SELECT DISTINCT city FROM users ORDER BY city", "helsinki\nlondon\nnew york")
	check(t, s, "SELECT 1 + 2 * 3, 7 / 2, 7 % 3, 7.0 / 2, 'a' || 'b', -(3)", "7|3|1|3.5|ab|-3")
	check(t, s, "SELECT upper(name), length(name) FROM users WHERE id = 1", "ADA|3")
	check(t, s, "SELECT coalesce(age, -1) FROM users WHERE id = 4", "-1")
	check(t, s, "SELECT CASE WHEN age > 40 THEN 'old' WHEN age IS NULL THEN '?' ELSE 'young' END FROM users ORDER BY id", "young\nyoung\nold\n?")
	check(t, s, "SELECT CAST('42' AS INTEGER) + 1, CAST(2.5 AS INT), 3::text || '!'", "43|2|3!")
	check(t, s, "SELECT * FROM users WHERE id = 3", "3|grace|new york|45")
	check(t, s, "SELECT u.* FROM users u WHERE u.id = 3", "3|grace|new york|45")
	check(t, s, "SELECT NULL = NULL, NULL IS NULL, 1 = 1 AND NULL, 1 = 0 AND NULL, 1 = 1 OR NULL", "NULL|t|NULL|f|t")
	check(t, s, "SELECT substr('database', 1, 4), replace('a-b', '-', '+'), trim('  x '), round(2.567, 2)", "data|a+b|x|2.57")
}

func TestAggregates(t *testing.T) {
	s := setupShop(t)
	check(t, s, "SELECT count(*), count(age), sum(age), min(age), max(age) FROM users", "4|3|109|28|45")
	check(t, s, "SELECT avg(age) FROM users", fmt.Sprint(109.0/3))
	check(t, s, "SELECT city, count(*) AS n FROM users GROUP BY city ORDER BY n DESC, city", "london|2\nhelsinki|1\nnew york|1")
	check(t, s, "SELECT city, count(*) FROM users GROUP BY 1 HAVING count(*) > 1", "london|2")
	check(t, s, "SELECT count(DISTINCT item) FROM orders", "4")
	check(t, s, "SELECT count(*), sum(price) FROM orders WHERE price > 10000", "0|NULL")
	check(t, s, "SELECT item, sum(price) FROM orders GROUP BY item HAVING sum(price) > 20 ORDER BY 2", "book|25\nlaptop|999")
	check(t, s, "SELECT upper(city) c, count(*) FROM users GROUP BY upper(city) ORDER BY c", "HELSINKI|1\nLONDON|2\nNEW YORK|1")
	expectErr(t, s, "SELECT name, count(*) FROM users", CodeGrouping)
	expectErr(t, s, "SELECT * FROM users WHERE count(*) > 1", CodeGrouping)
}

func TestJoins(t *testing.T) {
	s := setupShop(t)
	check(t, s, `SELECT u.name, o.item FROM users u JOIN orders o ON o.user_id = u.id ORDER BY u.id, o.id`,
		"ada|book\nada|pen\nlinus|laptop\ngrace|book")
	check(t, s, `SELECT u.name, o.item FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.id >= 3 ORDER BY u.id`,
		"grace|book\nalan|NULL")
	check(t, s, `SELECT o.item, u.name FROM orders o LEFT JOIN users u ON u.id = o.user_id ORDER BY o.id`,
		"book|ada\npen|ada\nlaptop|linus\nbook|grace\nghost|NULL")
	check(t, s, `SELECT u.name, sum(o.price) FROM users u, orders o WHERE o.user_id = u.id GROUP BY u.name ORDER BY 2 DESC`,
		"linus|999\nada|13.75\ngrace|12.5")
	// Hash join on a non-indexed column.
	check(t, s, `SELECT a.name, b.name FROM users a JOIN users b ON a.city = b.city AND a.id < b.id`, "ada|alan")
	// Three-way join.
	run(t, s, "CREATE TABLE cities (name TEXT PRIMARY KEY, country TEXT); INSERT INTO cities VALUES ('london', 'uk'), ('helsinki', 'fi')")
	check(t, s, `SELECT u.name, c.country, o.item FROM users u JOIN cities c ON c.name = u.city JOIN orders o ON o.user_id = u.id ORDER BY o.id`,
		"ada|uk|book\nada|uk|pen\nlinus|fi|laptop")
	expectErr(t, s, "SELECT id FROM users u JOIN orders o ON o.user_id = u.id", CodeAmbiguousColumn)
}

func TestPlansUseIndexes(t *testing.T) {
	s := setupShop(t)
	plan := func(q string) string { return run(t, s, "EXPLAIN "+q) }
	if p := plan("SELECT * FROM users WHERE id = 3"); !strings.Contains(p, "Primary Key Lookup on users (id = 3)") {
		t.Errorf("pk lookup not used:\n%s", p)
	}
	if p := plan("SELECT * FROM orders WHERE user_id = 1"); !strings.Contains(p, "Index Scan using orders_user") {
		t.Errorf("index not used:\n%s", p)
	}
	if p := plan("SELECT count(*) FROM orders WHERE user_id = 1"); !strings.Contains(p, "Index Only Scan using orders_user") {
		t.Errorf("index-only scan not used:\n%s", p)
	}
	check(t, s, "SELECT count(*), sum(id) FROM orders WHERE user_id = 1", "2|3")
	check(t, s, "SELECT user_id, id FROM orders WHERE user_id >= 2 ORDER BY id", "2|3\n3|4\n9|5")
	if p := plan("SELECT * FROM users WHERE id > 1 AND id <= 3"); !strings.Contains(p, "id > 1 AND id <= 3") {
		t.Errorf("pk range not used:\n%s", p)
	}
	if p := plan("SELECT * FROM users ORDER BY id LIMIT 1"); strings.Contains(p, "Sort") {
		t.Errorf("unnecessary sort:\n%s", p)
	}
	if p := plan("SELECT * FROM users u JOIN orders o ON o.user_id = u.id"); !strings.Contains(p, "Index Nested Loop") {
		t.Errorf("index join not used:\n%s", p)
	}
	if p := plan("SELECT * FROM users a JOIN users b ON a.city = b.city"); !strings.Contains(p, "Hash Join") {
		t.Errorf("hash join not used:\n%s", p)
	}
	// Index results must match full-scan results.
	check(t, s, "SELECT id FROM orders WHERE user_id = 1 ORDER BY id", "1\n2")
	check(t, s, "SELECT id FROM users WHERE id > 1 AND id <= 3", "2\n3")
	check(t, s, "SELECT id FROM users WHERE id >= 2.5", "3\n4")
	check(t, s, "SELECT id FROM users WHERE id = '2'", "2")
	check(t, s, "SELECT id FROM users WHERE id < 3 AND id > 3", "")
	check(t, s, "SELECT id FROM orders WHERE user_id IN (2, 3, NULL) ORDER BY id", "3\n4")
}

func TestConstraints(t *testing.T) {
	s := setupShop(t)
	expectErr(t, s, "INSERT INTO users VALUES (1, 'dup', NULL, NULL)", CodeUniqueViolation)
	expectErr(t, s, "INSERT INTO users (id) VALUES (10)", CodeNotNullViolation)
	expectErr(t, s, "INSERT INTO users VALUES (11, 'x', 'y', 'not a number')", CodeDatatypeMismatch)
	expectErr(t, s, "INSERT INTO nope VALUES (1)", CodeUndefinedTable)
	expectErr(t, s, "SELECT nope FROM users", CodeUndefinedColumn)
	expectErr(t, s, "SELECT 1 / 0", CodeDivisionByZero)
	expectErr(t, s, "SELECT 9223372036854775807 + 1", CodeNumericRange)
	expectErr(t, s, "CREATE TABLE users (x INT)", CodeDuplicateTable)
	run(t, s, "CREATE TABLE emails (id INTEGER PRIMARY KEY, addr TEXT UNIQUE)")
	run(t, s, "INSERT INTO emails (addr) VALUES ('a@x'), (NULL), (NULL)")
	expectErr(t, s, "INSERT INTO emails (addr) VALUES ('a@x')", CodeUniqueViolation)
	check(t, s, "SELECT id, addr FROM emails ORDER BY id", "1|a@x\n2|NULL\n3|NULL")
	run(t, s, "UPDATE emails SET addr = 'b@x' WHERE id = 1")
	run(t, s, "INSERT INTO emails (addr) VALUES ('a@x')")
	expectErr(t, s, "CREATE UNIQUE INDEX u_city ON users (city)", CodeUniqueViolation)
}

func TestUpdateDelete(t *testing.T) {
	s := setupShop(t)
	check(t, s, "UPDATE users SET age = age + 1 WHERE city = 'london'", "")
	check(t, s, "SELECT id, age FROM users WHERE city = 'london' ORDER BY id", "1|37\n4|NULL")
	// Changing the primary key moves the row; indexes follow.
	run(t, s, "UPDATE orders SET id = id + 100, user_id = 2 WHERE item = 'pen'")
	check(t, s, "SELECT id, item FROM orders WHERE user_id = 2 ORDER BY id", "3|laptop\n102|pen")
	res, err := s.Exec("DELETE FROM orders WHERE price < 2")
	if err != nil || res[0].Tag != "DELETE 2" {
		t.Fatalf("delete: %v %v", res, err)
	}
	check(t, s, "SELECT count(*) FROM orders", "3")
	check(t, s, "SELECT id FROM orders WHERE user_id = 2", "3")
	// Swap values without seeing our own writes.
	run(t, s, "CREATE TABLE sw (k INT PRIMARY KEY, v INT); INSERT INTO sw VALUES (1, 10), (2, 20)")
	run(t, s, "UPDATE sw SET k = k + 10")
	check(t, s, "SELECT k, v FROM sw ORDER BY k", "11|10\n12|20")
	run(t, s, "DELETE FROM sw")
	check(t, s, "SELECT count(*) FROM sw", "0")
}

func TestTransactions(t *testing.T) {
	db, path := openDB(t)
	a, b := db.NewSession(), db.NewSession()
	run(t, a, "CREATE TABLE acct (id INT PRIMARY KEY, bal INT)")
	run(t, a, "INSERT INTO acct VALUES (1, 100), (2, 0)")

	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = bal - 30 WHERE id = 1")
	run(t, a, "UPDATE acct SET bal = bal + 30 WHERE id = 2")
	if a.TxStatus() != 'T' {
		t.Fatal("expected in-transaction status")
	}
	// b cannot see a's uncommitted transfer.
	check(t, b, "SELECT sum(bal), max(bal) FROM acct", "100|100")
	run(t, a, "COMMIT")
	check(t, b, "SELECT bal FROM acct ORDER BY id", "70\n30")

	// Rollback undoes everything, including DDL.
	run(t, a, "BEGIN; DELETE FROM acct; CREATE TABLE tmp (x INT); ROLLBACK")
	check(t, b, "SELECT count(*) FROM acct", "2")
	expectErr(t, b, "SELECT * FROM tmp", CodeUndefinedTable)

	// An error aborts the transaction until ROLLBACK.
	run(t, a, "BEGIN; INSERT INTO acct VALUES (3, 5)")
	expectErr(t, a, "INSERT INTO acct VALUES (3, 5)", CodeUniqueViolation)
	if a.TxStatus() != 'E' {
		t.Fatal("expected failed-transaction status")
	}
	expectErr(t, a, "SELECT 1", CodeInFailedTxn)
	if got := run(t, a, "COMMIT"); got != "" {
		t.Fatal(got)
	}
	check(t, b, "SELECT count(*) FROM acct", "2")

	// Snapshot isolation: a's view is fixed at its first read.
	run(t, a, "BEGIN")
	check(t, a, "SELECT bal FROM acct WHERE id = 1", "70")
	run(t, b, "UPDATE acct SET bal = 0 WHERE id = 1")
	check(t, a, "SELECT bal FROM acct WHERE id = 1", "70")
	// Writing on a stale snapshot is a serialization failure.
	expectErr(t, a, "UPDATE acct SET bal = 1 WHERE id = 2", CodeSerialization)
	run(t, a, "ROLLBACK")

	// Everything survives a reopen.
	a.Close()
	b.Close()
	db.Close()
	db2, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	check(t, db2.NewSession(), "SELECT id, bal FROM acct ORDER BY id", "1|0\n2|30")
}

func TestParams(t *testing.T) {
	s := setupShop(t)
	if got := run(t, s, "SELECT name FROM users WHERE id = $1", record.NewInt(3)); got != "grace" {
		t.Fatal(got)
	}
	// Text parameters are compared like untyped literals.
	if got := run(t, s, "SELECT name FROM users WHERE id = $1 OR age = $2 ORDER BY id", record.NewText("1"), record.NewText("28")); got != "ada\nlinus" {
		t.Fatal(got)
	}
	run(t, s, "INSERT INTO users VALUES ($1, $2, $3, $4)", record.NewText("7"), record.NewText("eve"), record.Value{}, record.NewText("22"))
	check(t, s, "SELECT age + 1 FROM users WHERE id = 7", "23")
}

func TestLargeTableAndDropReclaimsSpace(t *testing.T) {
	db, _ := openDB(t)
	s := db.NewSession()
	run(t, s, "CREATE TABLE big (id INT PRIMARY KEY, body TEXT, n INT)")
	run(t, s, "CREATE INDEX big_n ON big (n)")
	run(t, s, "BEGIN")
	for i := 0; i < 3000; i++ {
		run(t, s, "INSERT INTO big VALUES ($1, $2, $3)", record.NewInt(int64(i)), record.NewText(strings.Repeat("x", i%2000)), record.NewInt(int64(i%7)))
	}
	run(t, s, "COMMIT")
	check(t, s, "SELECT count(*), sum(length(body)) FROM big", fmt.Sprintf("3000|%d", sumLens()))
	check(t, s, "SELECT count(*) FROM big WHERE n = 3", "429")
	before := db.Stats().PageCount
	run(t, s, "DROP TABLE big")
	run(t, s, "CREATE TABLE big2 (id INT PRIMARY KEY, body TEXT)")
	run(t, s, "BEGIN")
	for i := 0; i < 1000; i++ {
		run(t, s, "INSERT INTO big2 VALUES ($1, $2)", record.NewInt(int64(i)), record.NewText(strings.Repeat("y", 1000)))
	}
	run(t, s, "COMMIT")
	if after := db.Stats().PageCount; after > before {
		t.Fatalf("freed pages were not reused: %d -> %d", before, after)
	}
}

func sumLens() int {
	n := 0
	for i := 0; i < 3000; i++ {
		n += i % 2000
	}
	return n
}

func TestShowAndSet(t *testing.T) {
	db, _ := openDB(t)
	s := db.NewSession()
	run(t, s, "SET client_encoding = 'UTF8'")
	check(t, s, "SHOW transaction_isolation", "serializable")
	check(t, s, "SELECT version() LIKE 'OakDB%'", "t")
}
