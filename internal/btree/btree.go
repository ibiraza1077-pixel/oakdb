// Package btree implements a disk-based B+tree over pager pages.
//
// Keys and values are byte strings; keys are compared with bytes.Compare.
// Every table and every index in OakDB is one of these trees.
//
// Page layout (slotted page):
//
//	[0]      node type: 1 = leaf, 2 = internal
//	[1:3]    number of cells (uint16)
//	[3:7]    leaf: page number of the next leaf (0 = none)
//	         internal: page number of the rightmost child
//	[7:7+2n] cell offsets (uint16 each), in key order
//	...      cells
//
//	leaf cell:     uvarint keyLen | key | flag (0 inline, 1 overflow) |
//	               inline:   uvarint valLen | value
//	               overflow: uint32 totalLen | uint32 firstOverflowPage
//	internal cell: uvarint keyLen | key | uint32 child
//
// An internal node with keys k0..k(n-1) has children c0..cn. Child ci holds
// keys in [k(i-1), ki). Leaves are linked left to right so range scans walk
// along the bottom of the tree without going back up.
//
// The root page number never changes: when the root splits, its contents
// move to a new page and the root becomes the new parent. Callers can keep
// the root page number in the catalog forever.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/ibiraza1077-pixel/oakdb/internal/pager"
)

const (
	typeLeaf     = 1
	typeInternal = 2
	headerSize   = 7

	// MaxKeySize is the largest key the tree accepts.
	MaxKeySize = 512
	// maxInline is the largest value stored inside a leaf cell; larger
	// values go to a chain of overflow pages. These limits keep every cell
	// under a third of a page, which guarantees that any overfull node can
	// be split into two halves that both fit.
	maxInline = 700
	// minFill is the size below which a node is merged or rebalanced.
	minFill = pager.PageSize / 4

	overflowHeader = 6
	overflowData   = pager.PageSize - overflowHeader
)

// ErrExists is returned by Insert when the key is present and replace is false.
var ErrExists = errors.New("key already exists")

// ErrKeyTooLarge is returned for keys longer than MaxKeySize.
var ErrKeyTooLarge = fmt.Errorf("key larger than %d bytes", MaxKeySize)

// Reader reads pages.
type Reader interface {
	Page(n uint32) ([]byte, error)
}

// Writer reads and modifies pages inside a write transaction.
type Writer interface {
	Reader
	Mutable(n uint32) ([]byte, error)
	Alloc() (uint32, error)
	Free(n uint32) error
}

// --- raw page accessors (no allocation; used on the read path) ------------

func isLeaf(p []byte) bool    { return p[0] == typeLeaf }
func nCells(p []byte) int     { return int(binary.LittleEndian.Uint16(p[1:3])) }
func link(p []byte) uint32    { return binary.LittleEndian.Uint32(p[3:7]) }
func cellOff(p []byte, i int) int {
	return int(binary.LittleEndian.Uint16(p[headerSize+2*i:]))
}

// cellKey returns the key of cell i and the offset just past it.
func cellKey(p []byte, i int) ([]byte, int) {
	off := cellOff(p, i)
	l, n := binary.Uvarint(p[off:])
	start := off + n
	return p[start : start+int(l)], start + int(l)
}

func childAt(p []byte, i int) uint32 {
	if i == nCells(p) {
		return link(p)
	}
	_, end := cellKey(p, i)
	return binary.LittleEndian.Uint32(p[end:])
}

// leafValue decodes the value part of leaf cell i.
func leafValue(p []byte, i int) leafVal {
	_, end := cellKey(p, i)
	if p[end] == 0 {
		l, n := binary.Uvarint(p[end+1:])
		s := end + 1 + n
		return leafVal{inline: p[s : s+int(l)]}
	}
	return leafVal{
		overflow: true,
		total:    binary.LittleEndian.Uint32(p[end+1:]),
		first:    binary.LittleEndian.Uint32(p[end+5:]),
	}
}

// childIndex returns which child of an internal node covers key.
func childIndex(p []byte, key []byte) int {
	n := nCells(p)
	return sort.Search(n, func(i int) bool {
		k, _ := cellKey(p, i)
		return bytes.Compare(key, k) < 0
	})
}

// lowerBound returns the first cell in a leaf whose key is >= key.
func lowerBound(p []byte, key []byte) int {
	return sort.Search(nCells(p), func(i int) bool {
		k, _ := cellKey(p, i)
		return bytes.Compare(k, key) >= 0
	})
}

// --- decoded nodes (used on the write path) --------------------------------

type leafVal struct {
	inline   []byte
	overflow bool
	total    uint32
	first    uint32
}

type node struct {
	leaf     bool
	keys     [][]byte
	vals     []leafVal // leaf only
	children []uint32  // internal only; len(keys)+1
	next     uint32    // leaf only
}

func decode(p []byte) *node {
	n := nCells(p)
	nd := &node{leaf: isLeaf(p), keys: make([][]byte, n)}
	for i := 0; i < n; i++ {
		k, _ := cellKey(p, i)
		nd.keys[i] = bytes.Clone(k)
	}
	if nd.leaf {
		nd.next = link(p)
		nd.vals = make([]leafVal, n)
		for i := 0; i < n; i++ {
			v := leafValue(p, i)
			v.inline = bytes.Clone(v.inline)
			nd.vals[i] = v
		}
	} else {
		nd.children = make([]uint32, n+1)
		for i := 0; i <= n; i++ {
			nd.children[i] = childAt(p, i)
		}
	}
	return nd
}

func (nd *node) cellSize(i int) int {
	k := len(nd.keys[i])
	s := uvarintLen(uint64(k)) + k
	if !nd.leaf {
		return s + 4
	}
	v := nd.vals[i]
	if v.overflow {
		return s + 1 + 8
	}
	return s + 1 + uvarintLen(uint64(len(v.inline))) + len(v.inline)
}

func (nd *node) size() int {
	s := headerSize
	for i := range nd.keys {
		s += 2 + nd.cellSize(i)
	}
	return s
}

func (nd *node) encode() []byte {
	p := make([]byte, pager.PageSize)
	if nd.leaf {
		p[0] = typeLeaf
		binary.LittleEndian.PutUint32(p[3:], nd.next)
	} else {
		p[0] = typeInternal
		binary.LittleEndian.PutUint32(p[3:], nd.children[len(nd.keys)])
	}
	binary.LittleEndian.PutUint16(p[1:], uint16(len(nd.keys)))
	off := headerSize + 2*len(nd.keys)
	for i, k := range nd.keys {
		binary.LittleEndian.PutUint16(p[headerSize+2*i:], uint16(off))
		off += binary.PutUvarint(p[off:], uint64(len(k)))
		off += copy(p[off:], k)
		if !nd.leaf {
			binary.LittleEndian.PutUint32(p[off:], nd.children[i])
			off += 4
			continue
		}
		v := nd.vals[i]
		if v.overflow {
			p[off] = 1
			binary.LittleEndian.PutUint32(p[off+1:], v.total)
			binary.LittleEndian.PutUint32(p[off+5:], v.first)
			off += 9
		} else {
			p[off] = 0
			off++
			off += binary.PutUvarint(p[off:], uint64(len(v.inline)))
			off += copy(p[off:], v.inline)
		}
	}
	return p
}

func uvarintLen(x uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], x)
}

func writeNode(w Writer, pageNo uint32, nd *node) error {
	if nd.size() > pager.PageSize {
		return errors.New("btree: node does not fit in a page")
	}
	p, err := w.Mutable(pageNo)
	if err != nil {
		return err
	}
	copy(p, nd.encode())
	return nil
}

func readNode(r Reader, pageNo uint32) (*node, error) {
	p, err := r.Page(pageNo)
	if err != nil {
		return nil, err
	}
	if p[0] != typeLeaf && p[0] != typeInternal {
		return nil, fmt.Errorf("btree: page %d is not a tree node", pageNo)
	}
	return decode(p), nil
}

// --- public API ------------------------------------------------------------

// Create allocates an empty tree and returns its root page.
func Create(w Writer) (uint32, error) {
	n, err := w.Alloc()
	if err != nil {
		return 0, err
	}
	return n, writeNode(w, n, &node{leaf: true})
}

// Get looks up key.
func Get(r Reader, root uint32, key []byte) ([]byte, bool, error) {
	p, err := r.Page(root)
	if err != nil {
		return nil, false, err
	}
	for !isLeaf(p) {
		if p, err = r.Page(childAt(p, childIndex(p, key))); err != nil {
			return nil, false, err
		}
	}
	i := lowerBound(p, key)
	if i == nCells(p) {
		return nil, false, nil
	}
	if k, _ := cellKey(p, i); !bytes.Equal(k, key) {
		return nil, false, nil
	}
	v, err := readValue(r, leafValue(p, i))
	return v, err == nil, err
}

// Insert adds key with val. If the key exists, it is replaced when replace is
// true and ErrExists is returned otherwise.
func Insert(w Writer, root uint32, key, val []byte, replace bool) error {
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	split, err := insert(w, root, key, val, replace)
	if err != nil || split == nil {
		return err
	}
	// The root split. Move its left half to a fresh page and turn the root
	// into a parent of the two halves, so the root page number is stable.
	left, err := w.Alloc()
	if err != nil {
		return err
	}
	src, err := w.Page(root)
	if err != nil {
		return err
	}
	dst, err := w.Mutable(left)
	if err != nil {
		return err
	}
	copy(dst, src)
	return writeNode(w, root, &node{keys: [][]byte{split.sep}, children: []uint32{left, split.right}})
}

type splitResult struct {
	sep   []byte // smallest key in right
	right uint32
}

func insert(w Writer, pageNo uint32, key, val []byte, replace bool) (*splitResult, error) {
	nd, err := readNode(w, pageNo)
	if err != nil {
		return nil, err
	}
	if !nd.leaf {
		idx := sort.Search(len(nd.keys), func(i int) bool { return bytes.Compare(key, nd.keys[i]) < 0 })
		res, err := insert(w, nd.children[idx], key, val, replace)
		if err != nil || res == nil {
			return nil, err
		}
		nd.keys = insertAt(nd.keys, idx, res.sep)
		nd.children = insertAt(nd.children, idx+1, res.right)
		return writeOrSplit(w, pageNo, nd)
	}
	i := sort.Search(len(nd.keys), func(i int) bool { return bytes.Compare(nd.keys[i], key) >= 0 })
	lv, err := makeValue(w, val)
	if err != nil {
		return nil, err
	}
	if i < len(nd.keys) && bytes.Equal(nd.keys[i], key) {
		if !replace {
			if lv.overflow {
				freeOverflow(w, lv.first)
			}
			return nil, ErrExists
		}
		if old := nd.vals[i]; old.overflow {
			if err := freeOverflow(w, old.first); err != nil {
				return nil, err
			}
		}
		nd.vals[i] = lv
	} else {
		nd.keys = insertAt(nd.keys, i, bytes.Clone(key))
		nd.vals = insertAt(nd.vals, i, lv)
	}
	return writeOrSplit(w, pageNo, nd)
}

func insertAt[T any](s []T, i int, v T) []T {
	s = append(s, v)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAt[T any](s []T, i int) []T {
	return append(s[:i], s[i+1:]...)
}

// splitPoint picks the cell index that balances the bytes in the two halves.
func (nd *node) splitPoint() int {
	total := nd.size() - headerSize
	acc, best, bestDiff := 0, 1, total
	for i := 0; i < len(nd.keys)-1; i++ {
		acc += 2 + nd.cellSize(i)
		diff := total - 2*acc
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff = i+1, diff
		}
	}
	return best
}

// halves splits a node into two. For internal nodes the middle key moves up
// and is returned as the separator.
func (nd *node) halves() (left, right *node, sep []byte) {
	m := nd.splitPoint()
	if nd.leaf {
		left = &node{leaf: true, keys: nd.keys[:m:m], vals: nd.vals[:m:m]}
		right = &node{leaf: true, keys: nd.keys[m:], vals: nd.vals[m:], next: nd.next}
		return left, right, bytes.Clone(right.keys[0])
	}
	if m >= len(nd.keys) {
		m = len(nd.keys) - 1
	}
	left = &node{keys: nd.keys[:m:m], children: nd.children[: m+1 : m+1]}
	right = &node{keys: nd.keys[m+1:], children: nd.children[m+1:]}
	return left, right, nd.keys[m]
}

func writeOrSplit(w Writer, pageNo uint32, nd *node) (*splitResult, error) {
	if nd.size() <= pager.PageSize {
		return nil, writeNode(w, pageNo, nd)
	}
	left, right, sep := nd.halves()
	rightPage, err := w.Alloc()
	if err != nil {
		return nil, err
	}
	if left.leaf {
		left.next = rightPage
	}
	if err := writeNode(w, pageNo, left); err != nil {
		return nil, err
	}
	if err := writeNode(w, rightPage, right); err != nil {
		return nil, err
	}
	return &splitResult{sep: sep, right: rightPage}, nil
}

// Delete removes key and reports whether it was present.
func Delete(w Writer, root uint32, key []byte) (bool, error) {
	found, _, err := del(w, root, key)
	if err != nil || !found {
		return found, err
	}
	// Collapse a root that has a single child.
	nd, err := readNode(w, root)
	if err != nil {
		return true, err
	}
	if !nd.leaf && len(nd.keys) == 0 {
		child := nd.children[0]
		src, err := w.Page(child)
		if err != nil {
			return true, err
		}
		dst, err := w.Mutable(root)
		if err != nil {
			return true, err
		}
		copy(dst, src)
		return true, w.Free(child)
	}
	return true, nil
}

func del(w Writer, pageNo uint32, key []byte) (found, underfull bool, err error) {
	nd, err := readNode(w, pageNo)
	if err != nil {
		return false, false, err
	}
	if nd.leaf {
		i := sort.Search(len(nd.keys), func(i int) bool { return bytes.Compare(nd.keys[i], key) >= 0 })
		if i == len(nd.keys) || !bytes.Equal(nd.keys[i], key) {
			return false, false, nil
		}
		if v := nd.vals[i]; v.overflow {
			if err := freeOverflow(w, v.first); err != nil {
				return false, false, err
			}
		}
		nd.keys = removeAt(nd.keys, i)
		nd.vals = removeAt(nd.vals, i)
		return true, nd.size() < minFill, writeNode(w, pageNo, nd)
	}
	idx := sort.Search(len(nd.keys), func(i int) bool { return bytes.Compare(key, nd.keys[i]) < 0 })
	found, childUnder, err := del(w, nd.children[idx], key)
	if err != nil || !found {
		return found, false, err
	}
	if childUnder {
		changed, err := rebalance(w, nd, idx)
		if err != nil {
			return true, false, err
		}
		if changed {
			if err := writeNode(w, pageNo, nd); err != nil {
				return true, false, err
			}
		}
	}
	return true, nd.size() < minFill || len(nd.keys) == 0, nil
}

// rebalance fixes an underfull child of parent by merging it with a sibling,
// or, if the two don't fit in one page, by sharing cells evenly between
// them. It reports whether parent was modified.
func rebalance(w Writer, parent *node, idx int) (bool, error) {
	if len(parent.children) < 2 {
		return false, nil
	}
	li := idx
	if idx == len(parent.children)-1 {
		li = idx - 1
	}
	ri := li + 1
	lp, rp := parent.children[li], parent.children[ri]
	left, err := readNode(w, lp)
	if err != nil {
		return false, err
	}
	right, err := readNode(w, rp)
	if err != nil {
		return false, err
	}
	merged := &node{leaf: left.leaf}
	if left.leaf {
		merged.keys = append(left.keys, right.keys...)
		merged.vals = append(left.vals, right.vals...)
		merged.next = right.next
	} else {
		merged.keys = append(append(left.keys, parent.keys[li]), right.keys...)
		merged.children = append(left.children, right.children...)
	}
	if merged.size() <= pager.PageSize {
		if err := writeNode(w, lp, merged); err != nil {
			return false, err
		}
		if err := w.Free(rp); err != nil {
			return false, err
		}
		parent.keys = removeAt(parent.keys, li)
		parent.children = removeAt(parent.children, ri)
		return true, nil
	}
	nl, nr, sep := merged.halves()
	// The parent must still fit with the new separator; if not, leave the
	// child underfull. That is allowed, just slightly less compact.
	oldSep := parent.keys[li]
	parent.keys[li] = sep
	if parent.size() > pager.PageSize {
		parent.keys[li] = oldSep
		return false, nil
	}
	if nl.leaf {
		nl.next = rp
	}
	if err := writeNode(w, lp, nl); err != nil {
		return false, err
	}
	if err := writeNode(w, rp, nr); err != nil {
		return false, err
	}
	return true, nil
}

// Drop frees every page of the tree, including the root.
func Drop(w Writer, root uint32) error {
	nd, err := readNode(w, root)
	if err != nil {
		return err
	}
	if nd.leaf {
		for _, v := range nd.vals {
			if v.overflow {
				if err := freeOverflow(w, v.first); err != nil {
					return err
				}
			}
		}
	} else {
		for _, c := range nd.children {
			if err := Drop(w, c); err != nil {
				return err
			}
		}
	}
	return w.Free(root)
}

// --- overflow pages --------------------------------------------------------

func makeValue(w Writer, val []byte) (leafVal, error) {
	if len(val) <= maxInline {
		return leafVal{inline: bytes.Clone(val)}, nil
	}
	// Write the chain back to front so each page knows its successor.
	var next uint32
	for end := len(val); end > 0; {
		start := ((end - 1) / overflowData) * overflowData
		n, err := w.Alloc()
		if err != nil {
			return leafVal{}, err
		}
		p, err := w.Mutable(n)
		if err != nil {
			return leafVal{}, err
		}
		binary.LittleEndian.PutUint32(p[0:], next)
		binary.LittleEndian.PutUint16(p[4:], uint16(end-start))
		copy(p[overflowHeader:], val[start:end])
		next, end = n, start
	}
	return leafVal{overflow: true, total: uint32(len(val)), first: next}, nil
}

func readValue(r Reader, v leafVal) ([]byte, error) {
	if !v.overflow {
		return v.inline, nil
	}
	out := make([]byte, 0, v.total)
	for n := v.first; n != 0; {
		p, err := r.Page(n)
		if err != nil {
			return nil, err
		}
		l := int(binary.LittleEndian.Uint16(p[4:]))
		out = append(out, p[overflowHeader:overflowHeader+l]...)
		n = binary.LittleEndian.Uint32(p[0:])
	}
	if len(out) != int(v.total) {
		return nil, errors.New("btree: overflow chain is corrupt")
	}
	return out, nil
}

func freeOverflow(w Writer, first uint32) error {
	for n := first; n != 0; {
		p, err := w.Page(n)
		if err != nil {
			return err
		}
		next := binary.LittleEndian.Uint32(p[0:])
		if err := w.Free(n); err != nil {
			return err
		}
		n = next
	}
	return nil
}

// --- cursor ------------------------------------------------------------------

// Cursor iterates over a tree in key order. A cursor over a write
// transaction must not be used across modifications of the same tree.
type Cursor struct {
	r    Reader
	root uint32
	page []byte
	idx  int
	err  error
}

// NewCursor returns an unpositioned cursor; call First, Seek or Last.
func NewCursor(r Reader, root uint32) *Cursor { return &Cursor{r: r, root: root} }

// First positions the cursor at the smallest key.
func (c *Cursor) First() { c.Seek(nil) }

// Seek positions the cursor at the first key >= key.
func (c *Cursor) Seek(key []byte) {
	p, err := c.r.Page(c.root)
	for err == nil && !isLeaf(p) {
		p, err = c.r.Page(childAt(p, childIndex(p, key)))
	}
	if err != nil {
		c.err, c.page = err, nil
		return
	}
	c.page, c.idx = p, lowerBound(p, key)
	c.skipToCell()
}

// Last positions the cursor at the largest key.
func (c *Cursor) Last() {
	p, err := c.r.Page(c.root)
	for err == nil && !isLeaf(p) {
		p, err = c.r.Page(link(p))
	}
	if err != nil {
		c.err, c.page = err, nil
		return
	}
	c.page, c.idx = p, nCells(p)-1
	if c.idx < 0 {
		c.page = nil
	}
}

func (c *Cursor) skipToCell() {
	for c.page != nil && c.idx >= nCells(c.page) {
		next := link(c.page)
		if next == 0 {
			c.page = nil
			return
		}
		p, err := c.r.Page(next)
		if err != nil {
			c.err, c.page = err, nil
			return
		}
		c.page, c.idx = p, 0
	}
}

// Valid reports whether the cursor points at an entry.
func (c *Cursor) Valid() bool { return c.page != nil && c.err == nil }

// Next advances to the following key.
func (c *Cursor) Next() {
	if c.page == nil {
		return
	}
	c.idx++
	c.skipToCell()
}

// Key returns the current key. It aliases page memory: copy it to keep it.
func (c *Cursor) Key() []byte {
	k, _ := cellKey(c.page, c.idx)
	return k
}

// Value returns the current value.
func (c *Cursor) Value() ([]byte, error) { return readValue(c.r, leafValue(c.page, c.idx)) }

// Err returns the first I/O error the cursor hit.
func (c *Cursor) Err() error { return c.err }

// Stats describes a tree's shape, for EXPLAIN-style introspection and tests.
type Stats struct {
	Depth, Leaves, Internal, Entries, OverflowPages int
}

// Inspect walks the whole tree and checks its invariants: keys sorted
// within and across nodes, separators bounding their subtrees, all leaves at
// the same depth, and the leaf chain visiting every leaf in order.
func Inspect(r Reader, root uint32) (Stats, error) {
	var st Stats
	var leaves []uint32
	var walk func(n uint32, lo, hi []byte, depth int) error
	walk = func(n uint32, lo, hi []byte, depth int) error {
		nd, err := readNode(r, n)
		if err != nil {
			return err
		}
		for i, k := range nd.keys {
			if i > 0 && bytes.Compare(nd.keys[i-1], k) >= 0 {
				return fmt.Errorf("page %d: keys out of order", n)
			}
			if lo != nil && bytes.Compare(k, lo) < 0 || hi != nil && bytes.Compare(k, hi) >= 0 {
				return fmt.Errorf("page %d: key outside parent bounds", n)
			}
		}
		if nd.leaf {
			if st.Depth == 0 {
				st.Depth = depth
			} else if st.Depth != depth {
				return fmt.Errorf("leaves at different depths")
			}
			st.Leaves++
			st.Entries += len(nd.keys)
			for _, v := range nd.vals {
				if v.overflow {
					st.OverflowPages += (int(v.total) + overflowData - 1) / overflowData
				}
			}
			leaves = append(leaves, n)
			return nil
		}
		st.Internal++
		for i, c := range nd.children {
			clo, chi := lo, hi
			if i > 0 {
				clo = nd.keys[i-1]
			}
			if i < len(nd.keys) {
				chi = nd.keys[i]
			}
			if err := walk(c, clo, chi, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, nil, nil, 1); err != nil {
		return st, err
	}
	for i, n := range leaves {
		p, err := r.Page(n)
		if err != nil {
			return st, err
		}
		want := uint32(0)
		if i+1 < len(leaves) {
			want = leaves[i+1]
		}
		if link(p) != want {
			return st, fmt.Errorf("leaf %d links to %d, want %d", n, link(p), want)
		}
	}
	return st, nil
}
