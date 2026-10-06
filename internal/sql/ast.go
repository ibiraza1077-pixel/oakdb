package sql

import (
	"strconv"
	"strings"

	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// Stmt is any SQL statement.
type Stmt interface{ stmt() }

// Expr is any SQL expression.
type Expr interface{ expr() }

// --- expressions -------------------------------------------------------------

type (
	Literal   struct{ Val record.Value }
	Param     struct{ Index int } // $1 is Index 1
	ColumnRef struct{ Table, Column string }
	Unary     struct {
		Op string // "-", "NOT"
		X  Expr
	}
	Binary struct {
		Op   string // + - * / % || = <> < <= > >= AND OR
		L, R Expr
	}
	IsNull struct {
		X   Expr
		Not bool
	}
	Like struct {
		X, Pattern       Expr
		Not, Insensitive bool
	}
	In struct {
		X    Expr
		List []Expr
		Not  bool
	}
	Between struct {
		X, Lo, Hi Expr
		Not       bool
	}
	Call struct {
		Name     string // lower case
		Args     []Expr
		Star     bool // COUNT(*)
		Distinct bool
	}
	When struct{ Cond, Result Expr }
	Case struct {
		Operand Expr // CASE x WHEN ... (nil for CASE WHEN cond ...)
		Whens   []When
		Else    Expr
	}
	Cast struct {
		X    Expr
		Type record.Kind
	}
)

func (*Literal) expr()   {}
func (*Param) expr()     {}
func (*ColumnRef) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*IsNull) expr()    {}
func (*Like) expr()      {}
func (*In) expr()        {}
func (*Between) expr()   {}
func (*Call) expr()      {}
func (*Case) expr()      {}
func (*Cast) expr()      {}

// --- statements --------------------------------------------------------------

type ColumnDef struct {
	Name       string
	Type       record.Kind
	PrimaryKey bool
	NotNull    bool
	Unique     bool
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
}

type DropTable struct {
	Name     string
	IfExists bool
}

type CreateIndex struct {
	Name, Table string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

type DropIndex struct {
	Name     string
	IfExists bool
}

type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
	Select  *Select
}

type SelectItem struct {
	Expr      Expr
	Alias     string
	Star      bool   // SELECT *
	StarTable string // SELECT t.*
}

type FromItem struct {
	Table, Alias string
	Join         string // "" for the first table, else "INNER", "LEFT" or "CROSS"
	On           Expr
}

type OrderItem struct {
	Expr Expr
	Desc bool
}

type Select struct {
	Distinct bool
	Items    []SelectItem
	From     []FromItem
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table string
	Sets  []Assignment
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

type (
	Begin    struct{}
	Commit   struct{}
	Rollback struct{}
	Explain  struct{ Stmt Stmt }
	Set      struct{ Name, Value string }
	Show     struct{ Name string }
)

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}
func (*Insert) stmt()      {}
func (*Select) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}
func (*Explain) stmt()     {}
func (*Set) stmt()         {}
func (*Show) stmt()        {}

// Format renders an expression back to SQL. It is used for EXPLAIN output
// and error messages.
func Format(e Expr) string {
	var sb strings.Builder
	format(&sb, e)
	return sb.String()
}

func format(sb *strings.Builder, e Expr) {
	switch x := e.(type) {
	case nil:
		sb.WriteString("NULL")
	case *Literal:
		switch x.Val.K {
		case record.Text:
			sb.WriteString("'" + strings.ReplaceAll(x.Val.S, "'", "''") + "'")
		case record.Bool:
			if x.Val.B {
				sb.WriteString("TRUE")
			} else {
				sb.WriteString("FALSE")
			}
		default:
			sb.WriteString(x.Val.String())
		}
	case *Param:
		sb.WriteString("$")
		sb.WriteString(strconv.Itoa(x.Index))
	case *ColumnRef:
		if x.Table != "" {
			sb.WriteString(x.Table + ".")
		}
		sb.WriteString(x.Column)
	case *Unary:
		if x.Op == "NOT" {
			sb.WriteString("NOT ")
		} else {
			sb.WriteString(x.Op)
		}
		format(sb, x.X)
	case *Binary:
		sb.WriteString("(")
		format(sb, x.L)
		sb.WriteString(" " + x.Op + " ")
		format(sb, x.R)
		sb.WriteString(")")
	case *IsNull:
		format(sb, x.X)
		if x.Not {
			sb.WriteString(" IS NOT NULL")
		} else {
			sb.WriteString(" IS NULL")
		}
	case *Like:
		format(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		if x.Insensitive {
			sb.WriteString(" ILIKE ")
		} else {
			sb.WriteString(" LIKE ")
		}
		format(sb, x.Pattern)
	case *In:
		format(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		sb.WriteString(" IN (")
		for i, a := range x.List {
			if i > 0 {
				sb.WriteString(", ")
			}
			format(sb, a)
		}
		sb.WriteString(")")
	case *Between:
		format(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		sb.WriteString(" BETWEEN ")
		format(sb, x.Lo)
		sb.WriteString(" AND ")
		format(sb, x.Hi)
	case *Call:
		sb.WriteString(x.Name + "(")
		if x.Star {
			sb.WriteString("*")
		}
		if x.Distinct {
			sb.WriteString("DISTINCT ")
		}
		for i, a := range x.Args {
			if i > 0 {
				sb.WriteString(", ")
			}
			format(sb, a)
		}
		sb.WriteString(")")
	case *Case:
		sb.WriteString("CASE")
		if x.Operand != nil {
			sb.WriteString(" ")
			format(sb, x.Operand)
		}
		for _, w := range x.Whens {
			sb.WriteString(" WHEN ")
			format(sb, w.Cond)
			sb.WriteString(" THEN ")
			format(sb, w.Result)
		}
		if x.Else != nil {
			sb.WriteString(" ELSE ")
			format(sb, x.Else)
		}
		sb.WriteString(" END")
	case *Cast:
		sb.WriteString("CAST(")
		format(sb, x.X)
		sb.WriteString(" AS " + x.Type.String() + ")")
	default:
		sb.WriteString("?")
	}
}
