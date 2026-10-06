package record

import (
	"bytes"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func randValue(r *rand.Rand, k Kind) Value {
	switch k {
	case Int:
		switch r.Intn(4) {
		case 0:
			return NewInt(r.Int63() - r.Int63())
		case 1:
			return NewInt(math.MaxInt64 - int64(r.Intn(3)))
		default:
			return NewInt(int64(r.Intn(200) - 100))
		}
	case Real:
		return NewReal((r.Float64() - 0.5) * math.Pow(10, float64(r.Intn(12))))
	case Text:
		b := make([]byte, r.Intn(6))
		for i := range b {
			b[i] = "\x00ab\xffz"[r.Intn(5)]
		}
		return NewText(string(b))
	case Bool:
		return NewBool(r.Intn(2) == 0)
	}
	return NewNull()
}

// The core property: byte order of encoded keys equals value order.
func TestKeyOrderMatchesCompare(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, kinds := range [][]Kind{{Int}, {Real}, {Text}, {Int, Real}, {Bool}} {
		for i := 0; i < 20000; i++ {
			a := randValue(r, kinds[r.Intn(len(kinds))])
			b := randValue(r, kinds[r.Intn(len(kinds))])
			if r.Intn(10) == 0 {
				a = NewNull()
			}
			want := Compare(a, b)
			got := bytes.Compare(EncodeKey(a), EncodeKey(b))
			if got != want {
				t.Fatalf("order mismatch for %#v vs %#v: key %d, compare %d", a, b, got, want)
			}
		}
	}
}

func TestTupleKeyPrefixOrder(t *testing.T) {
	// ("a", 2) < ("ab", 1): the text terminator must sort below any byte.
	k1 := EncodeKey(NewText("a"), NewInt(2))
	k2 := EncodeKey(NewText("ab"), NewInt(1))
	if bytes.Compare(k1, k2) >= 0 {
		t.Fatal("tuple ordering is wrong")
	}
}

func TestRowAndKeyRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 2000; i++ {
		var row []Value
		var kinds []Kind
		for j := 0; j < r.Intn(6); j++ {
			k := Kind(r.Intn(5))
			row = append(row, randValue(r, k))
			kinds = append(kinds, k)
		}
		got, err := DecodeRow(EncodeRow(nil, row))
		if err != nil {
			t.Fatal(err)
		}
		if len(row) == 0 {
			row = []Value{}
		}
		if !reflect.DeepEqual(got, row) {
			t.Fatalf("row round trip: got %v want %v", got, row)
		}
		keyVals, err := DecodeKey(EncodeKey(row...), kinds)
		if err != nil {
			t.Fatal(err)
		}
		for j := range row {
			if Compare(keyVals[j], row[j]) != 0 {
				t.Fatalf("key round trip: got %v want %v", keyVals[j], row[j])
			}
		}
	}
}

func TestCoerce(t *testing.T) {
	cases := []struct {
		in   Value
		to   Kind
		want Value
		err  bool
	}{
		{NewInt(3), Real, NewReal(3), false},
		{NewReal(3), Int, NewInt(3), false},
		{NewReal(3.5), Int, Value{}, true},
		{NewText(" 12 "), Int, NewInt(12), false},
		{NewText("x"), Int, Value{}, true},
		{NewText("true"), Bool, NewBool(true), false},
		{NewInt(5), Text, NewText("5"), false},
	}
	for _, c := range cases {
		got, err := Coerce(c.in, c.to)
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("Coerce(%v, %v) = %v, %v", c.in, c.to, got, err)
		}
	}
}
