package engine

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/ibiraza1077-pixel/oakdb/internal/btree"
	"github.com/ibiraza1077-pixel/oakdb/internal/catalog"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// The executor uses the classic "Volcano" iterator model: every operator
// has Next(), which pulls rows from its children one at a time. Operators
// that must see all their input first (sort, aggregate, hash-join build)
// materialise it; everything else streams, so LIMIT stops work early.

type iterator interface {
	next() (Row, error) // nil row means the end
}

// execCtx is what operators need at run time.
type execCtx struct {
	r btree.Reader
}

// planNode is one operator in a query plan.
type planNode interface {
	open(ctx *execCtx, outer Row) (iterator, error)
	explain() (label string, kids []planNode)
	width() int
}

// --- table access ----------------------------------------------------------

// keyBound is one end of a key range. The key is computed per execution
// (it may depend on parameters or, in an index join, on the outer row).
type keyBound struct {
	val       evalFn
	inclusive bool
	label     string
}

// accessPath reads rows of one table: a full scan, a primary key lookup or
// range, or an index lookup or range. It yields table keys too, which
// UPDATE and DELETE need.
type accessPath struct {
	table *catalog.Table
	alias string
	index *catalog.Index // nil: read the table's own B-tree

	eq     []evalFn // equality values for the leading key columns
	eqText []string
	lo, hi *keyBound // optional range on the next key column
	in     []evalFn  // IN-list point lookups on the first key column
	inText string
}

func (a *accessPath) width() int { return len(a.table.Columns) }

func (a *accessPath) keyKinds() []record.Kind {
	if a.index == nil {
		if a.table.PK >= 0 {
			return []record.Kind{a.table.Columns[a.table.PK].Type}
		}
		return []record.Kind{record.Int}
	}
	kinds := make([]record.Kind, len(a.index.Columns))
	for i, c := range a.index.Columns {
		kinds[i] = a.table.Columns[c].Type
	}
	return kinds
}

func (a *accessPath) explain() (string, []planNode) {
	name := a.table.Name
	if a.alias != "" && a.alias != a.table.Name {
		name += " " + a.alias
	}
	var conds []string
	cols := a.keyColumnNames()
	for i, t := range a.eqText {
		conds = append(conds, fmt.Sprintf("%s = %s", cols[i], t))
	}
	if a.inText != "" {
		conds = append(conds, fmt.Sprintf("%s IN (%s)", cols[0], a.inText))
	}
	rc := cols[min(len(a.eq), len(cols)-1)]
	if a.lo != nil {
		op := ">"
		if a.lo.inclusive {
			op = ">="
		}
		conds = append(conds, fmt.Sprintf("%s %s %s", rc, op, a.lo.label))
	}
	if a.hi != nil {
		op := "<"
		if a.hi.inclusive {
			op = "<="
		}
		conds = append(conds, fmt.Sprintf("%s %s %s", rc, op, a.hi.label))
	}
	cond := ""
	if len(conds) > 0 {
		cond = " (" + strings.Join(conds, " AND ") + ")"
	}
	switch {
	case a.index != nil:
		return fmt.Sprintf("Index Scan using %s on %s%s", a.index.Name, name, cond), nil
	case len(conds) > 0:
		return fmt.Sprintf("Primary Key Lookup on %s%s", name, cond), nil
	}
	return "Seq Scan on " + name, nil
}

func (a *accessPath) keyColumnNames() []string {
	if a.index != nil {
		out := make([]string, len(a.index.Columns))
		for i, c := range a.index.Columns {
			out[i] = a.table.Columns[c].Name
		}
		return out
	}
	if a.table.PK >= 0 {
		return []string{a.table.Columns[a.table.PK].Name}
	}
	return []string{"rowid"}
}

func (a *accessPath) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := a.openKeyed(ctx, outer)
	if err != nil {
		return nil, err
	}
	return dropKeys{it}, nil
}

type dropKeys struct{ it *accessIter }

func (d dropKeys) next() (Row, error) {
	_, row, err := d.it.nextKeyed()
	return row, err
}

// keyRange is a byte range [start, end) over a B-tree, plus a prefix every
// key in the range must start with.
type keyRange struct {
	start   []byte
	prefix  []byte
	stop    []byte // keys >= stop end the range (nil: no limit)
	stopInc []byte // keys > stopInc and not prefixed by it end the range
	skip    []byte // keys prefixed by this are skipped (exclusive lower bound)
}

type accessIter struct {
	a      *accessPath
	ctx    *execCtx
	ranges []keyRange
	cur    *btree.Cursor
	rng    int
	tree   uint32
	empty  bool
}

func (a *accessPath) openKeyed(ctx *execCtx, outer Row) (*accessIter, error) {
	it := &accessIter{a: a, ctx: ctx, rng: -1, tree: a.table.Root}
	if a.index != nil {
		it.tree = a.index.Root
	}
	kinds := a.keyKinds()
	// coerce turns a bound value into key bytes for the key column. Numbers
	// share one key encoding, so 2.5 is a valid bound on an integer column.
	// If the value cannot be compared in key order (for example a number
	// against a text column), ok is false and the caller widens the scan;
	// the filter above still checks every row, so results stay correct.
	coerce := func(f evalFn, col int) (key []byte, ok, null bool, err error) {
		v, err := f(outer)
		if err != nil {
			return nil, false, false, err
		}
		if v.IsNull() {
			return nil, false, true, nil
		}
		if col < len(kinds) {
			switch kinds[col] {
			case record.Int, record.Real:
				if v.K == record.Text {
					n, err := toNumber(v)
					if err != nil {
						return nil, false, false, nil
					}
					v = n
				} else if !v.IsNumeric() {
					return nil, false, false, nil
				}
			case record.Text:
				if v.K != record.Text {
					return nil, false, false, nil
				}
			case record.Bool:
				b, err := record.Coerce(v, record.Bool)
				if err != nil {
					return nil, false, false, nil
				}
				v = b
			}
		}
		return record.AppendKey(nil, v), true, false, nil
	}
	var prefix []byte
	for i, f := range a.eq {
		k, ok, null, err := coerce(f, i)
		if err != nil {
			return nil, err
		}
		if null { // "col = NULL" matches nothing
			it.empty = true
			return it, nil
		}
		if !ok {
			it.ranges = []keyRange{{}}
			return it, nil
		}
		prefix = append(prefix, k...)
	}
	if len(a.in) > 0 {
		seen := map[string]bool{}
		var keys [][]byte
		for _, f := range a.in {
			k, ok, null, err := coerce(f, 0)
			if err != nil {
				return nil, err
			}
			if null {
				continue
			}
			if !ok {
				it.ranges = []keyRange{{}}
				return it, nil
			}
			if !seen[string(k)] {
				seen[string(k)] = true
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
		for _, k := range keys {
			it.ranges = append(it.ranges, keyRange{start: k, prefix: k})
		}
		return it, nil
	}
	r := keyRange{start: prefix, prefix: prefix}
	col := len(a.eq)
	if a.lo != nil {
		k, ok, null, err := coerce(a.lo.val, col)
		if err != nil {
			return nil, err
		}
		if null {
			it.empty = true
			return it, nil
		}
		if ok {
			r.start = append(bytes.Clone(prefix), k...)
			if !a.lo.inclusive {
				r.skip = r.start
			}
		}
	}
	if a.hi != nil {
		k, ok, null, err := coerce(a.hi.val, col)
		if err != nil {
			return nil, err
		}
		if null {
			it.empty = true
			return it, nil
		}
		if ok {
			bound := append(bytes.Clone(prefix), k...)
			if a.hi.inclusive {
				r.stopInc = bound
			} else {
				r.stop = bound
			}
		}
	}
	it.ranges = []keyRange{r}
	return it, nil
}

// nextKeyed returns the next row with its table key.
func (it *accessIter) nextKeyed() ([]byte, Row, error) {
	if it.empty {
		return nil, nil, nil
	}
	for {
		if it.cur == nil || !it.cur.Valid() {
			if err := it.curErr(); err != nil {
				return nil, nil, err
			}
			it.rng++
			if it.rng >= len(it.ranges) {
				return nil, nil, nil
			}
			it.cur = btree.NewCursor(it.ctx.r, it.tree)
			it.cur.Seek(it.ranges[it.rng].start)
			continue
		}
		r := it.ranges[it.rng]
		k := it.cur.Key()
		if r.prefix != nil && !bytes.HasPrefix(k, r.prefix) ||
			r.stop != nil && bytes.Compare(k, r.stop) >= 0 ||
			r.stopInc != nil && bytes.Compare(k, r.stopInc) > 0 && !bytes.HasPrefix(k, r.stopInc) {
			it.cur = nil // move on to the next range
			continue
		}
		if r.skip != nil && bytes.HasPrefix(k, r.skip) {
			it.cur.Next()
			continue
		}
		key := bytes.Clone(k)
		val, err := it.cur.Value()
		if err != nil {
			return nil, nil, err
		}
		it.cur.Next()
		if it.a.index != nil {
			// Index entry: the table key follows the indexed values.
			off, err := record.SkipKey(key, len(it.a.index.Columns))
			if err != nil {
				return nil, nil, err
			}
			tkey := key[off:]
			val, ok, err := btree.Get(it.ctx.r, it.a.table.Root, tkey)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				return nil, nil, errf(CodeInternal, "index %s points to a missing row", it.a.index.Name)
			}
			row, err := record.DecodeRow(val)
			return tkey, row, err
		}
		row, err := record.DecodeRow(val)
		return key, row, err
	}
}

func (it *accessIter) curErr() error {
	if it.cur != nil {
		return it.cur.Err()
	}
	return nil
}

// --- simple operators ------------------------------------------------------

// valuesNode produces a single empty row, for SELECT without FROM.
type valuesNode struct{}

func (valuesNode) width() int                           { return 0 }
func (valuesNode) explain() (string, []planNode)        { return "Result", nil }
func (valuesNode) open(*execCtx, Row) (iterator, error) { return &sliceIter{rows: []Row{{}}}, nil }

type sliceIter struct {
	rows []Row
	i    int
}

func (s *sliceIter) next() (Row, error) {
	if s.i >= len(s.rows) {
		return nil, nil
	}
	s.i++
	return s.rows[s.i-1], nil
}

type filterNode struct {
	child planNode
	pred  evalFn
	text  string
}

func (f *filterNode) width() int { return f.child.width() }
func (f *filterNode) explain() (string, []planNode) {
	return "Filter: " + f.text, []planNode{f.child}
}
func (f *filterNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := f.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &filterIter{it, f.pred}, nil
}

type filterIter struct {
	it   iterator
	pred evalFn
}

func (f *filterIter) next() (Row, error) {
	for {
		row, err := f.it.next()
		if row == nil || err != nil {
			return nil, err
		}
		ok, err := passes(f.pred, row)
		if err != nil {
			return nil, err
		}
		if ok {
			return row, nil
		}
	}
}

func passes(pred evalFn, row Row) (bool, error) {
	v, err := pred(row)
	if err != nil {
		return false, err
	}
	t, null, err := truth(v)
	return t && !null, err
}

type projectNode struct {
	child planNode
	exprs []evalFn
	names []string
}

func (p *projectNode) width() int { return len(p.exprs) }
func (p *projectNode) explain() (string, []planNode) {
	return "Project (" + strings.Join(p.names, ", ") + ")", []planNode{p.child}
}
func (p *projectNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := p.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &projectIter{it, p.exprs}, nil
}

type projectIter struct {
	it    iterator
	exprs []evalFn
}

func (p *projectIter) next() (Row, error) {
	row, err := p.it.next()
	if row == nil || err != nil {
		return nil, err
	}
	out := make(Row, len(p.exprs))
	for i, f := range p.exprs {
		if out[i], err = f(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type limitNode struct {
	child         planNode
	limit, offset int64 // limit < 0 means none
}

func (l *limitNode) width() int { return l.child.width() }
func (l *limitNode) explain() (string, []planNode) {
	parts := []string{}
	if l.limit >= 0 {
		parts = append(parts, fmt.Sprintf("limit %d", l.limit))
	}
	if l.offset > 0 {
		parts = append(parts, fmt.Sprintf("offset %d", l.offset))
	}
	return "Limit (" + strings.Join(parts, ", ") + ")", []planNode{l.child}
}
func (l *limitNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := l.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &limitIter{it: it, limit: l.limit, skip: l.offset}, nil
}

type limitIter struct {
	it          iterator
	limit, skip int64
	n           int64
}

func (l *limitIter) next() (Row, error) {
	for l.skip > 0 {
		row, err := l.it.next()
		if row == nil || err != nil {
			return nil, err
		}
		l.skip--
	}
	if l.limit >= 0 && l.n >= l.limit {
		return nil, nil
	}
	row, err := l.it.next()
	if row != nil {
		l.n++
	}
	return row, err
}

// trimNode drops trailing hidden columns (extra sort keys).
type trimNode struct {
	child planNode
	n     int
}

func (t *trimNode) width() int                    { return t.n }
func (t *trimNode) explain() (string, []planNode) { return t.child.explain() }
func (t *trimNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := t.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &trimIter{it, t.n}, nil
}

type trimIter struct {
	it iterator
	n  int
}

func (t *trimIter) next() (Row, error) {
	row, err := t.it.next()
	if row == nil || err != nil {
		return nil, err
	}
	return row[:t.n], nil
}

// --- sort and distinct -----------------------------------------------------

type sortKey struct {
	col  int
	desc bool
}

type sortNode struct {
	child planNode
	keys  []sortKey
	text  string
}

func (s *sortNode) width() int { return s.child.width() }
func (s *sortNode) explain() (string, []planNode) {
	return "Sort (" + s.text + ")", []planNode{s.child}
}
func (s *sortNode) open(ctx *execCtx, outer Row) (iterator, error) {
	rows, err := drain(ctx, s.child, outer)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range s.keys {
			c := record.Compare(rows[i][k.col], rows[j][k.col])
			if c == 0 {
				continue
			}
			// NULLs sort first ascending and last descending, like SQLite.
			if k.desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	return &sliceIter{rows: rows}, nil
}

func drain(ctx *execCtx, n planNode, outer Row) ([]Row, error) {
	it, err := n.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	var rows []Row
	for {
		row, err := it.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			return rows, nil
		}
		rows = append(rows, row)
	}
}

type distinctNode struct {
	child planNode
	n     int // compare the first n columns
}

func (d *distinctNode) width() int                    { return d.child.width() }
func (d *distinctNode) explain() (string, []planNode) { return "Distinct", []planNode{d.child} }
func (d *distinctNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := d.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &distinctIter{it: it, n: d.n, seen: map[string]bool{}}, nil
}

type distinctIter struct {
	it   iterator
	n    int
	seen map[string]bool
}

func (d *distinctIter) next() (Row, error) {
	for {
		row, err := d.it.next()
		if row == nil || err != nil {
			return nil, err
		}
		k := string(groupKey(row[:d.n]))
		if !d.seen[k] {
			d.seen[k] = true
			return row, nil
		}
	}
}

// groupKey encodes values so equal values (including 1 = 1.0) give equal keys.
func groupKey(vals []record.Value) []byte {
	var b []byte
	for _, v := range vals {
		b = record.AppendKey(b, v)
	}
	return b
}

// --- joins -----------------------------------------------------------------

// nestedLoopJoin pairs every left row with every right row that satisfies
// cond. If the right side is an access path that depends on the left row
// (an index join), it is re-opened per left row; otherwise it is read once.
type nestedLoopJoin struct {
	left, right planNode
	cond        evalFn // on the combined row; nil means true
	condText    string
	leftJoin    bool
	perRow      bool // right side uses the left row (index lookup)
}

func (j *nestedLoopJoin) width() int { return j.left.width() + j.right.width() }
func (j *nestedLoopJoin) explain() (string, []planNode) {
	kind := "Nested Loop"
	if j.perRow {
		kind = "Index Nested Loop"
	}
	if j.leftJoin {
		kind += " Left Join"
	} else {
		kind += " Join"
	}
	if j.condText != "" {
		kind += " (" + j.condText + ")"
	}
	return kind, []planNode{j.left, j.right}
}

func (j *nestedLoopJoin) open(ctx *execCtx, outer Row) (iterator, error) {
	left, err := j.left.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	it := &nlIter{j: j, ctx: ctx, left: left}
	if !j.perRow {
		if it.rightRows, err = drain(ctx, j.right, outer); err != nil {
			return nil, err
		}
	}
	return it, nil
}

type nlIter struct {
	j         *nestedLoopJoin
	ctx       *execCtx
	left      iterator
	lrow      Row
	matched   bool
	rightRows []Row
	ri        int
	rightIt   iterator
}

func (it *nlIter) nextRight() (Row, error) {
	if it.j.perRow {
		return it.rightIt.next()
	}
	if it.ri >= len(it.rightRows) {
		return nil, nil
	}
	it.ri++
	return it.rightRows[it.ri-1], nil
}

func (it *nlIter) next() (Row, error) {
	for {
		if it.lrow == nil {
			row, err := it.left.next()
			if row == nil || err != nil {
				return nil, err
			}
			it.lrow, it.matched, it.ri = row, false, 0
			if it.j.perRow {
				if it.rightIt, err = it.j.right.open(it.ctx, row); err != nil {
					return nil, err
				}
			}
		}
		r, err := it.nextRight()
		if err != nil {
			return nil, err
		}
		if r == nil {
			l := it.lrow
			it.lrow = nil
			if it.j.leftJoin && !it.matched {
				return append(append(Row{}, l...), make(Row, it.j.right.width())...), nil
			}
			continue
		}
		combined := append(append(make(Row, 0, len(it.lrow)+len(r)), it.lrow...), r...)
		if it.j.cond != nil {
			ok, err := passes(it.j.cond, combined)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		it.matched = true
		return combined, nil
	}
}

// hashJoin builds a hash table on the right input keyed by the join columns,
// then streams the left input and probes it. O(n + m) instead of O(n * m).
type hashJoin struct {
	left, right         planNode
	leftKeys, rightKeys []evalFn // leftKeys see left rows; rightKeys see right rows
	cond                evalFn   // remaining condition on the combined row
	text                string
	leftJoin            bool
}

func (h *hashJoin) width() int { return h.left.width() + h.right.width() }
func (h *hashJoin) explain() (string, []planNode) {
	kind := "Hash Join"
	if h.leftJoin {
		kind = "Hash Left Join"
	}
	return kind + " (" + h.text + ")", []planNode{h.left, h.right}
}

func (h *hashJoin) open(ctx *execCtx, outer Row) (iterator, error) {
	rows, err := drain(ctx, h.right, outer)
	if err != nil {
		return nil, err
	}
	table := map[string][]Row{}
	for _, r := range rows {
		k, null, err := evalKey(h.rightKeys, r)
		if err != nil {
			return nil, err
		}
		if !null { // NULL never equals anything
			table[string(k)] = append(table[string(k)], r)
		}
	}
	left, err := h.left.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	return &hashIter{h: h, left: left, table: table}, nil
}

func evalKey(fns []evalFn, row Row) ([]byte, bool, error) {
	var b []byte
	for _, f := range fns {
		v, err := f(row)
		if err != nil {
			return nil, false, err
		}
		if v.IsNull() {
			return nil, true, nil
		}
		b = record.AppendKey(b, v)
	}
	return b, false, nil
}

type hashIter struct {
	h       *hashJoin
	table   map[string][]Row
	left    iterator
	lrow    Row
	matches []Row
	mi      int
	matched bool
}

func (it *hashIter) next() (Row, error) {
	for {
		if it.lrow == nil {
			row, err := it.left.next()
			if row == nil || err != nil {
				return nil, err
			}
			it.lrow, it.mi, it.matched = row, 0, false
			k, null, err := evalKey(it.h.leftKeys, row)
			if err != nil {
				return nil, err
			}
			it.matches = nil
			if !null {
				it.matches = it.table[string(k)]
			}
		}
		if it.mi >= len(it.matches) {
			l := it.lrow
			it.lrow = nil
			if it.h.leftJoin && !it.matched {
				return append(append(Row{}, l...), make(Row, it.h.right.width())...), nil
			}
			continue
		}
		r := it.matches[it.mi]
		it.mi++
		combined := append(append(make(Row, 0, len(it.lrow)+len(r)), it.lrow...), r...)
		if it.h.cond != nil {
			ok, err := passes(it.h.cond, combined)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		it.matched = true
		return combined, nil
	}
}

// --- aggregation -------------------------------------------------------------

type aggregateNode struct {
	child  planNode
	groups []evalFn
	aggs   []*aggSpec
	text   string
}

func (a *aggregateNode) width() int { return len(a.groups) + len(a.aggs) }
func (a *aggregateNode) explain() (string, []planNode) {
	label := "Aggregate"
	if len(a.groups) > 0 {
		label = "Hash Aggregate"
	}
	return label + " (" + a.text + ")", []planNode{a.child}
}

type aggState struct {
	count    int64
	sumI     int64
	sumF     float64
	isReal   bool
	overflow bool
	best     record.Value
	seen     map[string]bool
}

func (a *aggregateNode) open(ctx *execCtx, outer Row) (iterator, error) {
	it, err := a.child.open(ctx, outer)
	if err != nil {
		return nil, err
	}
	type group struct {
		keys   Row
		states []*aggState
	}
	groups := map[string]*group{}
	var order []string
	newGroup := func(keys Row) *group {
		g := &group{keys: keys, states: make([]*aggState, len(a.aggs))}
		for i, spec := range a.aggs {
			g.states[i] = &aggState{}
			if spec.distinct {
				g.states[i].seen = map[string]bool{}
			}
		}
		return g
	}
	for {
		row, err := it.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		keys := make(Row, len(a.groups))
		for i, f := range a.groups {
			if keys[i], err = f(row); err != nil {
				return nil, err
			}
		}
		k := string(groupKey(keys))
		g, ok := groups[k]
		if !ok {
			g = newGroup(keys)
			groups[k] = g
			order = append(order, k)
		}
		for i, spec := range a.aggs {
			if err := accumulate(spec, g.states[i], row); err != nil {
				return nil, err
			}
		}
	}
	// An aggregate with no GROUP BY returns one row even for empty input.
	if len(a.groups) == 0 && len(order) == 0 {
		groups[""] = newGroup(Row{})
		order = append(order, "")
	}
	out := make([]Row, 0, len(order))
	for _, k := range order {
		g := groups[k]
		row := append(Row{}, g.keys...)
		for i, spec := range a.aggs {
			v, err := finish(spec, g.states[i])
			if err != nil {
				return nil, err
			}
			row = append(row, v)
		}
		out = append(out, row)
	}
	return &sliceIter{rows: out}, nil
}

func accumulate(spec *aggSpec, st *aggState, row Row) error {
	if spec.arg == nil { // COUNT(*)
		st.count++
		return nil
	}
	v, err := spec.arg(row)
	if err != nil {
		return err
	}
	if v.IsNull() {
		return nil
	}
	if st.seen != nil {
		k := string(record.AppendKey(nil, v))
		if st.seen[k] {
			return nil
		}
		st.seen[k] = true
	}
	st.count++
	switch spec.name {
	case "sum", "avg", "total":
		n, err := toNumber(v)
		if err != nil {
			return err
		}
		if n.K == record.Real {
			if !st.isReal {
				st.isReal = true
				st.sumF = float64(st.sumI)
			}
			st.sumF += n.F
		} else if st.isReal {
			st.sumF += float64(n.I)
		} else {
			s := st.sumI + n.I
			if (s > st.sumI) != (n.I > 0) {
				st.overflow = true
			}
			st.sumI = s
			st.sumF += float64(n.I)
		}
	case "min", "max":
		if st.count == 1 {
			st.best = v
			return nil
		}
		c, err := compareValues(v, st.best)
		if err != nil {
			return err
		}
		if spec.name == "min" && c < 0 || spec.name == "max" && c > 0 {
			st.best = v
		}
	}
	return nil
}

func finish(spec *aggSpec, st *aggState) (record.Value, error) {
	switch spec.name {
	case "count":
		return record.NewInt(st.count), nil
	case "sum":
		if st.count == 0 {
			return record.Value{}, nil
		}
		if st.isReal {
			return record.NewReal(st.sumF), nil
		}
		if st.overflow {
			return record.Value{}, errf(CodeNumericRange, "integer out of range in SUM")
		}
		return record.NewInt(st.sumI), nil
	case "total":
		if st.isReal {
			return record.NewReal(st.sumF), nil
		}
		return record.NewReal(float64(st.sumI)), nil
	case "avg":
		if st.count == 0 {
			return record.Value{}, nil
		}
		if st.isReal {
			return record.NewReal(st.sumF / float64(st.count)), nil
		}
		return record.NewReal(float64(st.sumI) / float64(st.count)), nil
	case "min", "max":
		if st.count == 0 {
			return record.Value{}, nil
		}
		return st.best, nil
	}
	return record.Value{}, errf(CodeUndefinedFunction, "unknown aggregate %s", spec.name)
}

// explainPlan renders a plan as indented text, PostgreSQL style.
func explainPlan(n planNode) []string {
	var lines []string
	var walk func(n planNode, depth int)
	walk = func(n planNode, depth int) {
		label, kids := n.explain()
		if depth == 0 {
			lines = append(lines, label)
		} else {
			lines = append(lines, strings.Repeat("  ", depth-1)+"->  "+label)
		}
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(n, 0)
	return lines
}
