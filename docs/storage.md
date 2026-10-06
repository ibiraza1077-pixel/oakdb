# Storage: pages, the B+tree and overflow pages

Code: [`internal/pager`](../internal/pager), [`internal/btree`](../internal/btree), [`internal/record`](../internal/record)

## Why pages?

Disks and operating systems move data in blocks, not bytes. OakDB therefore treats its file as an array of fixed **4 KB pages**: page *n* lives at byte offset `n × 4096`. Every structure (tables, indexes, the catalog, the free list) is built out of pages, and the pager is the only code that reads or writes the file.

Page 0 is the **header**:

| Offset | Field | Meaning |
|---|---|---|
| 0 | magic `OAKDB001` | identifies the file format |
| 8 | page size | always 4096 |
| 12 | page count | pages in use, including free ones |
| 16 | free list head | first free page (0 = none) |
| 20 | catalog root | root page of the B-tree holding table definitions |
| 24 | schema token | random number changed by every schema change |

### The buffer pool

Reading from disk is slow, so recently used pages are kept in memory in an **LRU cache** (least recently used pages are evicted first). The default is 8,192 pages (32 MB). Cached pages are *immutable* committed versions, which means they can be handed to many readers at once with no copying and no locks. Writers never modify a cached page: they copy it first (see [durability](durability.md)).

### The free list

When a table is dropped or a B-tree node merges with its neighbour, its pages go onto a **free list**: a linked list threaded through the free pages themselves, with each free page storing the number of the next one in its first 4 bytes. `Alloc` takes from the free list before growing the file, so space is reused rather than leaked. A test drops a large table, inserts again, and checks the file did not grow.

## The B+tree

Every table and every index is a **B+tree**: a balanced tree where all the data lives in the leaves, and internal nodes only hold separator keys that guide the search. Leaves are linked left to right, so a range scan finds its first key and then walks sideways along the bottom.

Why a B+tree rather than a binary tree or a hash table?

- **Few disk reads.** Each node is a whole page holding dozens to hundreds of keys, so the tree is very shallow. 100,000 rows fit in a tree three levels deep, so any row is three page reads away.
- **Ordered.** Unlike a hash table, it supports range queries (`id BETWEEN 10 AND 20`), `ORDER BY` without sorting, and prefix lookups on multi-column indexes.
- **Always balanced.** Splits and merges keep every leaf at the same depth, so the worst case stays logarithmic.

Keys and values are plain byte strings compared with `bytes.Compare`. That keeps the tree completely independent of SQL types; the [record](#order-preserving-keys) layer makes SQL values sort correctly as bytes.

### Slotted page layout

```
 0        1        3          7
 ┌────────┬────────┬──────────┬───────────────────┬─────────────────────────┐
 │  type  │ ncells │ link     │ offsets[0..n-1]   │ cells (in key order)    │
 └────────┴────────┴──────────┴───────────────────┴─────────────────────────┘
   1=leaf   uint16   leaf: next leaf               uint16 each
            internal: rightmost child

 leaf cell:      uvarint keyLen | key | 0 | uvarint valLen | value
                 uvarint keyLen | key | 1 | uint32 totalLen | uint32 firstOverflowPage
 internal cell:  uvarint keyLen | key | uint32 child
```

The **offset array** lets a lookup binary-search a page without parsing every cell: read offset *i*, jump to the cell, compare its key. Lookups on the read path never allocate. They work directly on the page bytes.

An internal node with keys `k0 … k(n-1)` has `n + 1` children. Child *i* holds keys in `[k(i-1), ki)`, and the rightmost child (stored in the header) holds keys ≥ the last separator.

### Insert and split

1. Walk from the root to the leaf where the key belongs.
2. If the new cell fits, **splice it into the page in place**: shift the offsets, copy the cell bytes into position and you're done. This is the common case and costs one 4 KB copy.
3. If it doesn't fit, **split**: divide the cells into two halves of roughly equal *bytes* (not equal counts, since cells vary in size), write the right half to a new page, link the leaves, and pass `(separator, new page)` up to the parent. That may split the parent too, all the way to the root.

Two refinements:

- **Root stays put.** When the root splits, its contents move to a new page and the root page becomes the parent of the two halves. The root's page number therefore never changes, so the catalog can store it permanently.
- **Append split.** Auto-increment ids always insert at the far right. A 50/50 split would leave every leaf half empty forever. When the new key lands past the end of the rightmost leaf, OakDB keeps the full leaf intact and starts a fresh leaf with just the new key. Leaves fill to nearly 100%, and 100,000 sequential rows take about half the pages. SQLite has a similar optimisation, called "quick balance".

**Size limits.** Keys are capped at 512 bytes and values above 700 bytes go to overflow pages, so every cell is under a third of a page. That guarantees any overfull node can always be split into two halves that both fit, which keeps the split code simple and provably correct.

### Delete, merge and rebalance

Deleting removes the cell. If a node drops below a quarter full, it is fixed using a sibling:

- If the two nodes fit in one page, **merge** them and free the empty page. The parent loses a separator, which may make the parent underfull, so the fix propagates upwards.
- Otherwise **redistribute**: pool both nodes' cells and split them evenly again, updating the separator in the parent.

When the root is left with a single child, the child's contents are copied into the root (the tree gets one level shorter, and the root page number still never changes).

### Overflow pages

A value too large for a cell (say, a 10 KB text field) is stored in a **chain of overflow pages**, each holding `next page | length | data`. The leaf cell keeps only the total length and the first page number. Reading follows the chain; deleting frees it.

### Cursors

A cursor holds a pointer to a leaf page and a position in it. `Seek(key)` descends once; `Next()` steps along the leaf and follows the `next` link at the end. Because readers see immutable page versions, a cursor can hold a page reference safely while other transactions commit.

### Proving it correct

`btree.Inspect` walks a whole tree and checks every invariant: keys strictly increasing within each node, every key within the bounds its parent's separators allow, all leaves at the same depth, and the leaf chain visiting every leaf in order. The tests run thousands of random inserts, updates and deletes (including large overflow values) against a Go `map` as the model, call `Inspect` throughout, and count pages to catch leaks or double frees. The fuzzer does the same with random operation sequences.

## Order-preserving keys

The B-tree compares raw bytes, so SQL values must be encoded so that **byte order equals value order**. This is called a *memcomparable* encoding:

| Value | Encoding | Why it sorts correctly |
|---|---|---|
| NULL | `0x05` | smallest tag, so NULLs sort first |
| FALSE / TRUE | `0x10` / `0x11` | |
| number | `0x20` + 8-byte float, sign-flipped + 8-byte exact integer | flipping the sign bit (and all bits for negatives) makes IEEE-754 floats sort as unsigned integers; the exact int64 breaks ties between huge integers a float cannot tell apart |
| text | `0x30` + bytes with `0x00` escaped as `0x00 0xFF` + terminator `0x00 0x01` | the terminator sorts below any real character, so `"a"` < `"ab"`, and tuples (`"a", 2` vs `"ab", 1`) compare field by field |

Integers and reals share one numeric encoding, so `WHERE int_col > 2.5` can still use an index. A property test checks across tens of thousands of random pairs that `bytes.Compare(encode(a), encode(b)) == Compare(a, b)`.

**Index entries** are `encode(indexed columns) + table key`. Appending the table key makes every entry unique even when indexed values repeat, and lets an index scan find the row. A **unique index** checks whether any entry already starts with the same indexed-value prefix (NULLs never conflict, as in PostgreSQL).

Rows themselves are stored in a compact, non-sortable encoding (varints, length-prefixed text), since only keys need to be comparable.
