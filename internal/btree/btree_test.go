package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
)

// memPages is an in-memory Writer that also tracks which pages are in use,
// so tests can catch leaked or double-freed pages.
type memPages struct {
	pages map[uint32][]byte
	free  []uint32
	next  uint32
	inUse map[uint32]bool
}

func newMem() *memPages {
	return &memPages{pages: map[uint32][]byte{}, next: 1, inUse: map[uint32]bool{}}
}

func (m *memPages) Page(n uint32) ([]byte, error) {
	p, ok := m.pages[n]
	if !ok {
		return nil, fmt.Errorf("page %d does not exist", n)
	}
	return p, nil
}
func (m *memPages) Mutable(n uint32) ([]byte, error) { return m.Page(n) }
func (m *memPages) Alloc() (uint32, error) {
	var n uint32
	if len(m.free) > 0 {
		n, m.free = m.free[len(m.free)-1], m.free[:len(m.free)-1]
	} else {
		n = m.next
		m.next++
	}
	m.pages[n] = make([]byte, pager.PageSize)
	m.inUse[n] = true
	return n, nil
}
func (m *memPages) Free(n uint32) error {
	if !m.inUse[n] {
		return fmt.Errorf("double free of page %d", n)
	}
	delete(m.inUse, n)
	m.free = append(m.free, n)
	return nil
}

func key(i int) []byte { return binary.BigEndian.AppendUint64(nil, uint64(i)) }

func checkTree(t *testing.T, r Reader, root uint32, model map[string][]byte) {
	t.Helper()
	st, err := Inspect(r, root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != len(model) {
		t.Fatalf("tree has %d entries, model has %d", st.Entries, len(model))
	}
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := NewCursor(r, root)
	c.First()
	for _, k := range keys {
		if !c.Valid() {
			t.Fatalf("cursor ended early, missing %x", k)
		}
		if string(c.Key()) != k {
			t.Fatalf("cursor at %x, want %x", c.Key(), k)
		}
		v, err := c.Value()
		if err != nil || !bytes.Equal(v, model[k]) {
			t.Fatalf("value for %x: %v (len %d), want len %d", k, err, len(v), len(model[k]))
		}
		c.Next()
	}
	if c.Valid() {
		t.Fatalf("cursor has extra key %x", c.Key())
	}
}

// TestRandomOperationsAgainstMap runs thousands of random inserts, updates
// and deletes, comparing the tree with a Go map and checking every
// structural invariant along the way.
func TestRandomOperationsAgainstMap(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewSource(seed))
		m := newMem()
		root, _ := Create(m)
		model := map[string][]byte{}
		for op := 0; op < 6000; op++ {
			k := key(r.Intn(2500))
			switch r.Intn(10) {
			case 0, 1, 2, 3, 4:
				size := r.Intn(60)
				if r.Intn(30) == 0 {
					size = 700 + r.Intn(10000) // overflow pages
				}
				v := make([]byte, size)
				r.Read(v)
				if err := Insert(m, root, k, v, true); err != nil {
					t.Fatal(err)
				}
				model[string(k)] = v
			case 5:
				err := Insert(m, root, k, []byte("x"), false)
				_, exists := model[string(k)]
				if exists && err != ErrExists || !exists && err != nil {
					t.Fatalf("insert-if-absent: exists=%v err=%v", exists, err)
				}
				if !exists {
					model[string(k)] = []byte("x")
				}
			default:
				found, err := Delete(m, root, k)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := model[string(k)]; ok != found {
					t.Fatalf("delete found=%v, model has=%v", found, ok)
				}
				delete(model, string(k))
			}
			if op%500 == 0 {
				checkTree(t, m, root, model)
			}
		}
		checkTree(t, m, root, model)
		for k := range model {
			v, ok, err := Get(m, root, []byte(k))
			if err != nil || !ok || !bytes.Equal(v, model[k]) {
				t.Fatalf("Get(%x) = %v %v", k, ok, err)
			}
		}
	}
}

func TestDeleteEverythingFreesPages(t *testing.T) {
	m := newMem()
	root, _ := Create(m)
	for _, i := range rand.New(rand.NewSource(1)).Perm(20000) {
		if err := Insert(m, root, key(i), bytes.Repeat([]byte{byte(i)}, 40), false); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := Inspect(m, root)
	if st.Depth < 3 {
		t.Fatalf("expected a deep tree, got depth %d", st.Depth)
	}
	for i := 0; i < 20000; i++ {
		if found, err := Delete(m, root, key(i)); err != nil || !found {
			t.Fatalf("delete %d: %v %v", i, found, err)
		}
	}
	checkTree(t, m, root, map[string][]byte{})
	if len(m.inUse) != 1 {
		t.Fatalf("%d pages still in use after deleting everything, want 1 (the root)", len(m.inUse))
	}
	if err := Drop(m, root); err != nil {
		t.Fatal(err)
	}
	if len(m.inUse) != 0 {
		t.Fatalf("pages leaked after drop: %d", len(m.inUse))
	}
}

func TestSeekAndLast(t *testing.T) {
	m := newMem()
	root, _ := Create(m)
	for i := 0; i < 5000; i += 2 {
		Insert(m, root, key(i), nil, false)
	}
	c := NewCursor(m, root)
	c.Seek(key(1001))
	if !c.Valid() || !bytes.Equal(c.Key(), key(1002)) {
		t.Fatalf("seek landed on %x", c.Key())
	}
	c.Seek(key(99999))
	if c.Valid() {
		t.Fatal("seek past the end should be invalid")
	}
	c.Last()
	if !bytes.Equal(c.Key(), key(4998)) {
		t.Fatalf("last = %x", c.Key())
	}
}

func TestVariableKeySizes(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	m := newMem()
	root, _ := Create(m)
	model := map[string][]byte{}
	for i := 0; i < 3000; i++ {
		k := make([]byte, 1+r.Intn(MaxKeySize))
		r.Read(k)
		v := make([]byte, r.Intn(maxInline+1))
		if err := Insert(m, root, k, v, true); err != nil {
			t.Fatal(err)
		}
		model[string(k)] = v
		if i%3 == 0 {
			for dk := range model {
				Delete(m, root, []byte(dk))
				delete(model, dk)
				break
			}
		}
	}
	checkTree(t, m, root, model)
	if err := Insert(m, root, make([]byte, MaxKeySize+1), nil, false); err != ErrKeyTooLarge {
		t.Fatalf("oversized key: %v", err)
	}
}

// TestWithRealPager runs the tree on the real pager across commits and a
// reopen, to check the two layers work together.
func TestWithRealPager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p, err := pager.Open(path, pager.Options{NoSync: true, CheckpointFrames: 50, CachePages: 16})
	if err != nil {
		t.Fatal(err)
	}
	tx := p.BeginWrite()
	root, _ := Create(tx)
	tx.Commit()
	model := map[string][]byte{}
	r := rand.New(rand.NewSource(5))
	for batch := 0; batch < 40; batch++ {
		tx := p.BeginWrite()
		for i := 0; i < 100; i++ {
			k := key(r.Intn(3000))
			if r.Intn(4) == 0 {
				Delete(tx, root, k)
				delete(model, string(k))
			} else {
				v := []byte(fmt.Sprint(batch, i))
				Insert(tx, root, k, v, true)
				model[string(k)] = v
			}
		}
		if batch%7 == 3 {
			tx.Rollback()
			// Rebuild the model from the committed state.
			s := p.BeginRead()
			model = map[string][]byte{}
			c := NewCursor(s, root)
			for c.First(); c.Valid(); c.Next() {
				v, _ := c.Value()
				model[string(bytes.Clone(c.Key()))] = bytes.Clone(v)
			}
			s.Release()
			continue
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	p, err = pager.Open(path, pager.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	s := p.BeginRead()
	defer s.Release()
	checkTree(t, s, root, model)
}

func FuzzTree(f *testing.F) {
	f.Add([]byte{1, 2, 3, 200, 5, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		m := newMem()
		root, _ := Create(m)
		model := map[string][]byte{}
		for i := 0; i+1 < len(ops); i += 2 {
			k := key(int(ops[i+1]) * 7)
			if ops[i]%3 == 0 {
				Delete(m, root, k)
				delete(model, string(k))
			} else {
				v := bytes.Repeat([]byte{ops[i]}, int(ops[i])*4)
				Insert(m, root, k, v, true)
				model[string(k)] = v
			}
		}
		checkTree(t, m, root, model)
	})
}

// Sequential inserts should pack leaves nearly full (the append split).
func TestAppendPacksLeaves(t *testing.T) {
	m := newMem()
	root, _ := Create(m)
	for i := 0; i < 20000; i++ {
		Insert(m, root, key(i), bytes.Repeat([]byte{1}, 40), false)
	}
	st, err := Inspect(m, root)
	if err != nil {
		t.Fatal(err)
	}
	perLeaf := float64(st.Entries) / float64(st.Leaves)
	// A 4 KB page holds about 77 of these cells.
	if perLeaf < 70 {
		t.Fatalf("leaves are only %.0f entries full on average", perLeaf)
	}
}
