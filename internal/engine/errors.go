package engine

import (
	"errors"
	"fmt"

	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// Error is a SQL error with a PostgreSQL SQLSTATE code, so client drivers can
// react to it the same way they would with PostgreSQL.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// SQLSTATE codes used by OakDB.
const (
	CodeSyntax             = "42601"
	CodeUndefinedTable     = "42P01"
	CodeUndefinedColumn    = "42703"
	CodeUndefinedObject    = "42704"
	CodeUndefinedFunction  = "42883"
	CodeAmbiguousColumn    = "42702"
	CodeDuplicateTable     = "42P07"
	CodeDuplicateObject    = "42710"
	CodeDuplicateColumn    = "42701"
	CodeGrouping           = "42803"
	CodeDatatypeMismatch   = "42804"
	CodeUniqueViolation    = "23505"
	CodeNotNullViolation   = "23502"
	CodeDivisionByZero     = "22012"
	CodeNumericRange       = "22003"
	CodeInvalidText        = "22P02"
	CodeSerialization      = "40001"
	CodeInFailedTxn        = "25P02"
	CodeActiveTxn          = "25001"
	CodeNoActiveTxn        = "25P01"
	CodeFeatureUnsupported = "0A000"
	CodeInvalidParameter   = "22023"
	CodeInternal           = "XX000"
)

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// AsError converts any error into an *Error with a sensible code.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var se *sql.Error
	if errors.As(err, &se) {
		return &Error{Code: CodeSyntax, Msg: se.Error()}
	}
	if errors.Is(err, pager.ErrSerialization) {
		return &Error{Code: CodeSerialization, Msg: err.Error()}
	}
	return &Error{Code: CodeInternal, Msg: err.Error()}
}
