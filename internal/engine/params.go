package engine

import (
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// ParamTypes infers the type of each $n parameter from where it is used,
// as PostgreSQL does when a statement is prepared: "id = $1" makes $1 the
// type of id, "INSERT INTO t (a) VALUES ($1)" makes it the type of a, and
// LIMIT $1 is an integer. Drivers use these types to encode values.
// Parameters with no clue default to text.
func (s *Session) ParamTypes(st sql.Stmt) []record.Kind {
	kinds := make([]record.Kind, ParamCount(st))
	if len(kinds) == 0 {
		return kinds
	}
	r := s.db.pager.BeginRead()
	defer r.Release()
	reader := r
	cat, err := s.db.catalogFor(reader)
	if err != nil {
		return fillText(kinds)
	}
	if s.tx != nil {
		if c, err := s.db.catalogFor(s.tx.reader()); err == nil {
			cat = c
		}
	}
	p := &planner{cat: cat}
	set := func(e sql.Expr, k record.Kind) {
		if prm, ok := e.(*sql.Param); ok && k != record.Null && kinds[prm.Index-1] == record.Null {
			kinds[prm.Index-1] = k
		}
	}
	kindOf := func(e sql.Expr, sc *scope) record.Kind {
		if _, ok := e.(*sql.Param); ok {
			return record.Null
		}
		_, k, err := (&compiler{sc: sc}).compile(e)
		if err != nil {
			return record.Null
		}
		return k
	}
	walk := func(e sql.Expr, sc *scope) {
		mapExpr(e, func(x sql.Expr) sql.Expr {
			switch n := x.(type) {
			case *sql.Binary:
				switch n.Op {
				case "AND", "OR":
					set(n.L, record.Bool)
					set(n.R, record.Bool)
				case "||":
					set(n.L, record.Text)
					set(n.R, record.Text)
				default:
					set(n.L, kindOf(n.R, sc))
					set(n.R, kindOf(n.L, sc))
				}
			case *sql.Like:
				set(n.X, record.Text)
				set(n.Pattern, record.Text)
			case *sql.In:
				xk := kindOf(n.X, sc)
				for _, item := range n.List {
					set(item, xk)
					set(n.X, kindOf(item, sc))
				}
			case *sql.Between:
				xk := kindOf(n.X, sc)
				set(n.Lo, xk)
				set(n.Hi, xk)
				set(n.X, kindOf(n.Lo, sc))
			case *sql.Unary:
				if n.Op == "NOT" {
					set(n.X, record.Bool)
				}
			case *sql.Cast:
				set(n.X, n.Type)
			}
			return x
		})
	}
	var visit func(st sql.Stmt)
	visit = func(st sql.Stmt) {
		switch st := st.(type) {
		case *sql.Select:
			sc := &scope{}
			for _, fi := range st.From {
				t, ok := cat.Tables[fi.Table]
				if !ok {
					continue
				}
				alias := fi.Alias
				if alias == "" {
					alias = fi.Table
				}
				sc.cols = append(sc.cols, tableScope(t, alias)...)
			}
			for _, it := range st.Items {
				walk(it.Expr, sc)
			}
			for _, fi := range st.From {
				walk(fi.On, sc)
			}
			walk(st.Where, sc)
			for _, g := range st.GroupBy {
				walk(g, sc)
			}
			walk(st.Having, sc)
			for _, o := range st.OrderBy {
				walk(o.Expr, sc)
			}
			set(st.Limit, record.Int)
			set(st.Offset, record.Int)
		case *sql.Insert:
			t, err := p.table(st.Table)
			if err != nil {
				return
			}
			cols := make([]int, 0, len(t.Columns))
			if len(st.Columns) == 0 {
				for i := range t.Columns {
					cols = append(cols, i)
				}
			} else {
				for _, name := range st.Columns {
					cols = append(cols, t.ColumnIndex(name))
				}
			}
			for _, row := range st.Rows {
				for i, e := range row {
					if i < len(cols) && cols[i] >= 0 {
						set(e, t.Columns[cols[i]].Type)
					}
					walk(e, &scope{})
				}
			}
			if st.Select != nil {
				visit(st.Select)
			}
		case *sql.Update:
			t, err := p.table(st.Table)
			if err != nil {
				return
			}
			sc := &scope{cols: tableScope(t, t.Name)}
			for _, a := range st.Sets {
				if c := t.ColumnIndex(a.Column); c >= 0 {
					set(a.Value, t.Columns[c].Type)
				}
				walk(a.Value, sc)
			}
			walk(st.Where, sc)
		case *sql.Delete:
			t, err := p.table(st.Table)
			if err != nil {
				return
			}
			walk(st.Where, &scope{cols: tableScope(t, t.Name)})
		case *sql.Explain:
			visit(st.Stmt)
		}
	}
	visit(st)
	return fillText(kinds)
}

func fillText(kinds []record.Kind) []record.Kind {
	for i, k := range kinds {
		if k == record.Null {
			kinds[i] = record.Text
		}
	}
	return kinds
}
