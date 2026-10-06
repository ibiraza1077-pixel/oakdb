package pgwire

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
)

func startServer(t *testing.T) string {
	t.Helper()
	db, err := engine.Open(filepath.Join(t.TempDir(), "wire.oak"), engine.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{DB: db}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return fmt.Sprintf("postgres://oak@%s/oakdb?sslmode=disable", ln.Addr())
}

func connect(t *testing.T, url string, mode pgx.QueryExecMode) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = mode
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// TestPgxDriver drives OakDB with a real PostgreSQL driver in every query
// mode it supports: prepared statements with binary values, cached
// descriptions, and the plain-text simple protocol.
func TestPgxDriver(t *testing.T) {
	url := startServer(t)
	ctx := context.Background()
	modes := map[string]pgx.QueryExecMode{
		"extended":  pgx.QueryExecModeCacheStatement,
		"describe":  pgx.QueryExecModeDescribeExec,
		"simple":    pgx.QueryExecModeSimpleProtocol,
		"exec-only": pgx.QueryExecModeExec,
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			c := connect(t, url, mode)
			table := "people_" + name
			if name == "exec-only" {
				table = "people_exec"
			}
			if _, err := c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, name TEXT NOT NULL, score REAL, active BOOLEAN)`, table)); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 5; i++ {
				if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES ($1, $2, $3, $4)", table),
					i, fmt.Sprintf("p%d", i), float64(i)*1.5, i%2 == 0); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := c.Query(ctx, fmt.Sprintf("SELECT id, name, score, active FROM %s WHERE id >= $1 ORDER BY id", table), 4)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var id int64
				var nm string
				var score float64
				var active bool
				if err := rows.Scan(&id, &nm, &score, &active); err != nil {
					t.Fatal(err)
				}
				got = append(got, fmt.Sprintf("%d %s %v %v", id, nm, score, active))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != "[4 p4 6 true 5 p5 7.5 false]" {
				t.Fatalf("rows: %v", got)
			}
			var n int64
			var avg *float64
			if err := c.QueryRow(ctx, fmt.Sprintf("SELECT count(*), avg(score) FROM %s WHERE name LIKE $1", table), "p%").Scan(&n, &avg); err != nil {
				t.Fatal(err)
			}
			if n != 5 || avg == nil || *avg != 4.5 {
				t.Fatalf("count=%d avg=%v", n, avg)
			}
			var missing *string
			if err := c.QueryRow(ctx, "SELECT NULL").Scan(&missing); err != nil || missing != nil {
				t.Fatalf("null: %v %v", missing, err)
			}
		})
	}
}

func TestErrorsAndTransactions(t *testing.T) {
	url := startServer(t)
	ctx := context.Background()
	c := connect(t, url, pgx.QueryExecModeCacheStatement)
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Exec(ctx, "INSERT INTO t VALUES (1)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected unique violation, got %v", err)
	}
	// The connection is still usable after an error.
	_, err = c.Exec(ctx, "SELEC 1")
	if !errors.As(err, &pgErr) || pgErr.Code != "42601" {
		t.Fatalf("expected syntax error, got %v", err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = c.Begin(ctx)
	tx.Exec(ctx, "INSERT INTO t VALUES (3)")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, _ := c.Query(ctx, "SELECT id FROM t ORDER BY id")
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if fmt.Sprint(ids) != "[1 3]" {
		t.Fatalf("ids after rollback/commit: %v", ids)
	}
	// Two connections: the second sees committed data only.
	c2 := connect(t, url, pgx.QueryExecModeSimpleProtocol)
	tx, _ = c.Begin(ctx)
	tx.Exec(ctx, "DELETE FROM t")
	var n int
	c2.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n)
	if n != 2 {
		t.Fatalf("uncommitted delete was visible: %d", n)
	}
	tx.Commit(ctx)
	c2.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n)
	if n != 0 {
		t.Fatalf("committed delete not visible: %d", n)
	}
}

func TestMultiStatementSimpleQuery(t *testing.T) {
	url := startServer(t)
	ctx := context.Background()
	c := connect(t, url, pgx.QueryExecModeSimpleProtocol)
	results, err := c.PgConn().Exec(ctx, "CREATE TABLE m (x INT); INSERT INTO m VALUES (1), (2); SELECT sum(x) FROM m").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || string(results[2].Rows[0][0]) != "3" || results[1].CommandTag.String() != "INSERT 0 2" {
		t.Fatalf("results: %+v", results)
	}
}
