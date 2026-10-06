package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/ibiraza1077-pixel/oakdb/internal/btree"
	"github.com/ibiraza1077-pixel/oakdb/internal/catalog"
	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// --- DDL ---------------------------------------------------------------------

func (p *planner) nameTaken(name string) bool {
	_, t := p.cat.Tables[name]
	_, i := p.cat.Indexes[name]
	return t || i
}

func (p *planner) createTable(w *pager.WriteTx, ct *sql.CreateTable) (*Result, error) {
	res := &Result{Tag: "CREATE TABLE"}
	if _, exists := p.cat.Tables[ct.Name]; exists {
		if ct.IfNotExists {
			return res, nil
		}
		return nil, errf(CodeDuplicateTable, "relation %q already exists", ct.Name)
	}
	if p.nameTaken(ct.Name) {
		return nil, errf(CodeDuplicateTable, "relation %q already exists", ct.Name)
	}
	if len(ct.Columns) == 0 {
		return nil, errf(CodeSyntax, "a table needs at least one column")
	}
	t := &catalog.Table{Name: ct.Name, PK: -1}
	seen := map[string]bool{}
	for i, c := range ct.Columns {
		if seen[c.Name] {
			return nil, errf(CodeDuplicateColumn, "column %q specified more than once", c.Name)
		}
		seen[c.Name] = true
		if c.PrimaryKey {
			if t.PK >= 0 {
				return nil, errf("42P16", "multiple primary keys for table %q are not allowed", ct.Name)
			}
			t.PK = i
		}
		t.Columns = append(t.Columns, catalog.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull || c.PrimaryKey})
	}
	root, err := btree.Create(w)
	if err != nil {
		return nil, err
	}
	t.Root = root
	if err := catalog.PutTable(w, t); err != nil {
		return nil, err
	}
	for i, c := range ct.Columns {
		if c.Unique && !c.PrimaryKey {
			name := fmt.Sprintf("%s_%s_key", ct.Name, c.Name)
			r, err := btree.Create(w)
			if err != nil {
				return nil, err
			}
			if err := catalog.PutIndex(w, &catalog.Index{Name: name, Table: ct.Name, Columns: []int{i}, Unique: true, Root: r}); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (p *planner) dropTable(w *pager.WriteTx, dt *sql.DropTable) (*Result, error) {
	t, ok := p.cat.Tables[dt.Name]
	if !ok {
		if dt.IfExists {
			return &Result{Tag: "DROP TABLE"}, nil
		}
		return nil, errf(CodeUndefinedTable, "table %q does not exist", dt.Name)
	}
	for _, ix := range t.Indexes {
		if err := btree.Drop(w, ix.Root); err != nil {
			return nil, err
		}
		if err := catalog.DeleteIndex(w, ix.Name); err != nil {
			return nil, err
		}
	}
	if err := btree.Drop(w, t.Root); err != nil {
		return nil, err
	}
	if err := catalog.DeleteTable(w, t.Name); err != nil {
		return nil, err
	}
	return &Result{Tag: "DROP TABLE"}, nil
}

func (p *planner) createIndex(ctx *execCtx, w *pager.WriteTx, ci *sql.CreateIndex) (*Result, error) {
	res := &Result{Tag: "CREATE INDEX"}
	if p.nameTaken(ci.Name) {
		if ci.IfNotExists {
			return res, nil
		}
		return nil, errf(CodeDuplicateTable, "relation %q already exists", ci.Name)
	}
	t, err := p.table(ci.Table)
	if err != nil {
		return nil, err
	}
	ix := &catalog.Index{Name: ci.Name, Table: t.Name, Unique: ci.Unique}
	for _, name := range ci.Columns {
		c := t.ColumnIndex(name)
		if c < 0 {
			return nil, errf(CodeUndefinedColumn, "column %q does not exist", name)
		}
		ix.Columns = append(ix.Columns, c)
	}
	if ix.Root, err = btree.Create(w); err != nil {
		return nil, err
	}
	// Backfill from the existing rows.
	cur := btree.NewCursor(w, t.Root)
	var keys, vals [][]byte
	for cur.First(); cur.Valid(); cur.Next() {
		v, err := cur.Value()
		if err != nil {
			return nil, err
		}
		keys = append(keys, bytes.Clone(cur.Key()))
		vals = append(vals, bytes.Clone(v))
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	for i := range keys {
		row, err := record.DecodeRow(vals[i])
		if err != nil {
			return nil, err
		}
		if err := insertIndexEntry(w, ix, row, keys[i]); err != nil {
			if e := AsError(err); e.Code == CodeUniqueViolation {
				return nil, errf(CodeUniqueViolation, "could not create unique index %q: %s", ix.Name, e.Msg)
			}
			return nil, err
		}
	}
	if err := catalog.PutIndex(w, ix); err != nil {
		return nil, err
	}
	return res, nil
}

func (p *planner) dropIndex(w *pager.WriteTx, di *sql.DropIndex) (*Result, error) {
	ix, ok := p.cat.Indexes[di.Name]
	if !ok {
		if di.IfExists {
			return &Result{Tag: "DROP INDEX"}, nil
		}
		return nil, errf(CodeUndefinedObject, "index %q does not exist", di.Name)
	}
	if err := btree.Drop(w, ix.Root); err != nil {
		return nil, err
	}
	if err := catalog.DeleteIndex(w, ix.Name); err != nil {
		return nil, err
	}
	return &Result{Tag: "DROP INDEX"}, nil
}

// --- row and index maintenance -----------------------------------------------

func indexPrefix(ix *catalog.Index, row Row) ([]byte, bool) {
	var b []byte
	hasNull := false
	for _, c := range ix.Columns {
		if row[c].IsNull() {
			hasNull = true
		}
		b = record.AppendKey(b, row[c])
	}
	return b, hasNull
}

func insertIndexEntry(w *pager.WriteTx, ix *catalog.Index, row Row, tkey []byte) error {
	prefix, hasNull := indexPrefix(ix, row)
	// In a unique index, NULLs never conflict (as in PostgreSQL).
	if ix.Unique && !hasNull {
		cur := btree.NewCursor(w, ix.Root)
		cur.Seek(prefix)
		if cur.Valid() && bytes.HasPrefix(cur.Key(), prefix) {
			return errf(CodeUniqueViolation, "duplicate key value violates unique constraint %q", ix.Name)
		}
		if err := cur.Err(); err != nil {
			return err
		}
	}
	key := append(prefix, tkey...)
	if err := btree.Insert(w, ix.Root, key, nil, false); err != nil {
		if errors.Is(err, btree.ErrKeyTooLarge) {
			return errf("54000", "index row too large for index %q", ix.Name)
		}
		return err
	}
	return nil
}

func deleteIndexEntry(w *pager.WriteTx, ix *catalog.Index, row Row, tkey []byte) error {
	prefix, _ := indexPrefix(ix, row)
	_, err := btree.Delete(w, ix.Root, append(prefix, tkey...))
	return err
}

// checkRow coerces values to the column types and enforces NOT NULL.
func checkRow(t *catalog.Table, row Row) error {
	for i, c := range t.Columns {
		v, err := record.Coerce(row[i], c.Type)
		if err != nil {
			return errf(CodeDatatypeMismatch, "column %q: %v", c.Name, err)
		}
		row[i] = v
		if v.IsNull() && c.NotNull && !(i == t.PK && c.Type == record.Int) {
			return errf(CodeNotNullViolation, "null value in column %q of relation %q violates not-null constraint", c.Name, t.Name)
		}
	}
	return nil
}

// nextRowID returns one more than the largest integer key in the table.
func nextRowID(w *pager.WriteTx, t *catalog.Table) (int64, error) {
	cur := btree.NewCursor(w, t.Root)
	cur.Last()
	if err := cur.Err(); err != nil {
		return 0, err
	}
	if !cur.Valid() {
		return 1, nil
	}
	vals, err := record.DecodeKey(cur.Key(), []record.Kind{record.Int})
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 || vals[0].K != record.Int || vals[0].I < 0 {
		return 1, nil
	}
	if vals[0].I == math.MaxInt64 {
		return 0, errf(CodeNumericRange, "table %q has run out of row ids", t.Name)
	}
	return vals[0].I + 1, nil
}

func (p *planner) tableKey(w *pager.WriteTx, t *catalog.Table, row Row) ([]byte, error) {
	if t.PK < 0 {
		id, err := nextRowID(w, t)
		if err != nil {
			return nil, err
		}
		return record.EncodeKey(record.NewInt(id)), nil
	}
	if row[t.PK].IsNull() {
		// A NULL integer primary key is filled in automatically, like
		// SERIAL in PostgreSQL or INTEGER PRIMARY KEY in SQLite.
		id, err := nextRowID(w, t)
		if err != nil {
			return nil, err
		}
		row[t.PK] = record.NewInt(id)
	}
	return record.EncodeKey(row[t.PK]), nil
}

func pkName(t *catalog.Table) string { return t.Name + "_pkey" }

func insertRow(w *pager.WriteTx, t *catalog.Table, key []byte, row Row) error {
	if err := btree.Insert(w, t.Root, key, record.EncodeRow(nil, row), false); err != nil {
		switch {
		case errors.Is(err, btree.ErrExists):
			return errf(CodeUniqueViolation, "duplicate key value violates unique constraint %q", pkName(t))
		case errors.Is(err, btree.ErrKeyTooLarge):
			return errf("54000", "primary key value too long in table %q", t.Name)
		}
		return err
	}
	for _, ix := range t.Indexes {
		if err := insertIndexEntry(w, ix, row, key); err != nil {
			return err
		}
	}
	return nil
}

// --- INSERT, UPDATE, DELETE ----------------------------------------------------

func (p *planner) insert(ctx *execCtx, w *pager.WriteTx, ins *sql.Insert) (*Result, error) {
	t, err := p.table(ins.Table)
	if err != nil {
		return nil, err
	}
	targets := make([]int, 0, len(t.Columns))
	if len(ins.Columns) == 0 {
		for i := range t.Columns {
			targets = append(targets, i)
		}
	} else {
		seen := map[int]bool{}
		for _, name := range ins.Columns {
			c := t.ColumnIndex(name)
			if c < 0 {
				return nil, errf(CodeUndefinedColumn, "column %q of relation %q does not exist", name, t.Name)
			}
			if seen[c] {
				return nil, errf(CodeDuplicateColumn, "column %q specified more than once", name)
			}
			seen[c] = true
			targets = append(targets, c)
		}
	}
	var source []Row
	if ins.Select != nil {
		plan, err := p.planSelect(ins.Select)
		if err != nil {
			return nil, err
		}
		if len(plan.cols) != len(targets) {
			return nil, errf(CodeSyntax, "INSERT has %d target columns but the query returns %d", len(targets), len(plan.cols))
		}
		if source, err = drain(ctx, plan.root, nil); err != nil {
			return nil, err
		}
	} else {
		c := p.compiler(&scope{}, "VALUES")
		for _, exprs := range ins.Rows {
			if len(exprs) > len(targets) {
				return nil, errf(CodeSyntax, "INSERT has more expressions than target columns")
			}
			row := make(Row, len(exprs))
			for i, e := range exprs {
				f, _, err := c.compile(e)
				if err != nil {
					return nil, err
				}
				if row[i], err = f(nil); err != nil {
					return nil, err
				}
			}
			source = append(source, row)
		}
	}
	for _, src := range source {
		row := make(Row, len(t.Columns))
		for i, v := range src {
			row[targets[i]] = v
		}
		if err := checkRow(t, row); err != nil {
			return nil, err
		}
		key, err := p.tableKey(w, t, row)
		if err != nil {
			return nil, err
		}
		if err := insertRow(w, t, key, row); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: "INSERT 0 " + itoa(len(source))}, nil
}

func (p *planner) update(ctx *execCtx, w *pager.WriteTx, up *sql.Update) (*Result, error) {
	t, err := p.table(up.Table)
	if err != nil {
		return nil, err
	}
	sc := &scope{cols: tableScope(t, t.Name)}
	c := p.compiler(sc, "UPDATE")
	type set struct {
		col int
		f   evalFn
	}
	var sets []set
	for _, a := range up.Sets {
		col := t.ColumnIndex(a.Column)
		if col < 0 {
			return nil, errf(CodeUndefinedColumn, "column %q of relation %q does not exist", a.Column, t.Name)
		}
		f, _, err := c.compile(a.Value)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set{col, f})
	}
	keys, rows, err := p.targets(ctx, t, up.Where)
	if err != nil {
		return nil, err
	}
	for i, old := range rows {
		row := append(Row{}, old...)
		for _, s := range sets {
			// Every SET expression sees the row as it was before the update.
			if row[s.col], err = s.f(old); err != nil {
				return nil, err
			}
		}
		if err := checkRow(t, row); err != nil {
			return nil, err
		}
		if t.PK >= 0 && row[t.PK].IsNull() {
			return nil, errf(CodeNotNullViolation, "null value in column %q of relation %q violates not-null constraint", t.Columns[t.PK].Name, t.Name)
		}
		for _, ix := range t.Indexes {
			if err := deleteIndexEntry(w, ix, old, keys[i]); err != nil {
				return nil, err
			}
		}
		newKey := keys[i]
		if t.PK >= 0 {
			newKey = record.EncodeKey(row[t.PK])
		}
		if !bytes.Equal(newKey, keys[i]) {
			if _, err := btree.Delete(w, t.Root, keys[i]); err != nil {
				return nil, err
			}
			if err := insertRow(w, t, newKey, row); err != nil {
				return nil, err
			}
			continue
		}
		if err := btree.Insert(w, t.Root, newKey, record.EncodeRow(nil, row), true); err != nil {
			return nil, err
		}
		for _, ix := range t.Indexes {
			if err := insertIndexEntry(w, ix, row, newKey); err != nil {
				return nil, err
			}
		}
	}
	return &Result{Tag: "UPDATE " + itoa(len(rows))}, nil
}

func (p *planner) delete(ctx *execCtx, w *pager.WriteTx, del *sql.Delete) (*Result, error) {
	t, err := p.table(del.Table)
	if err != nil {
		return nil, err
	}
	keys, rows, err := p.targets(ctx, t, del.Where)
	if err != nil {
		return nil, err
	}
	for i, row := range rows {
		for _, ix := range t.Indexes {
			if err := deleteIndexEntry(w, ix, row, keys[i]); err != nil {
				return nil, err
			}
		}
		if _, err := btree.Delete(w, t.Root, keys[i]); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: "DELETE " + itoa(len(rows))}, nil
}
