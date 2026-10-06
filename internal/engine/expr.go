package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"unicode/utf8"

	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// Row is one tuple flowing through a query plan.
type Row = []record.Value

// evalFn is a compiled expression. Compiling the AST once into closures
// means the per-row work is just function calls, with no tree walking or
// name lookups.
type evalFn func(Row) (record.Value, error)

// colInfo describes one column of a row flowing through the plan.
type colInfo struct {
	table string // table alias (or name) the column came from
	name  string
	typ   record.Kind
}

type scope struct{ cols []colInfo }

func (s *scope) resolve(ref *sql.ColumnRef) (int, error) {
	found := -1
	for i, c := range s.cols {
		if c.name != ref.Column || ref.Table != "" && c.table != ref.Table {
			continue
		}
		if found >= 0 {
			return 0, errf(CodeAmbiguousColumn, "column reference %q is ambiguous", ref.Column)
		}
		found = i
	}
	if found < 0 {
		if ref.Table != "" {
			for _, c := range s.cols {
				if c.table == ref.Table {
					return 0, errf(CodeUndefinedColumn, "column %s.%s does not exist", ref.Table, ref.Column)
				}
			}
			return 0, errf(CodeUndefinedTable, "missing FROM-clause entry for table %q", ref.Table)
		}
		return 0, errf(CodeUndefinedColumn, "column %q does not exist", ref.Column)
	}
	return found, nil
}

// aggSpec is one aggregate call, e.g. SUM(price), computed per group.
type aggSpec struct {
	name     string
	arg      evalFn // nil for COUNT(*)
	distinct bool
	typ      record.Kind
	label    string
}

// aggMode maps expressions to their slot in a post-aggregation row, which
// is laid out as [group values..., aggregate values...].
type aggMode struct {
	groupKeys  map[string]int
	groupTypes []record.Kind
	aggKeys    map[string]int
	aggs       []*aggSpec
}

type compiler struct {
	sc     *scope
	agg    *aggMode
	params []record.Value
	// clause names the clause being compiled, for error messages.
	clause string
}

var aggregateFuncs = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true, "total": true}

func isAggCall(e sql.Expr) bool {
	c, ok := e.(*sql.Call)
	return ok && aggregateFuncs[c.Name]
}

// children returns the direct sub-expressions of e.
func children(e sql.Expr) []sql.Expr {
	switch x := e.(type) {
	case *sql.Unary:
		return []sql.Expr{x.X}
	case *sql.Binary:
		return []sql.Expr{x.L, x.R}
	case *sql.IsNull:
		return []sql.Expr{x.X}
	case *sql.Like:
		return []sql.Expr{x.X, x.Pattern}
	case *sql.In:
		return append([]sql.Expr{x.X}, x.List...)
	case *sql.Between:
		return []sql.Expr{x.X, x.Lo, x.Hi}
	case *sql.Call:
		return x.Args
	case *sql.Case:
		var out []sql.Expr
		if x.Operand != nil {
			out = append(out, x.Operand)
		}
		for _, w := range x.Whens {
			out = append(out, w.Cond, w.Result)
		}
		if x.Else != nil {
			out = append(out, x.Else)
		}
		return out
	case *sql.Cast:
		return []sql.Expr{x.X}
	}
	return nil
}

// collectAggs appends every outermost aggregate call inside e.
func collectAggs(e sql.Expr, out *[]*sql.Call) {
	if e == nil {
		return
	}
	if isAggCall(e) {
		*out = append(*out, e.(*sql.Call))
		return
	}
	for _, c := range children(e) {
		collectAggs(c, out)
	}
}

// mapExpr rebuilds e bottom-up, replacing each node with f(node).
func mapExpr(e sql.Expr, f func(sql.Expr) sql.Expr) sql.Expr {
	if e == nil {
		return nil
	}
	m := func(x sql.Expr) sql.Expr { return mapExpr(x, f) }
	switch x := e.(type) {
	case *sql.Unary:
		c := *x
		c.X = m(x.X)
		return f(&c)
	case *sql.Binary:
		c := *x
		c.L, c.R = m(x.L), m(x.R)
		return f(&c)
	case *sql.IsNull:
		c := *x
		c.X = m(x.X)
		return f(&c)
	case *sql.Like:
		c := *x
		c.X, c.Pattern = m(x.X), m(x.Pattern)
		return f(&c)
	case *sql.In:
		c := *x
		c.X = m(x.X)
		c.List = make([]sql.Expr, len(x.List))
		for i, a := range x.List {
			c.List[i] = m(a)
		}
		return f(&c)
	case *sql.Between:
		c := *x
		c.X, c.Lo, c.Hi = m(x.X), m(x.Lo), m(x.Hi)
		return f(&c)
	case *sql.Call:
		c := *x
		c.Args = make([]sql.Expr, len(x.Args))
		for i, a := range x.Args {
			c.Args[i] = m(a)
		}
		return f(&c)
	case *sql.Case:
		c := *x
		c.Operand, c.Else = m(x.Operand), m(x.Else)
		c.Whens = make([]sql.When, len(x.Whens))
		for i, w := range x.Whens {
			c.Whens[i] = sql.When{Cond: m(w.Cond), Result: m(w.Result)}
		}
		return f(&c)
	case *sql.Cast:
		c := *x
		c.X = m(x.X)
		return f(&c)
	}
	return f(e)
}

// fingerprint is a canonical string for e with column references resolved
// to positions, so "t.a + 1" and "a + 1" match when they mean the same thing.
func (c *compiler) fingerprint(e sql.Expr) string {
	resolved := mapExpr(e, func(x sql.Expr) sql.Expr {
		if ref, ok := x.(*sql.ColumnRef); ok {
			if i, err := c.sc.resolve(ref); err == nil {
				return &sql.ColumnRef{Column: fmt.Sprintf("#%d", i)}
			}
		}
		return x
	})
	return sql.Format(resolved)
}

// refsOnly reports whether every column e references resolves to a column
// position accepted by ok. Expressions referencing unknown columns fail.
func refsOnly(e sql.Expr, sc *scope, ok func(int) bool) bool {
	good := true
	mapExpr(e, func(x sql.Expr) sql.Expr {
		if ref, isRef := x.(*sql.ColumnRef); isRef {
			i, err := sc.resolve(ref)
			if err != nil || !ok(i) {
				good = false
			}
		}
		return x
	})
	return good
}

func constant(v record.Value) evalFn { return func(Row) (record.Value, error) { return v, nil } }

func (c *compiler) compile(e sql.Expr) (evalFn, record.Kind, error) {
	if c.agg != nil {
		key := c.fingerprint(e)
		if i, ok := c.agg.groupKeys[key]; ok {
			return func(r Row) (record.Value, error) { return r[i], nil }, c.agg.groupTypes[i], nil
		}
		if isAggCall(e) {
			j, ok := c.agg.aggKeys[key]
			if !ok {
				return nil, 0, errf(CodeInternal, "aggregate %s was not planned", sql.Format(e))
			}
			slot := len(c.agg.groupTypes) + j
			return func(r Row) (record.Value, error) { return r[slot], nil }, c.agg.aggs[j].typ, nil
		}
		if ref, ok := e.(*sql.ColumnRef); ok {
			return nil, 0, errf(CodeGrouping, "column %q must appear in the GROUP BY clause or be used in an aggregate function", sql.Format(ref))
		}
	}
	switch x := e.(type) {
	case *sql.Literal:
		return constant(x.Val), x.Val.K, nil
	case *sql.Param:
		if c.params == nil {
			// Planning without values (to describe a prepared statement).
			return func(Row) (record.Value, error) {
				return record.Value{}, errf(CodeInvalidParameter, "no value supplied for parameter $%d", x.Index)
			}, record.Null, nil
		}
		if x.Index > len(c.params) {
			return nil, 0, errf(CodeInvalidParameter, "there is no parameter $%d", x.Index)
		}
		v := c.params[x.Index-1]
		return constant(v), v.K, nil
	case *sql.ColumnRef:
		i, err := c.sc.resolve(x)
		if err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) { return r[i], nil }, c.sc.cols[i].typ, nil
	case *sql.Unary:
		f, k, err := c.compile(x.X)
		if err != nil {
			return nil, 0, err
		}
		if x.Op == "NOT" {
			return func(r Row) (record.Value, error) {
				v, err := f(r)
				if err != nil {
					return v, err
				}
				t, null, err := truth(v)
				if null || err != nil {
					return record.Value{}, err
				}
				return record.NewBool(!t), nil
			}, record.Bool, nil
		}
		return func(r Row) (record.Value, error) {
			v, err := f(r)
			if err != nil || v.IsNull() {
				return v, err
			}
			return arith("-", record.NewInt(0), v)
		}, k, nil
	case *sql.Binary:
		return c.binary(x)
	case *sql.IsNull:
		f, _, err := c.compile(x.X)
		if err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) {
			v, err := f(r)
			if err != nil {
				return v, err
			}
			return record.NewBool(v.IsNull() != x.Not), nil
		}, record.Bool, nil
	case *sql.Like:
		f, _, err := c.compile(x.X)
		if err != nil {
			return nil, 0, err
		}
		pf, _, err := c.compile(x.Pattern)
		if err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) {
			v, err := f(r)
			if err != nil {
				return v, err
			}
			p, err := pf(r)
			if err != nil {
				return p, err
			}
			if v.IsNull() || p.IsNull() {
				return record.Value{}, nil
			}
			s, pat := v.String(), p.String()
			if x.Insensitive {
				s, pat = strings.ToLower(s), strings.ToLower(pat)
			}
			return record.NewBool(like(s, pat) != x.Not), nil
		}, record.Bool, nil
	case *sql.In:
		f, _, err := c.compile(x.X)
		if err != nil {
			return nil, 0, err
		}
		list := make([]evalFn, len(x.List))
		for i, a := range x.List {
			if list[i], _, err = c.compile(a); err != nil {
				return nil, 0, err
			}
		}
		return func(r Row) (record.Value, error) {
			v, err := f(r)
			if err != nil || v.IsNull() {
				return record.Value{}, err
			}
			sawNull := false
			for _, lf := range list {
				w, err := lf(r)
				if err != nil {
					return w, err
				}
				if w.IsNull() {
					sawNull = true
					continue
				}
				cmp, err := compareValues(v, w)
				if err != nil {
					return record.Value{}, err
				}
				if cmp == 0 {
					return record.NewBool(!x.Not), nil
				}
			}
			if sawNull {
				return record.Value{}, nil
			}
			return record.NewBool(x.Not), nil
		}, record.Bool, nil
	case *sql.Between:
		var b sql.Expr = &sql.Binary{Op: "AND",
			L: &sql.Binary{Op: ">=", L: x.X, R: x.Lo},
			R: &sql.Binary{Op: "<=", L: x.X, R: x.Hi}}
		if x.Not {
			b = &sql.Unary{Op: "NOT", X: b}
		}
		return c.compile(b)
	case *sql.Case:
		return c.caseExpr(x)
	case *sql.Cast:
		f, _, err := c.compile(x.X)
		if err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) {
			v, err := f(r)
			if err != nil {
				return v, err
			}
			return castValue(v, x.Type)
		}, x.Type, nil
	case *sql.Call:
		if aggregateFuncs[x.Name] {
			where := c.clause
			if where == "" {
				where = "this context"
			}
			return nil, 0, errf(CodeGrouping, "aggregate functions are not allowed in %s", where)
		}
		return c.call(x)
	}
	return nil, 0, errf(CodeFeatureUnsupported, "unsupported expression %T", e)
}

func (c *compiler) binary(x *sql.Binary) (evalFn, record.Kind, error) {
	lf, lk, err := c.compile(x.L)
	if err != nil {
		return nil, 0, err
	}
	rf, rk, err := c.compile(x.R)
	if err != nil {
		return nil, 0, err
	}
	switch x.Op {
	case "AND", "OR":
		isAnd := x.Op == "AND"
		return func(r Row) (record.Value, error) {
			lv, err := lf(r)
			if err != nil {
				return lv, err
			}
			lt, lnull, err := truth(lv)
			if err != nil {
				return record.Value{}, err
			}
			// Short-circuit: FALSE AND x = FALSE, TRUE OR x = TRUE.
			if !lnull && lt != isAnd {
				return record.NewBool(lt), nil
			}
			rv, err := rf(r)
			if err != nil {
				return rv, err
			}
			rt, rnull, err := truth(rv)
			if err != nil {
				return record.Value{}, err
			}
			if !rnull && rt != isAnd {
				return record.NewBool(rt), nil
			}
			if lnull || rnull {
				return record.Value{}, nil
			}
			return record.NewBool(isAnd), nil
		}, record.Bool, nil
	case "=", "<>", "<", "<=", ">", ">=":
		op := x.Op
		return func(r Row) (record.Value, error) {
			a, err := lf(r)
			if err != nil {
				return a, err
			}
			b, err := rf(r)
			if err != nil {
				return b, err
			}
			if a.IsNull() || b.IsNull() {
				return record.Value{}, nil
			}
			cmp, err := compareValues(a, b)
			if err != nil {
				return record.Value{}, err
			}
			var res bool
			switch op {
			case "=":
				res = cmp == 0
			case "<>":
				res = cmp != 0
			case "<":
				res = cmp < 0
			case "<=":
				res = cmp <= 0
			case ">":
				res = cmp > 0
			case ">=":
				res = cmp >= 0
			}
			return record.NewBool(res), nil
		}, record.Bool, nil
	case "||":
		return func(r Row) (record.Value, error) {
			a, err := lf(r)
			if err != nil {
				return a, err
			}
			b, err := rf(r)
			if err != nil {
				return b, err
			}
			if a.IsNull() || b.IsNull() {
				return record.Value{}, nil
			}
			return record.NewText(a.String() + b.String()), nil
		}, record.Text, nil
	}
	op := x.Op
	kind := record.Real
	if (lk == record.Int || lk == record.Null) && (rk == record.Int || rk == record.Null) && !(lk == record.Null && rk == record.Null) {
		kind = record.Int
	}
	return func(r Row) (record.Value, error) {
		a, err := lf(r)
		if err != nil {
			return a, err
		}
		b, err := rf(r)
		if err != nil {
			return b, err
		}
		return arith(op, a, b)
	}, kind, nil
}

func (c *compiler) caseExpr(x *sql.Case) (evalFn, record.Kind, error) {
	var operand evalFn
	var err error
	if x.Operand != nil {
		if operand, _, err = c.compile(x.Operand); err != nil {
			return nil, 0, err
		}
	}
	conds := make([]evalFn, len(x.Whens))
	results := make([]evalFn, len(x.Whens))
	kind := record.Null
	for i, w := range x.Whens {
		if conds[i], _, err = c.compile(w.Cond); err != nil {
			return nil, 0, err
		}
		var k record.Kind
		if results[i], k, err = c.compile(w.Result); err != nil {
			return nil, 0, err
		}
		if kind == record.Null {
			kind = k
		}
	}
	elseFn := constant(record.Value{})
	if x.Else != nil {
		var k record.Kind
		if elseFn, k, err = c.compile(x.Else); err != nil {
			return nil, 0, err
		}
		if kind == record.Null {
			kind = k
		}
	}
	return func(r Row) (record.Value, error) {
		var subject record.Value
		if operand != nil {
			v, err := operand(r)
			if err != nil {
				return v, err
			}
			subject = v
		}
		for i, cf := range conds {
			cv, err := cf(r)
			if err != nil {
				return cv, err
			}
			var hit bool
			if operand != nil {
				if !subject.IsNull() && !cv.IsNull() {
					cmp, err := compareValues(subject, cv)
					if err != nil {
						return record.Value{}, err
					}
					hit = cmp == 0
				}
			} else {
				t, null, err := truth(cv)
				if err != nil {
					return record.Value{}, err
				}
				hit = t && !null
			}
			if hit {
				return results[i](r)
			}
		}
		return elseFn(r)
	}, kind, nil
}

// --- scalar functions ----------------------------------------------------------

func (c *compiler) call(x *sql.Call) (evalFn, record.Kind, error) {
	args := make([]evalFn, len(x.Args))
	kinds := make([]record.Kind, len(x.Args))
	for i, a := range x.Args {
		var err error
		if args[i], kinds[i], err = c.compile(a); err != nil {
			return nil, 0, err
		}
	}
	nargs := func(min, max int) error {
		if len(args) < min || len(args) > max {
			return errf(CodeUndefinedFunction, "function %s() does not take %d arguments", x.Name, len(args))
		}
		return nil
	}
	// evalArgs evaluates all arguments; strict functions return NULL when
	// any argument is NULL.
	evalArgs := func(r Row, strict bool) ([]record.Value, bool, error) {
		vals := make([]record.Value, len(args))
		for i, f := range args {
			v, err := f(r)
			if err != nil {
				return nil, false, err
			}
			if strict && v.IsNull() {
				return nil, true, nil
			}
			vals[i] = v
		}
		return vals, false, nil
	}
	strict := func(kind record.Kind, fn func([]record.Value) (record.Value, error)) (evalFn, record.Kind, error) {
		return func(r Row) (record.Value, error) {
			vals, null, err := evalArgs(r, true)
			if err != nil || null {
				return record.Value{}, err
			}
			return fn(vals)
		}, kind, nil
	}
	switch x.Name {
	case "lower", "upper":
		if err := nargs(1, 1); err != nil {
			return nil, 0, err
		}
		up := x.Name == "upper"
		return strict(record.Text, func(v []record.Value) (record.Value, error) {
			if up {
				return record.NewText(strings.ToUpper(v[0].String())), nil
			}
			return record.NewText(strings.ToLower(v[0].String())), nil
		})
	case "length", "char_length", "character_length":
		if err := nargs(1, 1); err != nil {
			return nil, 0, err
		}
		return strict(record.Int, func(v []record.Value) (record.Value, error) {
			return record.NewInt(int64(utf8.RuneCountInString(v[0].String()))), nil
		})
	case "abs":
		if err := nargs(1, 1); err != nil {
			return nil, 0, err
		}
		return strict(kinds[0], func(v []record.Value) (record.Value, error) {
			n, err := toNumber(v[0])
			if err != nil {
				return n, err
			}
			if n.K == record.Int {
				if n.I == math.MinInt64 {
					return record.Value{}, errf(CodeNumericRange, "integer out of range")
				}
				if n.I < 0 {
					return record.NewInt(-n.I), nil
				}
				return n, nil
			}
			return record.NewReal(math.Abs(n.F)), nil
		})
	case "round":
		if err := nargs(1, 2); err != nil {
			return nil, 0, err
		}
		return strict(record.Real, func(v []record.Value) (record.Value, error) {
			n, err := toNumber(v[0])
			if err != nil {
				return n, err
			}
			digits := int64(0)
			if len(v) == 2 {
				d, err := toNumber(v[1])
				if err != nil || d.K != record.Int {
					return record.Value{}, errf(CodeDatatypeMismatch, "round() digits must be an integer")
				}
				digits = d.I
			}
			scale := math.Pow(10, float64(digits))
			return record.NewReal(math.Round(n.Float()*scale) / scale), nil
		})
	case "coalesce":
		if len(args) == 0 {
			return nil, 0, errf(CodeUndefinedFunction, "coalesce() needs at least one argument")
		}
		kind := record.Null
		for _, k := range kinds {
			if kind == record.Null {
				kind = k
			}
		}
		return func(r Row) (record.Value, error) {
			for _, f := range args {
				v, err := f(r)
				if err != nil || !v.IsNull() {
					return v, err
				}
			}
			return record.Value{}, nil
		}, kind, nil
	case "nullif":
		if err := nargs(2, 2); err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) {
			vals, _, err := evalArgs(r, false)
			if err != nil {
				return record.Value{}, err
			}
			if !vals[0].IsNull() && !vals[1].IsNull() {
				if cmp, err := compareValues(vals[0], vals[1]); err == nil && cmp == 0 {
					return record.Value{}, nil
				}
			}
			return vals[0], nil
		}, kinds[0], nil
	case "substr", "substring":
		if err := nargs(2, 3); err != nil {
			return nil, 0, err
		}
		return strict(record.Text, func(v []record.Value) (record.Value, error) {
			runes := []rune(v[0].String())
			start, err := toNumber(v[1])
			if err != nil || start.K != record.Int {
				return record.Value{}, errf(CodeDatatypeMismatch, "substring start must be an integer")
			}
			from := start.I - 1
			to := int64(len(runes))
			if len(v) == 3 {
				n, err := toNumber(v[2])
				if err != nil || n.K != record.Int || n.I < 0 {
					return record.Value{}, errf(CodeDatatypeMismatch, "substring length must be a non-negative integer")
				}
				to = from + n.I
			}
			from = max(from, 0)
			to = min(to, int64(len(runes)))
			if from >= to {
				return record.NewText(""), nil
			}
			return record.NewText(string(runes[from:to])), nil
		})
	case "trim":
		if err := nargs(1, 1); err != nil {
			return nil, 0, err
		}
		return strict(record.Text, func(v []record.Value) (record.Value, error) {
			return record.NewText(strings.TrimSpace(v[0].String())), nil
		})
	case "replace":
		if err := nargs(3, 3); err != nil {
			return nil, 0, err
		}
		return strict(record.Text, func(v []record.Value) (record.Value, error) {
			return record.NewText(strings.ReplaceAll(v[0].String(), v[1].String(), v[2].String())), nil
		})
	case "concat":
		return func(r Row) (record.Value, error) {
			vals, _, err := evalArgs(r, false)
			if err != nil {
				return record.Value{}, err
			}
			var sb strings.Builder
			for _, v := range vals {
				if !v.IsNull() {
					sb.WriteString(v.String())
				}
			}
			return record.NewText(sb.String()), nil
		}, record.Text, nil
	case "random":
		if err := nargs(0, 0); err != nil {
			return nil, 0, err
		}
		return func(Row) (record.Value, error) { return record.NewReal(rand.Float64()), nil }, record.Real, nil
	case "typeof":
		if err := nargs(1, 1); err != nil {
			return nil, 0, err
		}
		return func(r Row) (record.Value, error) {
			v, err := args[0](r)
			if err != nil {
				return v, err
			}
			return record.NewText(strings.ToLower(v.K.String())), nil
		}, record.Text, nil
	case "version":
		return constant(record.NewText(VersionString)), record.Text, nil
	case "current_database":
		return constant(record.NewText("oakdb")), record.Text, nil
	}
	return nil, 0, errf(CodeUndefinedFunction, "function %s() does not exist", x.Name)
}

// VersionString is reported by version() and the wire protocol.
const VersionString = "OakDB 1.0 (speaks the PostgreSQL protocol)"

// --- value semantics -----------------------------------------------------------

// truth interprets a value as a SQL boolean. NULL is "unknown".
func truth(v record.Value) (val, null bool, err error) {
	switch v.K {
	case record.Null:
		return false, true, nil
	case record.Bool:
		return v.B, false, nil
	case record.Int:
		return v.I != 0, false, nil
	case record.Real:
		return v.F != 0, false, nil
	}
	b, err := record.Coerce(v, record.Bool)
	if err != nil {
		return false, false, errf(CodeDatatypeMismatch, "argument of WHERE must be type boolean, not %s", strings.ToLower(v.K.String()))
	}
	return b.B, false, nil
}

// compareValues compares two non-null values. A text value compared with a
// number or boolean is parsed first (so a text parameter like '5' matches
// an integer column), which mirrors how PostgreSQL treats untyped literals.
func compareValues(a, b record.Value) (int, error) {
	if a.K == record.Text && b.K != record.Text {
		v, err := record.Coerce(a, kindForCompare(b))
		if err != nil {
			return 0, errf(CodeInvalidText, "%v", err)
		}
		a = v
	} else if b.K == record.Text && a.K != record.Text {
		v, err := record.Coerce(b, kindForCompare(a))
		if err != nil {
			return 0, errf(CodeInvalidText, "%v", err)
		}
		b = v
	}
	return record.Compare(a, b), nil
}

func kindForCompare(v record.Value) record.Kind {
	if v.K == record.Int {
		return record.Real // '2.5' compared with 2 must not fail
	}
	return v.K
}

func toNumber(v record.Value) (record.Value, error) {
	switch v.K {
	case record.Int, record.Real:
		return v, nil
	case record.Text:
		if n, err := record.Coerce(v, record.Int); err == nil {
			return n, nil
		}
		n, err := record.Coerce(v, record.Real)
		if err != nil {
			return record.Value{}, errf(CodeInvalidText, "%v", err)
		}
		return n, nil
	}
	return record.Value{}, errf(CodeUndefinedFunction, "operator does not exist for type %s", strings.ToLower(v.K.String()))
}

// arith applies + - * / % with NULL propagation, integer overflow checks
// and integer division semantics.
func arith(op string, a, b record.Value) (record.Value, error) {
	if a.IsNull() || b.IsNull() {
		return record.Value{}, nil
	}
	a, err := toNumber(a)
	if err != nil {
		return a, err
	}
	b, err = toNumber(b)
	if err != nil {
		return b, err
	}
	if a.K == record.Int && b.K == record.Int {
		x, y := a.I, b.I
		var r int64
		switch op {
		case "+":
			r = x + y
			if (r > x) != (y > 0) {
				return record.Value{}, errf(CodeNumericRange, "integer out of range")
			}
		case "-":
			r = x - y
			if (r < x) != (y > 0) {
				return record.Value{}, errf(CodeNumericRange, "integer out of range")
			}
		case "*":
			if x != 0 && y != 0 {
				r = x * y
				if r/y != x || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) {
					return record.Value{}, errf(CodeNumericRange, "integer out of range")
				}
			}
		case "/", "%":
			if y == 0 {
				return record.Value{}, errf(CodeDivisionByZero, "division by zero")
			}
			if x == math.MinInt64 && y == -1 {
				if op == "%" {
					return record.NewInt(0), nil
				}
				return record.Value{}, errf(CodeNumericRange, "integer out of range")
			}
			if op == "/" {
				r = x / y
			} else {
				r = x % y
			}
		}
		return record.NewInt(r), nil
	}
	x, y := a.Float(), b.Float()
	switch op {
	case "+":
		return record.NewReal(x + y), nil
	case "-":
		return record.NewReal(x - y), nil
	case "*":
		return record.NewReal(x * y), nil
	case "/":
		if y == 0 {
			return record.Value{}, errf(CodeDivisionByZero, "division by zero")
		}
		return record.NewReal(x / y), nil
	case "%":
		if y == 0 {
			return record.Value{}, errf(CodeDivisionByZero, "division by zero")
		}
		return record.NewReal(math.Mod(x, y)), nil
	}
	return record.Value{}, errf(CodeUndefinedFunction, "unknown operator %s", op)
}

func castValue(v record.Value, k record.Kind) (record.Value, error) {
	if v.K == record.Real && k == record.Int {
		f := math.RoundToEven(v.F)
		if f < math.MinInt64 || f >= math.MaxInt64 || math.IsNaN(f) {
			return record.Value{}, errf(CodeNumericRange, "integer out of range")
		}
		return record.NewInt(int64(f)), nil
	}
	out, err := record.Coerce(v, k)
	if err != nil {
		return out, errf(CodeInvalidText, "%v", err)
	}
	return out, nil
}

// like matches SQL LIKE patterns: % is any run of characters, _ is exactly
// one character and \ escapes the next character.
func like(s, pattern string) bool {
	str, pat := []rune(s), []rune(pattern)
	// Iterative matcher with backtracking to the last %, linear in practice.
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(str) {
		if pi < len(pat) {
			switch pat[pi] {
			case '%':
				starP, starS = pi, si
				pi++
				continue
			case '_':
				si++
				pi++
				continue
			case '\\':
				if pi+1 < len(pat) && pat[pi+1] == str[si] {
					si++
					pi += 2
					continue
				}
			default:
				if pat[pi] == str[si] {
					si++
					pi++
					continue
				}
			}
		}
		if starP < 0 {
			return false
		}
		starS++
		si, pi = starS, starP+1
	}
	for pi < len(pat) && pat[pi] == '%' {
		pi++
	}
	return pi == len(pat)
}
