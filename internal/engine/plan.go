package engine

import (
	"fmt"
	"strings"

	"github.com/ibiraza1077-pixel/oakdb/internal/catalog"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// The planner is rule-based rather than cost-based: it has no statistics,
// so it applies a fixed preference order that is right for the common cases.
//
//   - Access paths: primary key equality, then the index with the longest
//     run of equality columns, then IN lists, then key ranges, then a full
//     scan.
//   - Joins: an index nested-loop join when the inner table's key or
//     index can be probed with values from the outer row, else a hash join
//     for equality conditions, else a nested loop.
//   - Every WHERE condition is still checked by a filter after the access
//     path, so choosing a narrower path can never change results.

// Column describes one result column.
type Column struct {
	Name string
	Type record.Kind
}

type planner struct {
	cat    *catalog.Catalog
	params []record.Value
}

type fromTable struct {
	t      *catalog.Table
	alias  string
	offset int // position of its first column in the combined row
}

// conjuncts splits an expression on top-level ANDs.
func conjuncts(e sql.Expr) []sql.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*sql.Binary); ok && b.Op == "AND" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

func andAll(es []sql.Expr) sql.Expr {
	var out sql.Expr
	for _, e := range es {
		if out == nil {
			out = e
		} else {
			out = &sql.Binary{Op: "AND", L: out, R: e}
		}
	}
	return out
}

func tableScope(t *catalog.Table, alias string) []colInfo {
	cols := make([]colInfo, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = colInfo{table: alias, name: c.Name, typ: c.Type}
	}
	return cols
}

func (p *planner) table(name string) (*catalog.Table, error) {
	t, ok := p.cat.Tables[name]
	if !ok {
		return nil, errf(CodeUndefinedTable, "relation %q does not exist", name)
	}
	return t, nil
}

func (p *planner) compiler(sc *scope, clause string) *compiler {
	return &compiler{sc: sc, params: p.params, clause: clause}
}

// --- access path selection -------------------------------------------------

// chooseAccess picks how to read table ft given conditions conds, which are
// expressed over scope sc (where ft's columns start at ft.offset). Bound
// values may only use columns before ft.offset (the outer row in a join) or
// no columns at all. It reports whether the path depends on the outer row.
func (p *planner) chooseAccess(ft fromTable, sc *scope, conds []sql.Expr) (*accessPath, bool, error) {
	w := len(ft.t.Columns)
	inTable := func(i int) bool { return i >= ft.offset && i < ft.offset+w }
	outerOnly := func(e sql.Expr) bool { return refsOnly(e, sc, func(i int) bool { return i < ft.offset }) }
	usesOuter := func(e sql.Expr) bool { return !refsOnly(e, sc, func(int) bool { return false }) }
	boundScope := &scope{cols: sc.cols[:ft.offset]}
	bc := p.compiler(boundScope, "WHERE")

	// col returns the table column a bare column reference points to.
	col := func(e sql.Expr) (int, bool) {
		ref, ok := e.(*sql.ColumnRef)
		if !ok {
			return 0, false
		}
		i, err := sc.resolve(ref)
		if err != nil || !inTable(i) {
			return 0, false
		}
		return i - ft.offset, true
	}
	type bound struct {
		expr      sql.Expr
		inclusive bool
	}
	eqs := map[int]sql.Expr{}
	los := map[int]bound{}
	his := map[int]bound{}
	ins := map[int][]sql.Expr{}
	flip := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "=": "="}
	for _, c := range conds {
		switch x := c.(type) {
		case *sql.Binary:
			op, ok := flip[x.Op]
			if !ok {
				continue
			}
			colSide, other, opn := x.L, x.R, x.Op
			if _, ok := col(colSide); !ok || !outerOnly(other) {
				colSide, other, opn = x.R, x.L, op
				if _, ok := col(colSide); !ok || !outerOnly(other) {
					continue
				}
			}
			ci, _ := col(colSide)
			switch opn {
			case "=":
				if _, dup := eqs[ci]; !dup {
					eqs[ci] = other
				}
			case ">", ">=":
				if _, dup := los[ci]; !dup {
					los[ci] = bound{other, opn == ">="}
				}
			case "<", "<=":
				if _, dup := his[ci]; !dup {
					his[ci] = bound{other, opn == "<="}
				}
			}
		case *sql.Between:
			ci, ok := col(x.X)
			if !ok || x.Not || !outerOnly(x.Lo) || !outerOnly(x.Hi) {
				continue
			}
			if _, dup := los[ci]; !dup {
				los[ci] = bound{x.Lo, true}
			}
			if _, dup := his[ci]; !dup {
				his[ci] = bound{x.Hi, true}
			}
		case *sql.In:
			ci, ok := col(x.X)
			if !ok || x.Not {
				continue
			}
			allConst := true
			for _, e := range x.List {
				if usesOuter(e) {
					allConst = false
				}
			}
			if allConst {
				ins[ci] = x.List
			}
		}
	}
	ap := &accessPath{table: ft.t, alias: ft.alias}
	dependsOnOuter := false
	compileBound := func(e sql.Expr) (evalFn, error) {
		if usesOuter(e) {
			dependsOnOuter = true
		}
		f, _, err := bc.compile(e)
		return f, err
	}
	setEq := func(cols []int) error {
		for _, c := range cols {
			f, err := compileBound(eqs[c])
			if err != nil {
				return err
			}
			ap.eq = append(ap.eq, f)
			ap.eqText = append(ap.eqText, sql.Format(eqs[c]))
		}
		return nil
	}
	setRange := func(c int) error {
		if b, ok := los[c]; ok {
			f, err := compileBound(b.expr)
			if err != nil {
				return err
			}
			ap.lo = &keyBound{val: f, inclusive: b.inclusive, label: sql.Format(b.expr)}
		}
		if b, ok := his[c]; ok {
			f, err := compileBound(b.expr)
			if err != nil {
				return err
			}
			ap.hi = &keyBound{val: f, inclusive: b.inclusive, label: sql.Format(b.expr)}
		}
		return nil
	}
	setIn := func(c int) error {
		var texts []string
		for _, e := range ins[c] {
			f, err := compileBound(e)
			if err != nil {
				return err
			}
			ap.in = append(ap.in, f)
			texts = append(texts, sql.Format(e))
		}
		ap.inText = strings.Join(texts, ", ")
		return nil
	}
	pk := ft.t.PK

	// 1. Primary key equality: at most one row.
	if _, ok := eqs[pk]; pk >= 0 && ok {
		return ap, dependsOnOuter, setEq([]int{pk})
	}
	// 2. The index with the most leading equality columns.
	var best *catalog.Index
	bestN := 0
	for _, ix := range ft.t.Indexes {
		n := 0
		for _, c := range ix.Columns {
			if _, ok := eqs[c]; !ok {
				break
			}
			n++
		}
		if n > bestN || n == bestN && n > 0 && ix.Unique && !best.Unique {
			best, bestN = ix, n
		}
	}
	if best != nil {
		ap.index = best
		if err := setEq(best.Columns[:bestN]); err != nil {
			return nil, false, err
		}
		if bestN < len(best.Columns) {
			if err := setRange(best.Columns[bestN]); err != nil {
				return nil, false, err
			}
		}
		return ap, dependsOnOuter, nil
	}
	// 3. IN list on the primary key or an index's first column.
	if _, ok := ins[pk]; pk >= 0 && ok {
		return ap, dependsOnOuter, setIn(pk)
	}
	for _, ix := range ft.t.Indexes {
		if _, ok := ins[ix.Columns[0]]; ok {
			ap.index = ix
			return ap, dependsOnOuter, setIn(ix.Columns[0])
		}
	}
	// 4. Range on the primary key, then on an index's first column.
	if pk >= 0 {
		if _, lo := los[pk]; lo {
			return ap, dependsOnOuter, setRange(pk)
		}
		if _, hi := his[pk]; hi {
			return ap, dependsOnOuter, setRange(pk)
		}
	}
	for _, ix := range ft.t.Indexes {
		c := ix.Columns[0]
		_, lo := los[c]
		_, hi := his[c]
		if lo || hi {
			ap.index = ix
			return ap, dependsOnOuter, setRange(c)
		}
	}
	// 5. Full scan.
	return ap, false, nil
}

// --- SELECT ------------------------------------------------------------------

type selectPlan struct {
	root planNode
	cols []Column
}

func (p *planner) planSelect(s *sql.Select) (*selectPlan, error) {
	var tables []fromTable
	sc := &scope{}
	aliases := map[string]bool{}
	for _, fi := range s.From {
		t, err := p.table(fi.Table)
		if err != nil {
			return nil, err
		}
		alias := fi.Alias
		if alias == "" {
			alias = fi.Table
		}
		if aliases[alias] {
			return nil, errf("42712", "table name %q specified more than once", alias)
		}
		aliases[alias] = true
		tables = append(tables, fromTable{t: t, alias: alias, offset: len(sc.cols)})
		sc.cols = append(sc.cols, tableScope(t, alias)...)
	}

	where := conjuncts(s.Where)
	// Check WHERE compiles (and has no aggregates) before planning around it.
	var whereFn evalFn
	if s.Where != nil {
		f, _, err := p.compiler(sc, "WHERE").compile(s.Where)
		if err != nil {
			return nil, err
		}
		whereFn = f
	}

	var node planNode
	singleTablePK := false
	var firstOnly []sql.Expr
	if len(tables) == 0 {
		node = valuesNode{}
	} else {
		ft := tables[0]
		inFirst := func(i int) bool { return i < len(ft.t.Columns) }
		for _, c := range where {
			if refsOnly(c, sc, inFirst) {
				firstOnly = append(firstOnly, c)
			}
		}
		ap, _, err := p.chooseAccess(ft, &scope{cols: sc.cols[:len(ft.t.Columns)]}, firstOnly)
		if err != nil {
			return nil, err
		}
		if ap.index != nil && len(tables) == 1 {
			ap.indexOnly = coveredBy(ap, usedColumns(s, sc))
		}
		node = ap
		singleTablePK = ap.index == nil && ft.t.PK >= 0
		if len(firstOnly) > 0 {
			node, err = p.filter(node, &scope{cols: sc.cols[:len(ft.t.Columns)]}, firstOnly)
			if err != nil {
				return nil, err
			}
		}
		for i := 1; i < len(tables); i++ {
			if node, err = p.planJoin(node, tables[i], s.From[i], sc, where); err != nil {
				return nil, err
			}
		}
	}
	// Remaining WHERE conditions (those not fully applied to the first table).
	if whereFn != nil && len(firstOnly) < len(where) {
		var rest []sql.Expr
		for _, c := range where {
			if !containsExpr(firstOnly, c) {
				rest = append(rest, c)
			}
		}
		var err error
		if node, err = p.filter(node, sc, rest); err != nil {
			return nil, err
		}
	}

	// Expand * and t.* into column references.
	var items []sql.SelectItem
	for _, it := range s.Items {
		if !it.Star {
			items = append(items, it)
			continue
		}
		if len(tables) == 0 {
			return nil, errf(CodeSyntax, "SELECT * with no tables specified is not valid")
		}
		matched := false
		for _, ft := range tables {
			if it.StarTable != "" && ft.alias != it.StarTable {
				continue
			}
			matched = true
			for _, c := range ft.t.Columns {
				items = append(items, sql.SelectItem{Expr: &sql.ColumnRef{Table: ft.alias, Column: c.Name}, Alias: c.Name})
			}
		}
		if !matched {
			return nil, errf(CodeUndefinedTable, "missing FROM-clause entry for table %q", it.StarTable)
		}
	}

	// Aggregation.
	var aggCalls []*sql.Call
	for _, it := range items {
		collectAggs(it.Expr, &aggCalls)
	}
	collectAggs(s.Having, &aggCalls)
	for _, o := range s.OrderBy {
		collectAggs(o.Expr, &aggCalls)
	}
	groupExprs := make([]sql.Expr, len(s.GroupBy))
	for i, g := range s.GroupBy {
		groupExprs[i] = p.resolveOutputRef(g, items, sc)
	}
	post := p.compiler(sc, "SELECT")
	if len(aggCalls) > 0 || len(groupExprs) > 0 || s.Having != nil {
		base := p.compiler(sc, "GROUP BY")
		mode := &aggMode{groupKeys: map[string]int{}, aggKeys: map[string]int{}}
		var groupFns []evalFn
		var labels []string
		for _, g := range groupExprs {
			f, k, err := base.compile(g)
			if err != nil {
				return nil, err
			}
			key := base.fingerprint(g)
			if _, dup := mode.groupKeys[key]; dup {
				continue
			}
			mode.groupKeys[key] = len(groupFns)
			mode.groupTypes = append(mode.groupTypes, k)
			groupFns = append(groupFns, f)
			labels = append(labels, sql.Format(g))
		}
		argc := p.compiler(sc, "aggregate function calls")
		for _, call := range aggCalls {
			key := base.fingerprint(call)
			if _, dup := mode.aggKeys[key]; dup {
				continue
			}
			spec := &aggSpec{name: call.Name, distinct: call.Distinct, label: sql.Format(call)}
			if call.Star {
				if call.Name != "count" {
					return nil, errf(CodeSyntax, "%s(*) is not valid", call.Name)
				}
			} else {
				if len(call.Args) != 1 {
					return nil, errf(CodeUndefinedFunction, "%s() takes exactly one argument", call.Name)
				}
				f, k, err := argc.compile(call.Args[0])
				if err != nil {
					return nil, err
				}
				spec.arg = f
				spec.typ = k
			}
			switch call.Name {
			case "count":
				spec.typ = record.Int
			case "avg", "total":
				spec.typ = record.Real
			}
			mode.aggKeys[key] = len(mode.aggs)
			mode.aggs = append(mode.aggs, spec)
			labels = append(labels, spec.label)
		}
		node = &aggregateNode{child: node, groups: groupFns, aggs: mode.aggs, text: strings.Join(labels, ", ")}
		post = &compiler{sc: sc, agg: mode, params: p.params, clause: "SELECT"}
		if s.Having != nil {
			hc := *post
			hc.clause = "HAVING"
			f, _, err := hc.compile(s.Having)
			if err != nil {
				return nil, err
			}
			node = &filterNode{child: node, pred: f, text: sql.Format(s.Having)}
		}
		singleTablePK = false
	}

	// Projection, plus hidden columns for ORDER BY expressions.
	var exprs []evalFn
	var cols []Column
	var names []string
	for _, it := range items {
		f, k, err := post.compile(it.Expr)
		if err != nil {
			return nil, err
		}
		if k == record.Null {
			k = record.Text
		}
		exprs = append(exprs, f)
		cols = append(cols, Column{Name: columnName(it), Type: k})
		names = append(names, sql.Format(it.Expr))
	}
	nOut := len(exprs)
	var keys []sortKey
	var sortText []string
	for _, o := range s.OrderBy {
		col := -1
		if lit, ok := o.Expr.(*sql.Literal); ok && lit.Val.K == record.Int {
			if lit.Val.I < 1 || lit.Val.I > int64(nOut) {
				return nil, errf(CodeSyntax, "ORDER BY position %d is not in select list", lit.Val.I)
			}
			col = int(lit.Val.I - 1)
		} else if ref, ok := o.Expr.(*sql.ColumnRef); ok && ref.Table == "" {
			for i, c := range cols {
				if c.Name == ref.Column {
					if _, err := sc.resolve(ref); err != nil || isAlias(items[i]) {
						col = i
						break
					}
				}
			}
		}
		if col < 0 {
			oc := *post
			oc.clause = "ORDER BY"
			f, _, err := oc.compile(o.Expr)
			if err != nil {
				return nil, err
			}
			// Reuse an identical output column if there is one.
			fp := oc.fingerprint(o.Expr)
			for i, it := range items {
				if oc.fingerprint(it.Expr) == fp {
					col = i
					break
				}
			}
			if col < 0 {
				if s.Distinct {
					return nil, errf("42P10", "for SELECT DISTINCT, ORDER BY expressions must appear in select list")
				}
				col = len(exprs)
				exprs = append(exprs, f)
				names = append(names, sql.Format(o.Expr))
			}
		}
		keys = append(keys, sortKey{col: col, desc: o.Desc})
		dir := ""
		if o.Desc {
			dir = " DESC"
		}
		sortText = append(sortText, sql.Format(o.Expr)+dir)
	}
	node = &projectNode{child: node, exprs: exprs, names: names}
	if s.Distinct {
		node = &distinctNode{child: node, n: nOut}
	}
	if len(keys) > 0 && !p.alreadyOrdered(s, singleTablePK, tables, sc) {
		node = &sortNode{child: node, keys: keys, text: strings.Join(sortText, ", ")}
	}
	if s.Limit != nil || s.Offset != nil {
		limit, err := p.constInt(s.Limit, "LIMIT", -1)
		if err != nil {
			return nil, err
		}
		offset, err := p.constInt(s.Offset, "OFFSET", 0)
		if err != nil {
			return nil, err
		}
		node = &limitNode{child: node, limit: limit, offset: offset}
	}
	if len(exprs) > nOut {
		node = &trimNode{child: node, n: nOut}
	}
	return &selectPlan{root: node, cols: cols}, nil
}

// usedColumns returns the positions of every column a single-table SELECT
// reads, or nil if it reads them all (SELECT *).
func usedColumns(s *sql.Select, sc *scope) map[int]bool {
	used := map[int]bool{}
	all := false
	mark := func(e sql.Expr) {
		mapExpr(e, func(x sql.Expr) sql.Expr {
			if ref, ok := x.(*sql.ColumnRef); ok {
				// Names that do not resolve are output aliases
				// (ORDER BY total), which need no table column.
				if i, err := sc.resolve(ref); err == nil {
					used[i] = true
				}
			}
			return x
		})
	}
	for _, it := range s.Items {
		if it.Star {
			all = true
		}
		mark(it.Expr)
	}
	mark(s.Where)
	for _, g := range s.GroupBy {
		mark(g)
	}
	mark(s.Having)
	for _, o := range s.OrderBy {
		mark(o.Expr)
	}
	if all {
		return nil
	}
	return used
}

// coveredBy reports whether an index holds every column in used. The
// primary key is always available because it is the suffix of every entry.
func coveredBy(ap *accessPath, used map[int]bool) bool {
	if used == nil {
		return false
	}
	have := map[int]bool{}
	for _, c := range ap.index.Columns {
		have[c] = true
	}
	if ap.table.PK >= 0 {
		have[ap.table.PK] = true
	}
	for c := range used {
		if !have[c] {
			return false
		}
	}
	return true
}

// alreadyOrdered reports whether a single-table plan reads rows in the
// requested order already: ORDER BY the primary key ascending, read
// through the table's own B-tree. Then no sort is needed, and with a LIMIT
// only the first rows are ever read.
func (p *planner) alreadyOrdered(s *sql.Select, singleTablePK bool, tables []fromTable, sc *scope) bool {
	if !singleTablePK || len(tables) != 1 || len(s.OrderBy) != 1 || s.OrderBy[0].Desc || s.Distinct {
		return false
	}
	ref, ok := s.OrderBy[0].Expr.(*sql.ColumnRef)
	if !ok {
		return false
	}
	i, err := sc.resolve(ref)
	return err == nil && i == tables[0].t.PK
}

func isAlias(it sql.SelectItem) bool {
	if it.Alias == "" {
		return false
	}
	ref, ok := it.Expr.(*sql.ColumnRef)
	return !ok || ref.Column != it.Alias
}

// resolveOutputRef lets GROUP BY use select-list positions (GROUP BY 1) and
// aliases, as PostgreSQL does.
func (p *planner) resolveOutputRef(e sql.Expr, items []sql.SelectItem, sc *scope) sql.Expr {
	if lit, ok := e.(*sql.Literal); ok && lit.Val.K == record.Int && lit.Val.I >= 1 && int(lit.Val.I) <= len(items) {
		return items[lit.Val.I-1].Expr
	}
	if ref, ok := e.(*sql.ColumnRef); ok && ref.Table == "" {
		if _, err := sc.resolve(ref); err != nil {
			for _, it := range items {
				if it.Alias == ref.Column {
					return it.Expr
				}
			}
		}
	}
	return e
}

func columnName(it sql.SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	e := it.Expr
	for {
		if c, ok := e.(*sql.Cast); ok {
			e = c.X
			continue
		}
		break
	}
	switch x := e.(type) {
	case *sql.ColumnRef:
		return x.Column
	case *sql.Call:
		return x.Name
	case *sql.Case:
		return "case"
	}
	return "?column?"
}

func containsExpr(list []sql.Expr, e sql.Expr) bool {
	for _, x := range list {
		if x == e {
			return true
		}
	}
	return false
}

func (p *planner) filter(node planNode, sc *scope, conds []sql.Expr) (planNode, error) {
	if len(conds) == 0 {
		return node, nil
	}
	e := andAll(conds)
	f, _, err := p.compiler(sc, "WHERE").compile(e)
	if err != nil {
		return nil, err
	}
	return &filterNode{child: node, pred: f, text: sql.Format(e)}, nil
}

// constInt evaluates LIMIT/OFFSET. When describing a prepared statement
// without parameter values, it returns def.
func (p *planner) constInt(e sql.Expr, clause string, def int64) (int64, error) {
	if e == nil {
		return def, nil
	}
	f, _, err := p.compiler(&scope{}, clause).compile(e)
	if err != nil {
		return 0, err
	}
	if p.params == nil && containsParam(e) {
		return def, nil
	}
	v, err := f(nil)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		return def, nil
	}
	n, err := record.Coerce(v, record.Int)
	if err != nil || n.I < 0 {
		return 0, errf(CodeInvalidParameter, "%s must be a non-negative integer", clause)
	}
	return n.I, nil
}

func containsParam(e sql.Expr) bool {
	found := false
	mapExpr(e, func(x sql.Expr) sql.Expr {
		if _, ok := x.(*sql.Param); ok {
			found = true
		}
		return x
	})
	return found
}

// planJoin adds table ft to the plan as the right side of a join.
func (p *planner) planJoin(left planNode, ft fromTable, fi sql.FromItem, sc *scope, where []sql.Expr) (planNode, error) {
	upto := ft.offset + len(ft.t.Columns)
	joinScope := &scope{cols: sc.cols[:upto]}
	leftScope := &scope{cols: sc.cols[:ft.offset]}
	rightScope := &scope{cols: tableScope(ft.t, ft.alias)}
	leftJoin := fi.Join == "LEFT"

	on := conjuncts(fi.On)
	// Conditions usable when choosing how to join: the ON clause, plus (for
	// inner joins) WHERE conditions that only mention tables joined so far.
	cands := append([]sql.Expr{}, on...)
	if !leftJoin {
		for _, c := range where {
			if refsOnly(c, sc, func(i int) bool { return i < upto }) && !refsOnly(c, sc, func(i int) bool { return i < ft.offset }) {
				cands = append(cands, c)
			}
		}
	}
	for _, c := range on {
		if _, _, err := p.compiler(joinScope, "JOIN conditions").compile(c); err != nil {
			return nil, err
		}
	}
	inRight := func(i int) bool { return i >= ft.offset && i < upto }
	var rightOnly []sql.Expr
	for _, c := range cands {
		if refsOnly(c, sc, inRight) {
			rightOnly = append(rightOnly, c)
		}
	}
	// Conditions on the right table alone, rewritten for the right scope.
	rightFilter := func(n planNode) (planNode, error) { return p.filter(n, rightScope, rightOnly) }

	var cond evalFn
	var condText string
	if len(cands) > 0 {
		e := andAll(cands)
		f, _, err := p.compiler(joinScope, "JOIN conditions").compile(e)
		if err != nil {
			return nil, err
		}
		cond, condText = f, sql.Format(e)
	}

	// 1. Index nested loop: probe the right table's key with outer values.
	ap, perRow, err := p.chooseAccess(ft, joinScope, cands)
	if err != nil {
		return nil, err
	}
	if perRow {
		right, err := rightFilter(ap)
		if err != nil {
			return nil, err
		}
		return &nestedLoopJoin{left: left, right: right, cond: cond, condText: condText, leftJoin: leftJoin, perRow: true}, nil
	}

	// 2. Hash join on equality conditions between the two sides.
	var lkeys, rkeys []evalFn
	var texts []string
	leftOnly := func(e sql.Expr) bool {
		return refsOnly(e, sc, func(i int) bool { return i < ft.offset }) && !refsOnly(e, sc, func(int) bool { return false })
	}
	rightSide := func(e sql.Expr) bool {
		return refsOnly(e, sc, inRight) && !refsOnly(e, sc, func(int) bool { return false })
	}
	for _, c := range cands {
		b, ok := c.(*sql.Binary)
		if !ok || b.Op != "=" {
			continue
		}
		l, r := b.L, b.R
		if rightSide(l) && leftOnly(r) {
			l, r = r, l
		}
		if !leftOnly(l) || !rightSide(r) {
			continue
		}
		lf, _, err := p.compiler(leftScope, "JOIN conditions").compile(l)
		if err != nil {
			return nil, err
		}
		rf, _, err := p.compiler(rightScope, "JOIN conditions").compile(r)
		if err != nil {
			return nil, err
		}
		lkeys, rkeys = append(lkeys, lf), append(rkeys, rf)
		texts = append(texts, sql.Format(c))
	}
	right, err := rightFilter(ap)
	if err != nil {
		return nil, err
	}
	if len(lkeys) > 0 {
		return &hashJoin{left: left, right: right, leftKeys: lkeys, rightKeys: rkeys, cond: cond,
			text: strings.Join(texts, " AND "), leftJoin: leftJoin}, nil
	}
	// 3. Nested loop.
	return &nestedLoopJoin{left: left, right: right, cond: cond, condText: condText, leftJoin: leftJoin}, nil
}

// --- helpers for DML ---------------------------------------------------------

// targets returns the rows of t matching where, with their table keys.
// They are collected before any change is made, so an UPDATE never sees
// its own writes (the "Halloween problem").
func (p *planner) targets(ctx *execCtx, t *catalog.Table, where sql.Expr) ([][]byte, []Row, error) {
	sc := &scope{cols: tableScope(t, t.Name)}
	conds := conjuncts(where)
	ap, _, err := p.chooseAccess(fromTable{t: t, alias: t.Name}, sc, conds)
	if err != nil {
		return nil, nil, err
	}
	var pred evalFn
	if where != nil {
		if pred, _, err = p.compiler(sc, "WHERE").compile(where); err != nil {
			return nil, nil, err
		}
	}
	it, err := ap.openKeyed(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	var keys [][]byte
	var rows []Row
	for {
		k, row, err := it.nextKeyed()
		if err != nil {
			return nil, nil, err
		}
		if row == nil {
			return keys, rows, nil
		}
		if pred != nil {
			ok, err := passes(pred, row)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
		}
		keys = append(keys, k)
		rows = append(rows, row)
	}
}

func explainDML(kind string, t *catalog.Table, ap planNode) []string {
	lines := []string{fmt.Sprintf("%s on %s", kind, t.Name)}
	for _, l := range explainPlan(ap) {
		lines = append(lines, "  ->  "+l)
	}
	return lines
}
