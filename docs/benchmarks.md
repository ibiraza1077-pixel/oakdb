# Benchmarks

OakDB compared with SQLite 3.53.4 (native C, via cgo) on the same machine (darwin/arm64, 10 CPUs), 100000 rows of `(id INTEGER PRIMARY KEY, k INT, v TEXT)` with an index on `k`.

Both run embedded in one process. SQLite uses WAL mode with `synchronous=FULL` and `fullfsync` on macOS, so both make every commit durable the same way. SQLite runs prepared statements; OakDB caches parsed statements but plans every query again.

| Workload | OakDB | SQLite | OakDB / SQLite |
|---|---:|---:|---:|
| Bulk insert (100k rows, 1 transaction) | 493k rows/s | 855k rows/s | 0.58× |
| Point lookup by primary key | 753k queries/s | 521k queries/s | 1.45× |
| Secondary index lookup | 354k queries/s | 652k queries/s | 0.54× |
| Range scan (100 rows each) | 44k scans/s | 125k scans/s | 0.35× |
| Full-table GROUP BY aggregate | 60 queries/s | 85 queries/s | 0.70× |
| Durable single-row commits (fsync) | 256 commits/s | 249 commits/s | 1.03× |

Regenerate with `go run ./cmd/oakbench`. Numbers vary between machines; the ratio is the useful part.

## Reading the results honestly

SQLite has had more than 20 years of tuning. OakDB is a teaching-sized engine, so the interesting question is *where* the gap is and why:

- **Per-query overhead.** OakDB plans every statement from scratch and decodes whole rows into Go values, while SQLite runs precompiled bytecode. This dominates small point lookups.
- **Durable commits** are bound by the disk's flush time for both engines, so they land close together.
- **Scans and aggregates** stress the B-tree and the executor; OakDB's copy-free cursor over immutable cached pages keeps these competitive.
