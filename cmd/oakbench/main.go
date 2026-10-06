// Command oakbench compares OakDB with SQLite (native C, through cgo) on
// identical workloads, and writes docs/benchmarks.md and docs/benchmarks.svg.
//
// Both databases run embedded in the same process, on the same machine,
// with comparable durability: SQLite in WAL mode with synchronous=FULL and
// (on macOS) fullfsync, matching the F_FULLFSYNC that Go's File.Sync uses.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

type result struct {
	name, unit string
	ops        int
	oak, lite  time.Duration
}

func main() {
	rows := flag.Int("rows", 100000, "rows to load")
	out := flag.String("out", "docs", "directory for benchmarks.md and benchmarks.svg")
	flag.Parse()

	dir, err := os.MkdirTemp("", "oakbench")
	check(err)
	defer os.RemoveAll(dir)

	oak, err := engine.Open(filepath.Join(dir, "oak.db"), engine.Options{})
	check(err)
	defer oak.Close()
	s := oak.NewSession()

	lite, err := sql.Open("sqlite3", filepath.Join(dir, "lite.db")+"?_journal_mode=WAL&_synchronous=FULL")
	check(err)
	defer lite.Close()
	lite.SetMaxOpenConns(1)
	if runtime.GOOS == "darwin" {
		_, err = lite.Exec("PRAGMA fullfsync = ON")
		check(err)
	}

	const ddl = "CREATE TABLE bench (id INTEGER PRIMARY KEY, k INT, v TEXT)"
	mustOak(s, ddl)
	mustOak(s, "CREATE INDEX bench_k ON bench (k)")
	_, err = lite.Exec(ddl)
	check(err)
	_, err = lite.Exec("CREATE INDEX bench_k ON bench (k)")
	check(err)

	r := rand.New(rand.NewSource(1))
	keys := make([]int64, *rows)
	vals := make([]string, *rows)
	for i := range keys {
		keys[i] = int64(r.Intn(10000))
		vals[i] = randText(r, 40+r.Intn(30))
	}
	var results []result
	add := func(name, unit string, ops int, oakFn, liteFn func()) {
		fmt.Fprintf(os.Stderr, "%-40s", name)
		t0 := time.Now()
		oakFn()
		o := time.Since(t0)
		t0 = time.Now()
		liteFn()
		l := time.Since(t0)
		fmt.Fprintf(os.Stderr, " OakDB %8.0f/s   SQLite %8.0f/s\n", float64(ops)/o.Seconds(), float64(ops)/l.Seconds())
		results = append(results, result{name, unit, ops, o, l})
	}

	add(fmt.Sprintf("Bulk insert (%dk rows, 1 transaction)", *rows/1000), "rows/s", *rows, func() {
		mustOak(s, "BEGIN")
		for i := range keys {
			mustOak(s, "INSERT INTO bench VALUES ($1, $2, $3)", record.NewInt(int64(i+1)), record.NewInt(keys[i]), record.NewText(vals[i]))
		}
		mustOak(s, "COMMIT")
	}, func() {
		tx, err := lite.Begin()
		check(err)
		st, err := tx.Prepare("INSERT INTO bench VALUES (?, ?, ?)")
		check(err)
		for i := range keys {
			_, err := st.Exec(i+1, keys[i], vals[i])
			check(err)
		}
		check(tx.Commit())
	})

	lookups := 100000
	ids := make([]int64, lookups)
	for i := range ids {
		ids[i] = int64(r.Intn(*rows) + 1)
	}
	add("Point lookup by primary key", "queries/s", lookups, func() {
		for _, id := range ids {
			res, err := s.Exec("SELECT v FROM bench WHERE id = $1", record.NewInt(id))
			check(err)
			if len(res[0].Rows) != 1 {
				panic("missing row")
			}
		}
	}, func() {
		st, err := lite.Prepare("SELECT v FROM bench WHERE id = ?")
		check(err)
		var v string
		for _, id := range ids {
			check(st.QueryRow(id).Scan(&v))
		}
	})

	idx := 20000
	add("Secondary index lookup", "queries/s", idx, func() {
		for i := 0; i < idx; i++ {
			_, err := s.Exec("SELECT count(*) FROM bench WHERE k = $1", record.NewInt(int64(i%10000)))
			check(err)
		}
	}, func() {
		st, err := lite.Prepare("SELECT count(*) FROM bench WHERE k = ?")
		check(err)
		var n int
		for i := 0; i < idx; i++ {
			check(st.QueryRow(i % 10000).Scan(&n))
		}
	})

	scans := 2000
	add("Range scan (100 rows each)", "scans/s", scans, func() {
		for i := 0; i < scans; i++ {
			lo := int64(r.Intn(*rows - 100))
			_, err := s.Exec("SELECT sum(length(v)) FROM bench WHERE id >= $1 AND id < $2", record.NewInt(lo), record.NewInt(lo+100))
			check(err)
		}
	}, func() {
		st, err := lite.Prepare("SELECT sum(length(v)) FROM bench WHERE id >= ? AND id < ?")
		check(err)
		var n int
		for i := 0; i < scans; i++ {
			lo := r.Intn(*rows - 100)
			check(st.QueryRow(lo, lo+100).Scan(&n))
		}
	})

	aggs := 10
	add("Full-table GROUP BY aggregate", "queries/s", aggs, func() {
		for i := 0; i < aggs; i++ {
			_, err := s.Exec("SELECT k % 10, count(*), avg(id) FROM bench GROUP BY k % 10")
			check(err)
		}
	}, func() {
		for i := 0; i < aggs; i++ {
			rs, err := lite.Query("SELECT k % 10, count(*), avg(id) FROM bench GROUP BY k % 10")
			check(err)
			for rs.Next() {
			}
			rs.Close()
		}
	})

	commits := 300
	add("Durable single-row commits (fsync)", "commits/s", commits, func() {
		for i := 0; i < commits; i++ {
			mustOak(s, "INSERT INTO bench (k, v) VALUES ($1, 'x')", record.NewInt(int64(i)))
		}
	}, func() {
		for i := 0; i < commits; i++ {
			_, err := lite.Exec("INSERT INTO bench (k, v) VALUES (?, 'x')", i)
			check(err)
		}
	})

	check(os.MkdirAll(*out, 0o755))
	check(os.WriteFile(filepath.Join(*out, "benchmarks.md"), []byte(markdown(results, *rows)), 0o644))
	check(os.WriteFile(filepath.Join(*out, "benchmarks.svg"), []byte(chart(results)), 0o644))
	fmt.Fprintf(os.Stderr, "wrote %s/benchmarks.md and %s/benchmarks.svg\n", *out, *out)
}

func randText(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = "abcdefghijklmnopqrstuvwxyz "[r.Intn(27)]
	}
	return string(b)
}

func mustOak(s *engine.Session, q string, params ...record.Value) {
	if _, err := s.Exec(q, params...); err != nil {
		panic(fmt.Sprintf("%s: %v", q, err))
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func rate(ops int, d time.Duration) float64 { return float64(ops) / d.Seconds() }

func human(x float64) string {
	switch {
	case x >= 1e6:
		return fmt.Sprintf("%.1fM", x/1e6)
	case x >= 1e4:
		return fmt.Sprintf("%.0fk", x/1e3)
	case x >= 1e3:
		return fmt.Sprintf("%.1fk", x/1e3)
	}
	return fmt.Sprintf("%.0f", x)
}

func markdown(rs []result, rows int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmarks\n\nOakDB compared with SQLite %s (native C, via cgo) on the same machine (%s/%s, %d CPUs), %d rows of `(id INTEGER PRIMARY KEY, k INT, v TEXT)` with an index on `k`.\n\n",
		sqliteVersion(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), rows)
	b.WriteString("Both run embedded in one process. SQLite uses WAL mode with `synchronous=FULL` and `fullfsync` on macOS, so both make every commit durable the same way. SQLite runs prepared statements; OakDB caches parsed statements but plans every query again.\n\n")
	b.WriteString("| Workload | OakDB | SQLite | OakDB / SQLite |\n|---|---:|---:|---:|\n")
	for _, r := range rs {
		o, l := rate(r.ops, r.oak), rate(r.ops, r.lite)
		fmt.Fprintf(&b, "| %s | %s %s | %s %s | %.2f× |\n", r.name, human(o), r.unit, human(l), r.unit, o/l)
	}
	b.WriteString("\nRegenerate with `go run ./cmd/oakbench`. Numbers vary between machines; the ratio is the useful part.\n\n")
	b.WriteString("## Reading the results honestly\n\n")
	b.WriteString("SQLite has had more than 20 years of tuning. OakDB is a teaching-sized engine, so the interesting question is *where* the gap is and why:\n\n")
	b.WriteString("- **Per-query overhead.** OakDB plans every statement from scratch and decodes whole rows into Go values, while SQLite runs precompiled bytecode. This dominates small point lookups.\n")
	b.WriteString("- **Durable commits** are bound by the disk's flush time for both engines, so they land close together.\n")
	b.WriteString("- **Scans and aggregates** stress the B-tree and the executor; OakDB's copy-free cursor over immutable cached pages keeps these competitive.\n")
	return b.String()
}

func sqliteVersion() string {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return ""
	}
	defer db.Close()
	var v string
	db.QueryRow("SELECT sqlite_version()").Scan(&v)
	return v
}

// chart draws OakDB's throughput relative to SQLite as horizontal bars on a
// log scale centred on 1× (parity), so "2× slower" and "2× faster" are the
// same distance from the line.
func chart(rs []result) string {
	const (
		w      = 760
		rowH   = 44
		top    = 64
		labelW = 270
		right  = 70
		barH   = 18
		minLog = -1.0 // 0.1×
		maxLog = 1.0  // 10×
	)
	h := top + rowH*len(rs) + 44
	plotW := float64(w - labelW - right)
	x := func(ratio float64) float64 {
		l := math.Log10(ratio)
		l = math.Max(minLog, math.Min(maxLog, l))
		return float64(labelW) + (l-minLog)/(maxLog-minLog)*plotW
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="t d">
<title id="t">OakDB throughput relative to SQLite</title>
<desc id="d">Bar length shows OakDB operations per second divided by SQLite's, on a log scale; the line at 1x is parity.</desc>
<style>
  .bg{fill:#fcfcfb} .ink{fill:#0b0b0b} .muted{fill:#52514e} .grid{stroke:#e4e3de} .par{stroke:#52514e} .bar{fill:#2a78d6}
  text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
  @media (prefers-color-scheme: dark){ .bg{fill:#1a1a19} .ink{fill:#ffffff} .muted{fill:#c3c2b7} .grid{stroke:#2e2e2c} .par{stroke:#c3c2b7} .bar{fill:#3987e5} }
</style>
<rect class="bg" width="%d" height="%d" rx="12"/>
<text class="ink" x="24" y="34" font-size="17" font-weight="600">OakDB throughput relative to SQLite</text>
<text class="muted" x="24" y="54" font-size="12.5">Log scale. Left of the line: slower than SQLite. Right: faster.</text>
`, w, h, w, h, w, h)
	ticks := []float64{0.1, 0.2, 0.5, 1, 2, 5, 10}
	gridBottom := top + rowH*len(rs)
	for _, t := range ticks {
		cls := "grid"
		if t == 1 {
			cls = "par"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%.1f" x2="%.1f" y1="%d" y2="%d" stroke-width="1"/>`+"\n", cls, x(t), x(t), top-6, gridBottom)
		label := fmt.Sprintf("%g×", t)
		if t == 1 {
			label = "1× (parity)"
		}
		fmt.Fprintf(&b, `<text class="muted" x="%.1f" y="%d" font-size="11.5" text-anchor="middle">%s</text>`+"\n", x(t), gridBottom+18, label)
	}
	for i, r := range rs {
		ratio := rate(r.ops, r.oak) / rate(r.ops, r.lite)
		y := top + i*rowH
		cy := y + rowH/2
		fmt.Fprintf(&b, `<text class="ink" x="24" y="%d" font-size="13" dominant-baseline="middle">%s</text>`+"\n", cy, r.name)
		x0, x1 := x(1), x(ratio)
		left, width := math.Min(x0, x1), math.Abs(x1-x0)
		if width < 2 {
			width = 2
		}
		// Rounded at the data end only: a flat edge at the parity line.
		fmt.Fprintf(&b, `<rect class="bar" x="%.1f" y="%d" width="%.1f" height="%d" rx="4"/>`+"\n", left, cy-barH/2, width, barH)
		if width > 8 {
			// Square off the parity end; the data end stays rounded.
			flatX := left
			if x1 < x0 {
				flatX = left + width - 4
			}
			fmt.Fprintf(&b, `<rect class="bar" x="%.1f" y="%d" width="4" height="%d"/>`+"\n", flatX, cy-barH/2, barH)
		}
		lx, anchor := x1+6, "start"
		if x1 < x0 {
			lx, anchor = x1-6, "end"
		}
		fmt.Fprintf(&b, `<text class="ink" x="%.1f" y="%d" font-size="12.5" font-weight="600" text-anchor="%s" dominant-baseline="middle">%.2f×</text>`+"\n", lx, cy, anchor, ratio)
	}
	b.WriteString("</svg>\n")
	return b.String()
}
