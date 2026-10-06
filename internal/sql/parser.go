package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

type parser struct {
	toks []token
	pos  int
}

// Parse parses one or more statements separated by semicolons. Empty
// statements are skipped.
func Parse(src string) ([]Stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var out []Stmt
	for {
		for p.acceptOp(";") {
		}
		if p.peek().kind == tEOF {
			return out, nil
		}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if !p.acceptOp(";") && p.peek().kind != tEOF {
			return nil, p.errf("expected end of statement, found %s", p.describe())
		}
	}
}

// ParseOne parses exactly one statement (a trailing semicolon is allowed).
// It returns nil for an empty query.
func ParseOne(src string) (Stmt, error) {
	stmts, err := Parse(src)
	if err != nil {
		return nil, err
	}
	switch len(stmts) {
	case 0:
		return nil, nil
	case 1:
		return stmts[0], nil
	}
	return nil, &Error{0, "expected a single statement"}
}

// --- token helpers -----------------------------------------------------------

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) errf(format string, args ...any) error {
	return &Error{p.peek().pos, fmt.Sprintf(format, args...)}
}

func (p *parser) describe() string {
	t := p.peek()
	switch t.kind {
	case tEOF:
		return "end of input"
	case tString:
		return "string '" + t.text + "'"
	}
	return `"` + t.text + `"`
}

func (p *parser) isKw(kw string) bool { t := p.peek(); return t.kind == tIdent && t.text == kw }

func (p *parser) acceptKw(kws ...string) bool {
	for i, kw := range kws {
		t := p.peekAt(i)
		if t.kind != tIdent || t.text != kw {
			return false
		}
	}
	p.pos += len(kws)
	return true
}

func (p *parser) expectKw(kws ...string) error {
	if !p.acceptKw(kws...) {
		return p.errf("expected %s, found %s", strings.ToUpper(strings.Join(kws, " ")), p.describe())
	}
	return nil
}

func (p *parser) acceptOp(op string) bool {
	if t := p.peek(); t.kind == tOp && t.text == op {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errf("expected %q, found %s", op, p.describe())
	}
	return nil
}

// ident parses a (possibly quoted) identifier.
func (p *parser) ident(what string) (string, error) {
	t := p.peek()
	if t.kind == tQuotedIdent || t.kind == tIdent && !reserved[t.text] {
		p.pos++
		return t.text, nil
	}
	return "", p.errf("expected %s, found %s", what, p.describe())
}

func (p *parser) identList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		id, err := p.ident("column name")
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		if !p.acceptOp(",") {
			break
		}
	}
	return out, p.expectOp(")")
}

// --- statements --------------------------------------------------------------

func (p *parser) statement() (Stmt, error) {
	t := p.peek()
	if t.kind == tOp && t.text == "(" {
		return p.selectStmt()
	}
	if t.kind != tIdent {
		return nil, p.errf("expected a statement, found %s", p.describe())
	}
	switch t.text {
	case "select":
		return p.selectStmt()
	case "insert":
		return p.insert()
	case "update":
		return p.update()
	case "delete":
		return p.delete()
	case "create":
		return p.create()
	case "drop":
		return p.drop()
	case "begin", "start":
		p.next()
		if t.text == "start" {
			if err := p.expectKw("transaction"); err != nil {
				return nil, err
			}
		} else {
			p.acceptKw("transaction")
			p.acceptKw("work")
		}
		p.skipTxnModes()
		return &Begin{}, nil
	case "commit", "end":
		p.next()
		p.acceptKw("transaction")
		p.acceptKw("work")
		return &Commit{}, nil
	case "rollback", "abort":
		p.next()
		p.acceptKw("transaction")
		p.acceptKw("work")
		return &Rollback{}, nil
	case "explain":
		p.next()
		p.acceptKw("query", "plan")
		inner, err := p.statement()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: inner}, nil
	case "set":
		return p.set()
	case "show":
		p.next()
		var parts []string
		for p.peek().kind == tIdent {
			parts = append(parts, p.next().text)
		}
		return &Show{Name: strings.Join(parts, " ")}, nil
	}
	return nil, p.errf("unsupported statement %s", strings.ToUpper(t.text))
}

// skipTxnModes accepts and ignores "ISOLATION LEVEL ..." and "READ WRITE".
// OakDB transactions are always serializable.
func (p *parser) skipTxnModes() {
	for p.peek().kind == tIdent {
		p.next()
	}
}

func (p *parser) set() (Stmt, error) {
	p.next()
	p.acceptKw("session")
	p.acceptKw("local")
	var name []string
	for p.peek().kind == tIdent || p.peek().kind == tOp && p.peek().text == "." {
		name = append(name, p.next().text)
	}
	if !p.acceptOp("=") {
		p.acceptKw("to")
	}
	var val []string
	for p.peek().kind != tEOF && !(p.peek().kind == tOp && p.peek().text == ";") {
		val = append(val, p.next().text)
	}
	return &Set{Name: strings.Join(name, ""), Value: strings.Join(val, " ")}, nil
}

func (p *parser) create() (Stmt, error) {
	p.next()
	unique := p.acceptKw("unique")
	if p.acceptKw("index") {
		return p.createIndex(unique)
	}
	if unique {
		return nil, p.errf("expected INDEX after UNIQUE")
	}
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	ct := &CreateTable{}
	ct.IfNotExists = p.acceptKw("if", "not", "exists")
	name, err := p.ident("table name")
	if err != nil {
		return nil, err
	}
	ct.Name = name
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var tablePK []string
	for {
		if p.acceptKw("primary", "key") {
			cols, err := p.identList()
			if err != nil {
				return nil, err
			}
			tablePK = cols
		} else if p.acceptKw("constraint") {
			return nil, p.errf("named constraints are not supported")
		} else {
			col, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			ct.Columns = append(ct.Columns, col)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if len(tablePK) > 1 {
		return nil, p.errf("composite primary keys are not supported; use a unique index")
	}
	if len(tablePK) == 1 {
		found := false
		for i := range ct.Columns {
			if ct.Columns[i].Name == tablePK[0] {
				ct.Columns[i].PrimaryKey = true
				found = true
			}
		}
		if !found {
			return nil, p.errf("primary key column %q does not exist", tablePK[0])
		}
	}
	return ct, nil
}

func (p *parser) columnDef() (ColumnDef, error) {
	name, err := p.ident("column name")
	if err != nil {
		return ColumnDef{}, err
	}
	typ, err := p.typeName()
	if err != nil {
		return ColumnDef{}, err
	}
	col := ColumnDef{Name: name, Type: typ}
	for {
		switch {
		case p.acceptKw("primary", "key"):
			col.PrimaryKey = true
			col.NotNull = true
		case p.acceptKw("not", "null"):
			col.NotNull = true
		case p.acceptKw("null"):
		case p.acceptKw("unique"):
			col.Unique = true
		default:
			return col, nil
		}
	}
}

// typeName maps SQL type names to OakDB's storage kinds.
func (p *parser) typeName() (record.Kind, error) {
	t := p.peek()
	if t.kind != tIdent {
		return 0, p.errf("expected a type name, found %s", p.describe())
	}
	p.next()
	var k record.Kind
	switch t.text {
	case "int", "integer", "int2", "int4", "int8", "bigint", "smallint", "serial", "bigserial":
		k = record.Int
	case "text", "varchar", "char", "string", "character", "name":
		k = record.Text
		p.acceptKw("varying")
	case "real", "float", "float4", "float8", "numeric", "decimal":
		k = record.Real
	case "double":
		p.acceptKw("precision")
		k = record.Real
	case "bool", "boolean":
		k = record.Bool
	default:
		return 0, &Error{t.pos, fmt.Sprintf("unknown type %q", t.text)}
	}
	// Ignore length/precision modifiers like VARCHAR(20) or NUMERIC(10,2).
	if p.acceptOp("(") {
		for !p.acceptOp(")") {
			if p.peek().kind == tEOF {
				return 0, p.errf("unterminated type modifier")
			}
			p.next()
		}
	}
	return k, nil
}

func (p *parser) createIndex(unique bool) (Stmt, error) {
	ci := &CreateIndex{Unique: unique}
	ci.IfNotExists = p.acceptKw("if", "not", "exists")
	name, err := p.ident("index name")
	if err != nil {
		return nil, err
	}
	ci.Name = name
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	if ci.Table, err = p.ident("table name"); err != nil {
		return nil, err
	}
	if ci.Columns, err = p.identList(); err != nil {
		return nil, err
	}
	return ci, nil
}

func (p *parser) drop() (Stmt, error) {
	p.next()
	isIndex := p.acceptKw("index")
	if !isIndex {
		if err := p.expectKw("table"); err != nil {
			return nil, err
		}
	}
	ifExists := p.acceptKw("if", "exists")
	name, err := p.ident("name")
	if err != nil {
		return nil, err
	}
	if isIndex {
		return &DropIndex{Name: name, IfExists: ifExists}, nil
	}
	return &DropTable{Name: name, IfExists: ifExists}, nil
}

func (p *parser) insert() (Stmt, error) {
	p.next()
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	ins := &Insert{}
	var err error
	if ins.Table, err = p.ident("table name"); err != nil {
		return nil, err
	}
	if p.peek().kind == tOp && p.peek().text == "(" && !(p.peekAt(1).kind == tIdent && p.peekAt(1).text == "select") {
		if ins.Columns, err = p.identList(); err != nil {
			return nil, err
		}
	}
	if p.isKw("select") || p.peek().kind == tOp && p.peek().text == "(" {
		s, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		ins.Select = s.(*Select)
		return ins, nil
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if !p.acceptOp(",") {
			break
		}
	}
	return ins, nil
}

func (p *parser) update() (Stmt, error) {
	p.next()
	up := &Update{}
	var err error
	if up.Table, err = p.ident("table name"); err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident("column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		val, err := p.expr()
		if err != nil {
			return nil, err
		}
		up.Sets = append(up.Sets, Assignment{Column: col, Value: val})
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("where") {
		if up.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return up, nil
}

func (p *parser) delete() (Stmt, error) {
	p.next()
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	del := &Delete{}
	var err error
	if del.Table, err = p.ident("table name"); err != nil {
		return nil, err
	}
	if p.acceptKw("where") {
		if del.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return del, nil
}

func (p *parser) selectStmt() (Stmt, error) {
	if p.acceptOp("(") {
		s, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		return s, p.expectOp(")")
	}
	if err := p.expectKw("select"); err != nil {
		return nil, err
	}
	s := &Select{}
	if p.acceptKw("distinct") {
		s.Distinct = true
	} else {
		p.acceptKw("all")
	}
	for {
		item, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	var err error
	if p.acceptKw("from") {
		if s.From, err = p.fromClause(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("group", "by") {
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, e)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("having") {
		if s.Having, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("order", "by") {
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			if p.acceptKw("desc") {
				item.Desc = true
			} else {
				p.acceptKw("asc")
			}
			s.OrderBy = append(s.OrderBy, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	for {
		if p.acceptKw("limit") {
			if p.acceptKw("all") {
				continue
			}
			if s.Limit, err = p.expr(); err != nil {
				return nil, err
			}
		} else if p.acceptKw("offset") {
			if s.Offset, err = p.expr(); err != nil {
				return nil, err
			}
			p.acceptKw("rows")
		} else {
			break
		}
	}
	return s, nil
}

func (p *parser) selectItem() (SelectItem, error) {
	if p.acceptOp("*") {
		return SelectItem{Star: true}, nil
	}
	if t := p.peek(); (t.kind == tIdent || t.kind == tQuotedIdent) &&
		p.peekAt(1).kind == tOp && p.peekAt(1).text == "." &&
		p.peekAt(2).kind == tOp && p.peekAt(2).text == "*" {
		p.pos += 3
		return SelectItem{Star: true, StarTable: t.text}, nil
	}
	e, err := p.expr()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Expr: e}
	if p.acceptKw("as") {
		if item.Alias, err = p.ident("alias"); err != nil {
			return item, err
		}
	} else if t := p.peek(); t.kind == tQuotedIdent || t.kind == tIdent && !reserved[t.text] {
		item.Alias = p.next().text
	}
	return item, nil
}

func (p *parser) tableRef() (FromItem, error) {
	name, err := p.ident("table name")
	if err != nil {
		return FromItem{}, err
	}
	fi := FromItem{Table: name}
	if p.acceptKw("as") {
		if fi.Alias, err = p.ident("alias"); err != nil {
			return fi, err
		}
	} else if t := p.peek(); t.kind == tQuotedIdent || t.kind == tIdent && !reserved[t.text] {
		fi.Alias = p.next().text
	}
	return fi, nil
}

func (p *parser) fromClause() ([]FromItem, error) {
	first, err := p.tableRef()
	if err != nil {
		return nil, err
	}
	items := []FromItem{first}
	for {
		var join string
		switch {
		case p.acceptOp(","):
			join = "CROSS"
		case p.acceptKw("cross", "join"):
			join = "CROSS"
		case p.acceptKw("inner", "join"), p.acceptKw("join"):
			join = "INNER"
		case p.acceptKw("left", "outer", "join"), p.acceptKw("left", "join"):
			join = "LEFT"
		default:
			return items, nil
		}
		fi, err := p.tableRef()
		if err != nil {
			return nil, err
		}
		fi.Join = join
		if join != "CROSS" {
			if err := p.expectKw("on"); err != nil {
				return nil, err
			}
			if fi.On, err = p.expr(); err != nil {
				return nil, err
			}
		}
		items = append(items, fi)
	}
}

// --- expressions (lowest to highest precedence) ----------------------------

func (p *parser) expr() (Expr, error) { return p.orExpr() }

func (p *parser) orExpr() (Expr, error) {
	l, err := p.andExpr()
	for err == nil && p.acceptKw("or") {
		var r Expr
		if r, err = p.andExpr(); err == nil {
			l = &Binary{Op: "OR", L: l, R: r}
		}
	}
	return l, err
}

func (p *parser) andExpr() (Expr, error) {
	l, err := p.notExpr()
	for err == nil && p.acceptKw("and") {
		var r Expr
		if r, err = p.notExpr(); err == nil {
			l = &Binary{Op: "AND", L: l, R: r}
		}
	}
	return l, err
}

func (p *parser) notExpr() (Expr, error) {
	if p.acceptKw("not") {
		x, err := p.notExpr()
		return &Unary{Op: "NOT", X: x}, err
	}
	return p.comparison()
}

func (p *parser) comparison() (Expr, error) {
	l, err := p.concat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == tOp {
			switch t.text {
			case "=", "<>", "!=", "<", "<=", ">", ">=":
				p.next()
				r, err := p.concat()
				if err != nil {
					return nil, err
				}
				op := t.text
				if op == "!=" {
					op = "<>"
				}
				l = &Binary{Op: op, L: l, R: r}
				continue
			}
			return l, nil
		}
		if p.acceptKw("is") {
			not := p.acceptKw("not")
			if p.acceptKw("null") {
				l = &IsNull{X: l, Not: not}
				continue
			}
			if p.acceptKw("true") {
				l = isBool(l, true, not)
				continue
			}
			if p.acceptKw("false") {
				l = isBool(l, false, not)
				continue
			}
			return nil, p.errf("expected NULL, TRUE or FALSE after IS")
		}
		not := false
		if p.isKw("not") {
			nt := p.peekAt(1)
			if nt.kind == tIdent && (nt.text == "like" || nt.text == "ilike" || nt.text == "in" || nt.text == "between") {
				p.next()
				not = true
			}
		}
		switch {
		case p.acceptKw("like"), p.isKw("ilike"):
			insensitive := p.acceptKw("ilike")
			r, err := p.concat()
			if err != nil {
				return nil, err
			}
			l = &Like{X: l, Pattern: r, Not: not, Insensitive: insensitive}
		case p.acceptKw("in"):
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			if p.isKw("select") {
				return nil, p.errf("subqueries are not supported")
			}
			in := &In{X: l, Not: not}
			for {
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				in.List = append(in.List, e)
				if !p.acceptOp(",") {
					break
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			l = in
		case p.acceptKw("between"):
			lo, err := p.concat()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("and"); err != nil {
				return nil, err
			}
			hi, err := p.concat()
			if err != nil {
				return nil, err
			}
			l = &Between{X: l, Lo: lo, Hi: hi, Not: not}
		default:
			return l, nil
		}
	}
}

// isBool rewrites "x IS [NOT] TRUE/FALSE" into equivalent expressions that
// treat NULL as "not true" / "not false".
func isBool(x Expr, val, not bool) Expr {
	lit := &Literal{Val: record.NewBool(val)}
	cmp := &Binary{Op: "AND", L: &IsNull{X: x, Not: true}, R: &Binary{Op: "=", L: x, R: lit}}
	if not {
		return &Unary{Op: "NOT", X: cmp}
	}
	return cmp
}

func (p *parser) concat() (Expr, error) {
	l, err := p.additive()
	for err == nil && p.acceptOp("||") {
		var r Expr
		if r, err = p.additive(); err == nil {
			l = &Binary{Op: "||", L: l, R: r}
		}
	}
	return l, err
}

func (p *parser) additive() (Expr, error) {
	l, err := p.multiplicative()
	for err == nil {
		t := p.peek()
		if t.kind != tOp || t.text != "+" && t.text != "-" {
			break
		}
		p.next()
		var r Expr
		if r, err = p.multiplicative(); err == nil {
			l = &Binary{Op: t.text, L: l, R: r}
		}
	}
	return l, err
}

func (p *parser) multiplicative() (Expr, error) {
	l, err := p.unary()
	for err == nil {
		t := p.peek()
		if t.kind != tOp || t.text != "*" && t.text != "/" && t.text != "%" {
			break
		}
		p.next()
		var r Expr
		if r, err = p.unary(); err == nil {
			l = &Binary{Op: t.text, L: l, R: r}
		}
	}
	return l, err
}

func (p *parser) unary() (Expr, error) {
	if p.acceptOp("-") {
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		// Fold negative literals so -9223372036854775808 works.
		if lit, ok := x.(*Literal); ok {
			switch lit.Val.K {
			case record.Int:
				return &Literal{Val: record.NewInt(-lit.Val.I)}, nil
			case record.Real:
				if lit.Val.F == 9223372036854775808 {
					return &Literal{Val: record.NewInt(math.MinInt64)}, nil
				}
				return &Literal{Val: record.NewReal(-lit.Val.F)}, nil
			}
		}
		return &Unary{Op: "-", X: x}, nil
	}
	if p.acceptOp("+") {
		return p.unary()
	}
	return p.postfix()
}

func (p *parser) postfix() (Expr, error) {
	x, err := p.primary()
	for err == nil && p.acceptOp("::") {
		var k record.Kind
		if k, err = p.typeName(); err == nil {
			x = &Cast{X: x, Type: k}
		}
	}
	return x, err
}

func (p *parser) primary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.next()
		return numberLiteral(t)
	case tString:
		p.next()
		return &Literal{Val: record.NewText(t.text)}, nil
	case tParam:
		p.next()
		n, err := strconv.Atoi(t.text)
		if err != nil || n < 1 {
			return nil, &Error{t.pos, "invalid parameter number"}
		}
		return &Param{Index: n}, nil
	case tOp:
		if t.text == "(" {
			p.next()
			if p.isKw("select") {
				return nil, p.errf("subqueries are not supported")
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		}
	case tQuotedIdent:
		return p.columnRef()
	case tIdent:
		switch t.text {
		case "null":
			p.next()
			return &Literal{}, nil
		case "true", "false":
			p.next()
			return &Literal{Val: record.NewBool(t.text == "true")}, nil
		case "case":
			return p.caseExpr()
		case "cast":
			p.next()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			k, err := p.typeName()
			if err != nil {
				return nil, err
			}
			return &Cast{X: x, Type: k}, p.expectOp(")")
		case "exists":
			return nil, p.errf("subqueries are not supported")
		}
		if p.peekAt(1).kind == tOp && p.peekAt(1).text == "(" && !reserved[t.text] {
			return p.call()
		}
		if !reserved[t.text] {
			return p.columnRef()
		}
	}
	return nil, p.errf("unexpected %s", p.describe())
}

func numberLiteral(t token) (Expr, error) {
	if !strings.ContainsAny(t.text, ".eE") {
		if i, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			return &Literal{Val: record.NewInt(i)}, nil
		}
		// Too big for an integer: fall through to a real. Unary minus turns
		// -9223372036854775808 back into the smallest integer.
	}
	f, err := strconv.ParseFloat(t.text, 64)
	if err != nil {
		return nil, &Error{t.pos, fmt.Sprintf("invalid number %q", t.text)}
	}
	return &Literal{Val: record.NewReal(f)}, nil
}

func (p *parser) columnRef() (Expr, error) {
	first := p.next().text
	if p.acceptOp(".") {
		col, err := p.ident("column name")
		if err != nil {
			return nil, err
		}
		return &ColumnRef{Table: first, Column: col}, nil
	}
	return &ColumnRef{Column: first}, nil
}

func (p *parser) call() (Expr, error) {
	name := p.next().text
	p.next() // (
	c := &Call{Name: name}
	if p.acceptOp("*") {
		c.Star = true
		return c, p.expectOp(")")
	}
	if p.acceptOp(")") {
		return c, nil
	}
	c.Distinct = p.acceptKw("distinct")
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.Args = append(c.Args, e)
		if !p.acceptOp(",") {
			break
		}
	}
	return c, p.expectOp(")")
}

func (p *parser) caseExpr() (Expr, error) {
	p.next()
	c := &Case{}
	var err error
	if !p.isKw("when") {
		if c.Operand, err = p.expr(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("when") {
		var w When
		if w.Cond, err = p.expr(); err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		if w.Result, err = p.expr(); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.errf("CASE needs at least one WHEN")
	}
	if p.acceptKw("else") {
		if c.Else, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("end")
}
