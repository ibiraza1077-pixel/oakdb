// Package engine ties the storage layers together into a SQL database:
// sessions, transactions, planning and execution.
package engine

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ibiraza1077-pixel/oakdb/internal/btree"
	"github.com/ibiraza1077-pixel/oakdb/internal/catalog"
	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// Options configure a database.
type Options = pager.Options

// DB is an open database. It is safe for concurrent use by many sessions.
type DB struct {
	pager *pager.Pager

	catMu    sync.Mutex
	catCache map[uint64]*catalog.Catalog

	started  time.Time
	queries  atomic.Uint64
	errors   atomic.Uint64
	sessions atomic.Int64
}

// Open opens or creates a database file.
func Open(path string, opts Options) (*DB, error) {
	p, err := pager.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return &DB{pager: p, catCache: map[uint64]*catalog.Catalog{}, started: time.Now()}, nil
}

// Close flushes and closes the database.
func (db *DB) Close() error { return db.pager.Close() }

// Stats is a snapshot of the database's counters.
type Stats struct {
	pager.Stats
	Queries        uint64
	Errors         uint64
	ActiveSessions int64
	Uptime         time.Duration
}

// Stats returns current counters.
func (db *DB) Stats() Stats {
	return Stats{Stats: db.pager.Stats(), Queries: db.queries.Load(), Errors: db.errors.Load(),
		ActiveSessions: db.sessions.Load(), Uptime: time.Since(db.started)}
}

// catalogFor returns the schema visible to r, from cache when possible.
func (db *DB) catalogFor(r btree.Reader) (*catalog.Catalog, error) {
	tok, root, err := catalog.Token(r)
	if err != nil {
		return nil, err
	}
	db.catMu.Lock()
	c, ok := db.catCache[tok]
	db.catMu.Unlock()
	if ok {
		return c, nil
	}
	c, err = catalog.Load(r, root)
	if err != nil {
		return nil, err
	}
	db.catMu.Lock()
	if len(db.catCache) > 32 {
		db.catCache = map[uint64]*catalog.Catalog{}
	}
	db.catCache[tok] = c
	db.catMu.Unlock()
	return c, nil
}

// Result is the outcome of one statement.
type Result struct {
	Columns []Column
	Rows    []Row
	Tag     string // PostgreSQL command tag, e.g. "SELECT 3" or "INSERT 0 1"
}

// --- sessions and transactions ---------------------------------------------

// Session is one client connection's state. It is not safe for concurrent use.
type Session struct {
	db     *DB
	cache  map[string][]sql.Stmt // parsed statements, keyed by query text
	tx     *txn
	failed bool // an error happened inside an explicit transaction
	closed bool
}

// NewSession starts a session.
func (db *DB) NewSession() *Session {
	db.sessions.Add(1)
	return &Session{db: db}
}

// Close rolls back any open transaction.
func (s *Session) Close() {
	if s.closed {
		return
	}
	s.closed = true
	if s.tx != nil {
		s.tx.rollback()
		s.tx = nil
	}
	s.db.sessions.Add(-1)
}

// TxStatus reports the transaction state as the wire protocol expects:
// 'I' idle, 'T' in a transaction, 'E' in a failed transaction.
func (s *Session) TxStatus() byte {
	switch {
	case s.failed:
		return 'E'
	case s.tx != nil:
		return 'T'
	}
	return 'I'
}

// txn wraps a pager snapshot that may be upgraded to a write transaction.
type txn struct {
	db   *DB
	snap *pager.Snapshot
	w    *pager.WriteTx
	used bool // a statement has read through the snapshot
}

func (t *txn) reader() btree.Reader {
	if t.w != nil {
		return t.w
	}
	return t.snap
}

// writer upgrades to a write transaction. If nothing has been read yet,
// the snapshot is simply replaced by a fresh write transaction, which
// cannot conflict.
func (t *txn) writer() (*pager.WriteTx, error) {
	if t.w != nil {
		return t.w, nil
	}
	if !t.used {
		t.snap.Release()
		t.snap = nil
		t.w = t.db.pager.BeginWrite()
		return t.w, nil
	}
	w, err := t.snap.Upgrade()
	t.snap = nil
	if err != nil {
		return nil, err
	}
	t.w = w
	return w, nil
}

func (t *txn) commit() error {
	if t.w != nil {
		return t.w.Commit()
	}
	if t.snap != nil {
		t.snap.Release()
	}
	return nil
}

func (t *txn) rollback() {
	if t.w != nil {
		t.w.Rollback()
	}
	if t.snap != nil {
		t.snap.Release()
	}
}

// Exec parses and runs one or more statements. It returns the results of
// the statements that ran; on error, the error describes the failing one.
func (s *Session) Exec(query string, params ...record.Value) ([]*Result, error) {
	stmts, ok := s.cache[query]
	if !ok {
		var err error
		if stmts, err = sql.Parse(query); err != nil {
			s.db.errors.Add(1)
			s.markFailed()
			return nil, AsError(err)
		}
		// Plans are not cached (they depend on the schema and parameter
		// values), but parsing is skipped for repeated query text.
		if s.cache == nil || len(s.cache) > 256 {
			s.cache = map[string][]sql.Stmt{}
		}
		s.cache[query] = stmts
	}
	var out []*Result
	for _, st := range stmts {
		r, err := s.Execute(st, params)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// MarkFailed puts an open explicit transaction into the failed state, as
// happens in PostgreSQL when any statement in it errors.
func (s *Session) MarkFailed() { s.markFailed() }

func (s *Session) markFailed() {
	if s.tx != nil {
		s.tx.rollback()
		s.tx = nil
		s.failed = true
	}
}

// Execute runs one parsed statement.
func (s *Session) Execute(st sql.Stmt, params []record.Value) (res *Result, err error) {
	s.db.queries.Add(1)
	defer func() {
		if err != nil {
			s.db.errors.Add(1)
			err = AsError(err)
		}
	}()
	switch st.(type) {
	case *sql.Begin:
		if s.failed {
			return nil, errf(CodeInFailedTxn, "current transaction is aborted, commands ignored until end of transaction block")
		}
		if s.tx == nil {
			s.tx = &txn{db: s.db, snap: s.db.pager.BeginRead()}
		}
		return &Result{Tag: "BEGIN"}, nil
	case *sql.Commit:
		if s.failed {
			s.failed = false
			return &Result{Tag: "ROLLBACK"}, nil
		}
		if s.tx == nil {
			return &Result{Tag: "COMMIT"}, nil
		}
		tx := s.tx
		s.tx = nil
		if err := tx.commit(); err != nil {
			return nil, err
		}
		return &Result{Tag: "COMMIT"}, nil
	case *sql.Rollback:
		s.failed = false
		if s.tx != nil {
			s.tx.rollback()
			s.tx = nil
		}
		return &Result{Tag: "ROLLBACK"}, nil
	}
	if s.failed {
		return nil, errf(CodeInFailedTxn, "current transaction is aborted, commands ignored until end of transaction block")
	}
	switch st := st.(type) {
	case *sql.Set:
		return &Result{Tag: "SET"}, nil
	case *sql.Show:
		return showSetting(st.Name), nil
	}

	write := isWrite(st)
	if s.tx != nil {
		// Inside BEGIN ... COMMIT.
		if write {
			if _, err := s.tx.writer(); err != nil {
				s.markFailed()
				return nil, err
			}
		}
		res, err := s.run(s.tx, st, params)
		s.tx.used = true
		if err != nil {
			s.markFailed()
			return nil, err
		}
		return res, nil
	}
	// Autocommit: one statement, one transaction.
	tx := &txn{db: s.db}
	if write {
		tx.w = s.db.pager.BeginWrite()
	} else {
		tx.snap = s.db.pager.BeginRead()
	}
	res, err = s.run(tx, st, params)
	if err != nil {
		tx.rollback()
		return nil, err
	}
	if err := tx.commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func isWrite(st sql.Stmt) bool {
	switch st.(type) {
	case *sql.Insert, *sql.Update, *sql.Delete, *sql.CreateTable, *sql.DropTable, *sql.CreateIndex, *sql.DropIndex:
		return true
	}
	return false
}

func showSetting(name string) *Result {
	settings := map[string]string{
		"server_version":              "16.0",
		"server_encoding":             "UTF8",
		"client_encoding":             "UTF8",
		"transaction_isolation":       "serializable",
		"transaction isolation level": "serializable",
		"standard_conforming_strings": "on",
		"datestyle":                   "ISO, MDY",
		"timezone":                    "UTC",
	}
	return &Result{
		Columns: []Column{{Name: name, Type: record.Text}},
		Rows:    []Row{{record.NewText(settings[strings.ToLower(name)])}},
		Tag:     "SHOW",
	}
}

func (s *Session) run(tx *txn, st sql.Stmt, params []record.Value) (*Result, error) {
	r := tx.reader()
	cat, err := s.db.catalogFor(r)
	if err != nil {
		return nil, err
	}
	p := &planner{cat: cat, params: params}
	if p.params == nil {
		p.params = []record.Value{}
	}
	ctx := &execCtx{r: r}
	switch st := st.(type) {
	case *sql.Select:
		plan, err := p.planSelect(st)
		if err != nil {
			return nil, err
		}
		rows, err := drain(ctx, plan.root, nil)
		if err != nil {
			return nil, err
		}
		return &Result{Columns: plan.cols, Rows: rows, Tag: "SELECT " + itoa(len(rows))}, nil
	case *sql.Explain:
		lines, err := s.explain(p, st.Stmt)
		if err != nil {
			return nil, err
		}
		res := &Result{Columns: []Column{{Name: "QUERY PLAN", Type: record.Text}}, Tag: "EXPLAIN"}
		for _, l := range lines {
			res.Rows = append(res.Rows, Row{record.NewText(l)})
		}
		return res, nil
	}
	w := tx.w
	if w == nil {
		return nil, errf(CodeInternal, "write statement without a write transaction")
	}
	switch st := st.(type) {
	case *sql.Insert:
		return p.insert(ctx, w, st)
	case *sql.Update:
		return p.update(ctx, w, st)
	case *sql.Delete:
		return p.delete(ctx, w, st)
	case *sql.CreateTable:
		return p.createTable(w, st)
	case *sql.DropTable:
		return p.dropTable(w, st)
	case *sql.CreateIndex:
		return p.createIndex(ctx, w, st)
	case *sql.DropIndex:
		return p.dropIndex(w, st)
	}
	return nil, errf(CodeFeatureUnsupported, "statement not supported")
}

func (s *Session) explain(p *planner, st sql.Stmt) ([]string, error) {
	switch st := st.(type) {
	case *sql.Select:
		plan, err := p.planSelect(st)
		if err != nil {
			return nil, err
		}
		return explainPlan(plan.root), nil
	case *sql.Update, *sql.Delete:
		var name string
		var where sql.Expr
		kind := "Update"
		if u, ok := st.(*sql.Update); ok {
			name, where = u.Table, u.Where
		} else {
			d := st.(*sql.Delete)
			name, where, kind = d.Table, d.Where, "Delete"
		}
		t, err := p.table(name)
		if err != nil {
			return nil, err
		}
		sc := &scope{cols: tableScope(t, t.Name)}
		ap, _, err := p.chooseAccess(fromTable{t: t, alias: t.Name}, sc, conjuncts(where))
		if err != nil {
			return nil, err
		}
		node, err := p.filter(ap, sc, conjuncts(where))
		if err != nil {
			return nil, err
		}
		return explainDML(kind, t, node), nil
	case *sql.Insert:
		return []string{"Insert on " + st.Table}, nil
	}
	return nil, errf(CodeFeatureUnsupported, "EXPLAIN is only supported for SELECT, INSERT, UPDATE and DELETE")
}

// Describe returns the result columns a statement will produce, without
// running it. Used by the wire protocol for prepared statements.
func (s *Session) Describe(st sql.Stmt) ([]Column, error) { return s.DescribeWith(st, nil) }

// DescribeWith is Describe with known parameter values (nil if unknown).
func (s *Session) DescribeWith(st sql.Stmt, params []record.Value) ([]Column, error) {
	switch st := st.(type) {
	case *sql.Select:
		tx := s.tx
		var snap *pager.Snapshot
		if tx == nil {
			snap = s.db.pager.BeginRead()
			defer snap.Release()
			tx = &txn{snap: snap}
		}
		cat, err := s.db.catalogFor(tx.reader())
		if err != nil {
			return nil, AsError(err)
		}
		plan, err := (&planner{cat: cat, params: params}).planSelect(st)
		if err != nil {
			return nil, AsError(err)
		}
		return plan.cols, nil
	case *sql.Explain:
		return []Column{{Name: "QUERY PLAN", Type: record.Text}}, nil
	case *sql.Show:
		return []Column{{Name: st.Name, Type: record.Text}}, nil
	}
	return nil, nil
}

// ParamCount returns the highest $n used in a statement.
func ParamCount(st sql.Stmt) int {
	n := 0
	visit := func(e sql.Expr) {
		mapExpr(e, func(x sql.Expr) sql.Expr {
			if p, ok := x.(*sql.Param); ok && p.Index > n {
				n = p.Index
			}
			return x
		})
	}
	var walk func(st sql.Stmt)
	walk = func(st sql.Stmt) {
		switch s := st.(type) {
		case *sql.Select:
			for _, it := range s.Items {
				visit(it.Expr)
			}
			for _, f := range s.From {
				visit(f.On)
			}
			visit(s.Where)
			for _, g := range s.GroupBy {
				visit(g)
			}
			visit(s.Having)
			for _, o := range s.OrderBy {
				visit(o.Expr)
			}
			visit(s.Limit)
			visit(s.Offset)
		case *sql.Insert:
			for _, r := range s.Rows {
				for _, e := range r {
					visit(e)
				}
			}
			if s.Select != nil {
				walk(s.Select)
			}
		case *sql.Update:
			for _, a := range s.Sets {
				visit(a.Value)
			}
			visit(s.Where)
		case *sql.Delete:
			visit(s.Where)
		case *sql.Explain:
			walk(s.Stmt)
		}
	}
	walk(st)
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

// TableInfo describes a table for tools like the shell's \d command.
type TableInfo struct {
	Name       string
	Columns    []ColumnInfo
	PrimaryKey string
	Indexes    []IndexInfo
}

// ColumnInfo describes one column.
type ColumnInfo struct {
	Name    string
	Type    record.Kind
	NotNull bool
}

// IndexInfo describes one index.
type IndexInfo struct {
	Name    string
	Columns []string
	Unique  bool
}

// Schema lists every table, sorted by name.
func (db *DB) Schema() ([]TableInfo, error) {
	snap := db.pager.BeginRead()
	defer snap.Release()
	cat, err := db.catalogFor(snap)
	if err != nil {
		return nil, err
	}
	var out []TableInfo
	for _, t := range cat.Tables {
		ti := TableInfo{Name: t.Name}
		for _, c := range t.Columns {
			ti.Columns = append(ti.Columns, ColumnInfo{Name: c.Name, Type: c.Type, NotNull: c.NotNull})
		}
		if t.PK >= 0 {
			ti.PrimaryKey = t.Columns[t.PK].Name
		}
		for _, ix := range t.Indexes {
			ii := IndexInfo{Name: ix.Name, Unique: ix.Unique}
			for _, c := range ix.Columns {
				ii.Columns = append(ii.Columns, t.Columns[c].Name)
			}
			ti.Indexes = append(ti.Indexes, ii)
		}
		out = append(out, ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Check walks every table and index and verifies the B-tree invariants and
// that every index matches its table exactly. It is what "oakdb check" and
// the crash tests use to prove the file is consistent.
func (db *DB) Check() error {
	snap := db.pager.BeginRead()
	defer snap.Release()
	cat, err := db.catalogFor(snap)
	if err != nil {
		return err
	}
	for _, t := range cat.Tables {
		st, err := btree.Inspect(snap, t.Root)
		if err != nil {
			return fmt.Errorf("table %s: %w", t.Name, err)
		}
		rows := map[string]Row{}
		cur := btree.NewCursor(snap, t.Root)
		for cur.First(); cur.Valid(); cur.Next() {
			v, err := cur.Value()
			if err != nil {
				return fmt.Errorf("table %s: %w", t.Name, err)
			}
			row, err := record.DecodeRow(v)
			if err != nil {
				return fmt.Errorf("table %s: %w", t.Name, err)
			}
			if len(row) != len(t.Columns) {
				return fmt.Errorf("table %s: row has %d values, want %d", t.Name, len(row), len(t.Columns))
			}
			rows[string(cur.Key())] = row
		}
		if err := cur.Err(); err != nil {
			return err
		}
		if len(rows) != st.Entries {
			return fmt.Errorf("table %s: cursor saw %d rows, tree has %d", t.Name, len(rows), st.Entries)
		}
		for _, ix := range t.Indexes {
			ist, err := btree.Inspect(snap, ix.Root)
			if err != nil {
				return fmt.Errorf("index %s: %w", ix.Name, err)
			}
			if ist.Entries != len(rows) {
				return fmt.Errorf("index %s has %d entries, table %s has %d rows", ix.Name, ist.Entries, t.Name, len(rows))
			}
			c := btree.NewCursor(snap, ix.Root)
			for c.First(); c.Valid(); c.Next() {
				k := c.Key()
				off, err := record.SkipKey(k, len(ix.Columns))
				if err != nil {
					return fmt.Errorf("index %s: %w", ix.Name, err)
				}
				row, ok := rows[string(k[off:])]
				if !ok {
					return fmt.Errorf("index %s points to a missing row", ix.Name)
				}
				if prefix, _ := indexPrefix(ix, row); !bytes.Equal(prefix, k[:off]) {
					return fmt.Errorf("index %s entry does not match its row", ix.Name)
				}
			}
		}
	}
	return nil
}
