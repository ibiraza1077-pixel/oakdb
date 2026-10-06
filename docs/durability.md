# Durability: the write-ahead log, checkpoints and crash recovery

Code: [`internal/pager/pager.go`](../internal/pager/pager.go), [`internal/pager/wal.go`](../internal/pager/wal.go)

## The problem

A transaction might change dozens of pages: a leaf, its parent after a split, an index, the free list, the header. The disk can only write one page at a time, and power can fail between any two writes, or even halfway through one page (a **torn write**). If we wrote changes straight into the database file, a crash could leave it with some of a transaction's pages updated and others not: a corrupt B-tree.

The promise OakDB makes is the "A" and "D" of ACID:

- **Atomicity:** after a crash, each transaction is either completely there or not at all.
- **Durability:** once `COMMIT` returns, the transaction survives any crash.

## The write-ahead log

OakDB uses the same design as SQLite's WAL mode.

1. **A write transaction never touches the database file.** The first time it changes a page, it copies the page into a private "dirty" map (copy-on-write) and modifies the copy. Other connections cannot see these copies.
2. **On `COMMIT`, every dirty page is appended to the log** (`oak.db-wal`) as a *frame*, a 24-byte header followed by the 4 KB page. The last frame is marked with a **commit flag**.
3. **One `fsync` of the log** makes the transaction durable. Only then does `COMMIT` return.
4. **Rollback is free:** throw the dirty map away. Nothing was ever written.

```
 WAL file
 ┌────────────┬───────────────────────┬───────────────────────┬─────┐
 │ header     │ frame: page 7  | data  │ frame: page 12 | data │ ... │
 │ magic,salt │ salt, checksum         │ salt, checksum, COMMIT│     │
 └────────────┴───────────────────────┴───────────────────────┴─────┘
```

### Checksums: knowing what is valid

Each frame stores a **running CRC-64 checksum** over every frame header and page body up to and including itself, seeded with a random **salt** from the log header. A frame is valid only if:

- its salt matches the header (so leftovers from an older generation of the log are rejected), and
- its checksum matches the chain (so any torn or corrupted frame breaks the chain, and *every frame after it* is rejected too).

### Recovery

When the database opens, it reads the log from the start:

```
for each frame:
    stop if the salt or checksum is wrong
    add the frame to "pending"
    if it has the commit flag: move all pending frames to "committed"
```

Frames after the last valid commit belonged to a transaction that never finished, so they are discarded and the log is truncated there. The committed frames are copied into the database file (a checkpoint, below), and the database is consistent again.

This is why a crash at *any* byte is safe: either the commit frame and every frame before it are intact and checksummed, or the transaction is ignored entirely.

## Reading through the log

Until a checkpoint, the newest version of a page may live in the log rather than the database file. The pager keeps an in-memory **WAL index**: for each page, the list of log frames holding versions of it. To read page *n*, it uses the newest frame (or falls back to the database file), and the buffer pool caches the result keyed by `(page, frame)`.

That structure is also what gives readers [snapshots](transactions.md): a reader remembers how many frames were committed when it started, and only looks at frames up to that point.

## Checkpoints

The log cannot grow forever. When it passes 1,000 frames and no reader is active, a **checkpoint**:

1. copies the newest version of every page from the log into the database file,
2. `fsync`s the database file,
3. **only then** resets the log (with a new random salt).

If the process crashes between steps 2 and 3, the next open replays the same frames again. Copying a page to the same place twice is harmless, so checkpoints are safe to interrupt at any point.

## How this is tested

- **Torn-write test** (`pager_test.go`): commit five transactions, then truncate the log at dozens of random byte offsets, and separately flip a bit in the last frame. Every time, the reopened database must contain exactly the transactions whose frames fully survived.
- **Crash test** (`tests/crash_test.go`): a child process commits transactions as fast as it can (with large rows that use overflow pages, deletes that free pages, and frequent checkpoints) and reports each commit the moment it returns. The parent kills it with `SIGKILL` at a random moment, reopens the file, runs the full integrity check, and verifies that every reported commit is present and no transaction is partly applied. It repeats this on the same file, so recovery must also cope with a database that has already crashed many times.

## fsync on macOS

On macOS, a plain `fsync` only pushes data to the drive's cache, not to permanent storage. Go's `File.Sync` uses `F_FULLFSYNC`, which really flushes. That is why durable commits run at about 250 per second on a laptop SSD, and why the benchmark turns on SQLite's `fullfsync` for a fair comparison. `-nosync` disables syncing for tests and benchmarks where a power cut is not a concern.
