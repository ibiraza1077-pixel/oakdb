// Package catalog stores table and index definitions inside the database
// itself, in a B-tree whose root page number lives in the file header.
// Because the catalog is ordinary pages, schema changes are transactional:
// CREATE TABLE inside a transaction that rolls back never happened.
package catalog

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ibiraza1077-pixel/oakdb/internal/btree"
	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// Column describes one table column.
type Column struct {
	Name    string      `json:"name"`
	Type    record.Kind `json:"type"`
	NotNull bool        `json:"not_null,omitempty"`
}

// Table describes a table. Rows are stored in a B-tree keyed by the primary
// key, or by a hidden auto-incrementing row id when there is none.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	PK      int      `json:"pk"` // index of the primary key column, or -1
	Root    uint32   `json:"root"`

	Indexes []*Index `json:"-"`
}

// Index describes a secondary index. Each entry's key is the encoded indexed
// values followed by the row's table key, so entries are unique even when
// the indexed values repeat.
type Index struct {
	Name    string `json:"name"`
	Table   string `json:"table"`
	Columns []int  `json:"columns"`
	Unique  bool   `json:"unique,omitempty"`
	Root    uint32 `json:"root"`
}

// Catalog is an in-memory snapshot of the schema.
type Catalog struct {
	Tables  map[string]*Table
	Indexes map[string]*Index
}

// ColumnIndex returns the position of a column, or -1.
func (t *Table) ColumnIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// Load reads the catalog visible to r. root 0 means an empty database.
func Load(r btree.Reader, root uint32) (*Catalog, error) {
	c := &Catalog{Tables: map[string]*Table{}, Indexes: map[string]*Index{}}
	if root == 0 {
		return c, nil
	}
	cur := btree.NewCursor(r, root)
	for cur.First(); cur.Valid(); cur.Next() {
		k := string(cur.Key())
		v, err := cur.Value()
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(k, "table:"):
			t := &Table{}
			if err := json.Unmarshal(v, t); err != nil {
				return nil, fmt.Errorf("catalog entry %s: %w", k, err)
			}
			c.Tables[t.Name] = t
		case strings.HasPrefix(k, "index:"):
			ix := &Index{}
			if err := json.Unmarshal(v, ix); err != nil {
				return nil, fmt.Errorf("catalog entry %s: %w", k, err)
			}
			c.Indexes[ix.Name] = ix
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	for _, ix := range c.Indexes {
		if t := c.Tables[ix.Table]; t != nil {
			t.Indexes = append(t.Indexes, ix)
		}
	}
	for _, t := range c.Tables {
		sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })
	}
	return c, nil
}

// Writer is the subset of a write transaction the catalog needs.
type Writer interface {
	btree.Writer
	CatalogRoot() (uint32, error)
	SetCatalogRoot(uint32) error
	SetSchemaToken(uint64) error
}

func root(w Writer) (uint32, error) {
	r, err := w.CatalogRoot()
	if err != nil || r != 0 {
		return r, err
	}
	if r, err = btree.Create(w); err != nil {
		return 0, err
	}
	return r, w.SetCatalogRoot(r)
}

// touch gives the schema a new random version token so every cached copy
// of the old catalog is recognised as stale.
func touch(w Writer) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	return w.SetSchemaToken(binary.LittleEndian.Uint64(b[:]))
}

func put(w Writer, key string, v any) error {
	r, err := root(w)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := btree.Insert(w, r, []byte(key), data, true); err != nil {
		return err
	}
	return touch(w)
}

func del(w Writer, key string) error {
	r, err := root(w)
	if err != nil {
		return err
	}
	if _, err := btree.Delete(w, r, []byte(key)); err != nil {
		return err
	}
	return touch(w)
}

// PutTable creates or replaces a table definition.
func PutTable(w Writer, t *Table) error { return put(w, "table:"+t.Name, t) }

// DeleteTable removes a table definition.
func DeleteTable(w Writer, name string) error { return del(w, "table:"+name) }

// PutIndex creates or replaces an index definition.
func PutIndex(w Writer, ix *Index) error { return put(w, "index:"+ix.Name, ix) }

// DeleteIndex removes an index definition.
func DeleteIndex(w Writer, name string) error { return del(w, "index:"+name) }

// Token reads the schema version token from the header.
func Token(r btree.Reader) (uint64, uint32, error) {
	p, err := r.Page(0)
	if err != nil {
		return 0, 0, err
	}
	m := pager.ReadMeta(p)
	return m.SchemaToken, m.CatalogRoot, nil
}
