// Package record defines SQL values and how rows and keys are stored as bytes.
//
// Two encodings are used:
//
//   - Row encoding (EncodeRow/DecodeRow) is compact and is what a table's
//     B-tree stores as the value for each row.
//   - Key encoding (AppendKey) is "memcomparable": comparing two encoded
//     keys with bytes.Compare gives the same answer as comparing the values.
//     That lets the B-tree treat keys as plain byte strings.
package record

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is a value's type.
type Kind uint8

const (
	Null Kind = iota
	Int
	Real
	Text
	Bool
)

func (k Kind) String() string {
	switch k {
	case Null:
		return "NULL"
	case Int:
		return "INTEGER"
	case Real:
		return "REAL"
	case Text:
		return "TEXT"
	case Bool:
		return "BOOLEAN"
	}
	return "UNKNOWN"
}

// Value is a single SQL value.
type Value struct {
	K Kind
	I int64
	F float64
	S string
	B bool
}

func NewNull() Value            { return Value{} }
func NewInt(i int64) Value      { return Value{K: Int, I: i} }
func NewReal(f float64) Value   { return Value{K: Real, F: f} }
func NewText(s string) Value    { return Value{K: Text, S: s} }
func NewBool(b bool) Value      { return Value{K: Bool, B: b} }
func (v Value) IsNull() bool    { return v.K == Null }
func (v Value) IsNumeric() bool { return v.K == Int || v.K == Real }

// Float returns a numeric value as float64.
func (v Value) Float() float64 {
	if v.K == Int {
		return float64(v.I)
	}
	return v.F
}

// String renders the value the way PostgreSQL's text format does.
func (v Value) String() string {
	switch v.K {
	case Null:
		return "NULL"
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Real:
		return FormatReal(v.F)
	case Text:
		return v.S
	case Bool:
		if v.B {
			return "t"
		}
		return "f"
	}
	return "?"
}

// FormatReal prints a float with the shortest representation that round-trips.
func FormatReal(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Compare orders two non-null values: numbers (compared numerically, int and
// real mixed) < text < booleans are compared as 0/1 numbers. NULL sorts
// before everything. Returns -1, 0 or 1.
func Compare(a, b Value) int {
	ra, rb := rank(a), rank(b)
	if ra != rb {
		return cmpInt(int64(ra), int64(rb))
	}
	switch ra {
	case 0:
		return 0
	case 1:
		na, nb := numeric(a), numeric(b)
		if na.K == Int && nb.K == Int {
			return cmpInt(na.I, nb.I)
		}
		fa, fb := na.Float(), nb.Float()
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	default:
		return strings.Compare(a.S, b.S)
	}
}

func rank(v Value) int {
	switch v.K {
	case Null:
		return 0
	case Int, Real, Bool:
		return 1
	}
	return 2
}

func numeric(v Value) Value {
	if v.K == Bool {
		if v.B {
			return NewInt(1)
		}
		return NewInt(0)
	}
	return v
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Coerce converts v to the column type k, or reports why it cannot.
// It follows PostgreSQL's assignment rules closely enough for normal use:
// integers widen to reals, integral reals narrow to integers, and quoted
// text is parsed when it holds a valid number or boolean.
func Coerce(v Value, k Kind) (Value, error) {
	if v.K == Null || v.K == k {
		return v, nil
	}
	switch k {
	case Int:
		switch v.K {
		case Real:
			if v.F == math.Trunc(v.F) && v.F >= math.MinInt64 && v.F <= math.MaxInt64 {
				return NewInt(int64(v.F)), nil
			}
			return Value{}, fmt.Errorf("invalid input syntax for type integer: %q", FormatReal(v.F))
		case Text:
			i, err := strconv.ParseInt(strings.TrimSpace(v.S), 10, 64)
			if err != nil {
				return Value{}, fmt.Errorf("invalid input syntax for type integer: %q", v.S)
			}
			return NewInt(i), nil
		case Bool:
			if v.B {
				return NewInt(1), nil
			}
			return NewInt(0), nil
		}
	case Real:
		switch v.K {
		case Int:
			return NewReal(float64(v.I)), nil
		case Text:
			f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
			if err != nil {
				return Value{}, fmt.Errorf("invalid input syntax for type real: %q", v.S)
			}
			return NewReal(f), nil
		}
	case Text:
		return NewText(v.String()), nil
	case Bool:
		switch v.K {
		case Int:
			return NewBool(v.I != 0), nil
		case Text:
			switch strings.ToLower(strings.TrimSpace(v.S)) {
			case "t", "true", "yes", "on", "1":
				return NewBool(true), nil
			case "f", "false", "no", "off", "0":
				return NewBool(false), nil
			}
			return Value{}, fmt.Errorf("invalid input syntax for type boolean: %q", v.S)
		}
	}
	return Value{}, fmt.Errorf("cannot store %s in a %s column", v.K, k)
}

// --- row encoding ----------------------------------------------------------

// EncodeRow appends the compact encoding of vals to dst.
func EncodeRow(dst []byte, vals []Value) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(vals)))
	for _, v := range vals {
		dst = append(dst, byte(v.K))
		switch v.K {
		case Int:
			dst = binary.AppendVarint(dst, v.I)
		case Real:
			dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.F))
		case Text:
			dst = binary.AppendUvarint(dst, uint64(len(v.S)))
			dst = append(dst, v.S...)
		case Bool:
			if v.B {
				dst = append(dst, 1)
			} else {
				dst = append(dst, 0)
			}
		}
	}
	return dst
}

var errCorrupt = errors.New("corrupt row encoding")

// DecodeRow decodes a row produced by EncodeRow.
func DecodeRow(b []byte) ([]Value, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return nil, errCorrupt
	}
	b = b[k:]
	vals := make([]Value, n)
	for i := range vals {
		if len(b) == 0 {
			return nil, errCorrupt
		}
		kind := Kind(b[0])
		b = b[1:]
		switch kind {
		case Null:
		case Int:
			x, k := binary.Varint(b)
			if k <= 0 {
				return nil, errCorrupt
			}
			vals[i] = NewInt(x)
			b = b[k:]
		case Real:
			if len(b) < 8 {
				return nil, errCorrupt
			}
			vals[i] = NewReal(math.Float64frombits(binary.LittleEndian.Uint64(b)))
			b = b[8:]
		case Text:
			l, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < l {
				return nil, errCorrupt
			}
			vals[i] = NewText(string(b[k : k+int(l)]))
			b = b[k+int(l):]
		case Bool:
			if len(b) < 1 {
				return nil, errCorrupt
			}
			vals[i] = NewBool(b[0] == 1)
			b = b[1:]
		default:
			return nil, errCorrupt
		}
	}
	return vals, nil
}

// --- key encoding ----------------------------------------------------------

const (
	tagNull  = 0x05
	tagFalse = 0x10
	tagTrue  = 0x11
	tagNum   = 0x20
	tagText  = 0x30
)

// AppendKey appends the memcomparable encoding of v. All numbers (int and
// real) share one encoding so mixed comparisons sort correctly: the value
// is stored as a float64 with the sign trick, followed by the exact int64
// to break ties between integers too large for a float to tell apart.
func AppendKey(dst []byte, v Value) []byte {
	switch v.K {
	case Null:
		return append(dst, tagNull)
	case Bool:
		if v.B {
			return append(dst, tagTrue)
		}
		return append(dst, tagFalse)
	case Int, Real:
		dst = append(dst, tagNum)
		f := v.Float()
		bits := math.Float64bits(f)
		if f >= 0 {
			bits ^= 1 << 63
		} else {
			bits = ^bits
		}
		dst = binary.BigEndian.AppendUint64(dst, bits)
		// Tie-breaker: exact integer part for ints, or the truncated float.
		var exact int64
		if v.K == Int {
			exact = v.I
		} else if f >= math.MinInt64 && f < math.MaxInt64 {
			exact = int64(f)
		} else if f > 0 {
			exact = math.MaxInt64
		} else {
			exact = math.MinInt64
		}
		return binary.BigEndian.AppendUint64(dst, uint64(exact)^(1<<63))
	case Text:
		dst = append(dst, tagText)
		for i := 0; i < len(v.S); i++ {
			c := v.S[i]
			if c == 0x00 {
				dst = append(dst, 0x00, 0xFF)
			} else {
				dst = append(dst, c)
			}
		}
		return append(dst, 0x00, 0x01)
	}
	return dst
}

// EncodeKey encodes a tuple of values as one key.
func EncodeKey(vals ...Value) []byte {
	var b []byte
	for _, v := range vals {
		b = AppendKey(b, v)
	}
	return b
}

// DecodeKey decodes a key produced by EncodeKey. Numbers come back as the
// kind hint given for each position (Int or Real); pass nil to get ints
// for integral values.
func DecodeKey(b []byte, kinds []Kind) ([]Value, error) {
	var out []Value
	for i := 0; len(b) > 0; i++ {
		tag := b[0]
		b = b[1:]
		switch tag {
		case tagNull:
			out = append(out, Value{})
		case tagFalse:
			out = append(out, NewBool(false))
		case tagTrue:
			out = append(out, NewBool(true))
		case tagNum:
			if len(b) < 16 {
				return nil, errCorrupt
			}
			bits := binary.BigEndian.Uint64(b)
			if bits&(1<<63) != 0 {
				bits ^= 1 << 63
			} else {
				bits = ^bits
			}
			f := math.Float64frombits(bits)
			exact := int64(binary.BigEndian.Uint64(b[8:]) ^ (1 << 63))
			b = b[16:]
			if i < len(kinds) && kinds[i] == Real {
				out = append(out, NewReal(f))
			} else {
				out = append(out, NewInt(exact))
			}
		case tagText:
			var sb strings.Builder
			for {
				if len(b) < 2 && (len(b) == 0 || b[0] == 0) {
					return nil, errCorrupt
				}
				if b[0] == 0x00 {
					if b[1] == 0x01 {
						b = b[2:]
						break
					}
					sb.WriteByte(0)
					b = b[2:]
					continue
				}
				sb.WriteByte(b[0])
				b = b[1:]
			}
			out = append(out, NewText(sb.String()))
		default:
			return nil, errCorrupt
		}
	}
	return out, nil
}

// SkipKey returns the length of the first n encoded values at the start of
// an encoded key. Index entries use it to find where the table key begins.
func SkipKey(b []byte, n int) (int, error) {
	off := 0
	for i := 0; i < n; i++ {
		if off >= len(b) {
			return 0, errCorrupt
		}
		switch b[off] {
		case tagNull, tagFalse, tagTrue:
			off++
		case tagNum:
			off += 17
		case tagText:
			off++
			for {
				if off+1 >= len(b) {
					return 0, errCorrupt
				}
				if b[off] == 0x00 {
					if b[off+1] == 0x01 {
						off += 2
						break
					}
					off += 2
					continue
				}
				off++
			}
		default:
			return 0, errCorrupt
		}
	}
	if off > len(b) {
		return 0, errCorrupt
	}
	return off, nil
}
