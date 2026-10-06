# Interview guide

Questions an interviewer is likely to ask about OakDB, with the answer in plain English and where to look in the code. Read the design docs first; this is for revision.

## The 60-second pitch

> "OakDB is a SQL database I built from scratch in Go. Data lives in a B+tree on 4 KB pages. Every commit goes to a write-ahead log first, so a crash at any point loses nothing that was committed and never leaves half a transaction. Readers get snapshots, so they never block. Queries go through a hand-written parser and a planner that chooses indexes, index-only scans and hash joins. It speaks the PostgreSQL wire protocol, so psql and normal Postgres drivers connect to it. To prove it's correct, I run tens of thousands of random queries against both OakDB and SQLite and require identical results, and a crash test kills the process with SIGKILL mid-write over and over and checks nothing is lost."

## Storage

**Why a B+tree and not a hash index?**
It keeps keys sorted, so it supports range queries, `ORDER BY` without sorting and prefix lookups on multi-column indexes. Each node is a full page with high fan-out, so 100,000 rows is a three-level tree: three page reads to find any row. A hash index only does equality.

**Walk me through an insert that splits.** (`btree.insert`, `writeOrSplit`)
Descend to the leaf. If the cell fits, splice it into the slotted page. If not, split the cells into two halves by bytes, write the right half to a new page, link the leaves, and return the right half's first key as a separator for the parent. The parent inserts it, and may split too. If the root splits, its contents move to a new page so the root's page number never changes.

**What's a slotted page?**
A header, then an array of 2-byte offsets in key order, then the cells. Binary search reads offsets without parsing every cell. Inserting shifts offsets and copies the new cell into place.

**What was the "append split" optimisation?**
Auto-increment ids always insert at the right edge. A 50/50 split leaves every leaf half empty forever. Instead, when the key goes past the end of the rightmost leaf, keep that leaf full and start a new one. Leaves went from about 50% to about 98% full, and the table needed roughly half the pages.

**How do you store values bigger than a page?**
Overflow pages: a linked chain of pages holding the value; the leaf cell stores the length and the first page.

**How do keys of different types sort correctly as bytes?**
Memcomparable encoding: a type tag, then numbers stored as sign-flipped IEEE floats (which sort like unsigned integers) plus the exact int64, and text with zero bytes escaped and a low terminator. A property test checks byte order equals value order.

## Durability and crashes

**What happens on COMMIT?** (`WriteTx.Commit`, `wal.appendCommit`)
The transaction's modified pages, which only ever existed as private copies, are appended to the WAL as frames with a running checksum, and the last frame gets a commit flag. One fsync makes it durable. Only then does COMMIT return.

**What if power fails halfway through writing the log?**
Each frame's checksum chains from the previous one, seeded by a random salt. Recovery reads frames until a checksum breaks, keeps only frames up to the last commit flag, and discards the rest. So a torn write invalidates the unfinished transaction, never a committed one.

**Why not write straight into the database file?**
A transaction touches many pages, and the disk writes one at a time. A crash in the middle would leave a half-updated B-tree. With the log, the file is only updated by checkpoints, and those copy already-committed pages, so replaying a checkpoint twice is harmless.

**What's a checkpoint and when is it safe?**
Copying the newest version of each page from the log into the database file, fsyncing it, then resetting the log. It runs when the log passes 1,000 frames and no reader needs the old versions.

**How do you know recovery works?**
Two tests. One cuts the log at random byte offsets and flips bits, and checks recovery produces exactly the transactions that fully survived. The other kills a writing process with SIGKILL at random moments, dozens of times, and checks every acknowledged commit is present, no transaction is partial, and the integrity checker passes.

## Transactions

**What isolation level do you provide?**
Serializable. There is one writer at a time, and readers use snapshots. A transaction that read and then wants to write is only allowed to if nothing committed since its snapshot; otherwise it gets error 40001 and retries. That's the same contract as PostgreSQL's SERIALIZABLE.

**How do snapshots work without locks?**
A snapshot remembers how many log frames were committed when it started and reads the newest page version at or before that. Committed page versions are immutable because writers copy before modifying, so nothing can change under a reader.

**Why only one writer?**
It makes serializability trivial and the code provably correct. The trade-off is write throughput under contention. The next step would be MVCC with row versions so writers to different rows don't conflict.

**What happens after an error inside BEGIN?**
Like PostgreSQL: the transaction is marked failed, every statement errors with 25P02 until ROLLBACK, and COMMIT turns into a rollback.

## Query processing

**How does the planner choose an index?** (`chooseAccess`)
It's rule-based: primary-key equality, then the index with the most leading equality columns, then IN lists, then ranges, then a full scan. Every WHERE condition is still applied by a filter afterwards, so the index only narrows the work and can never change the answer.

**What join algorithms do you have and when is each used?**
Index nested-loop when the inner table can be probed by key or index using the outer row's values. Hash join for other equality conditions: build a hash table on the inner side, then stream the outer side, for O(n + m). Plain nested loop otherwise.

**What's an index-only scan?**
If the index plus the primary key contains every column the query needs, rows are built from index entries and the table is never read. It made indexed lookups 2.5× faster.

**What's the Volcano model?**
Every operator has `next()`, which pulls one row from its child. Rows stream through filters and projections, so LIMIT stops early. Only sort, aggregation and the hash-join build side hold all their input.

**Why compile expressions to closures?**
So per-row work is just function calls on pre-resolved column positions, instead of walking the syntax tree and looking up names for every row.

**What's the Halloween problem?**
An UPDATE that changes the key it is scanning by can meet its own updated rows again. OakDB collects all target rows before changing any of them.

**How do you handle NULL?**
Three-valued logic: comparisons with NULL are NULL, AND/OR follow the truth tables, WHERE keeps only TRUE, `NOT IN` with a NULL in the list is never true, and NULLs never conflict in unique indexes.

## Testing and performance

**How do you know the SQL results are right?**
Differential testing: a random generator produces about 30,000 queries and 30,000 writes, each run on both OakDB and SQLite, and results must match exactly, with full table comparisons after every write. It found a real semantic difference (the LIKE escape character), which taught me where the two databases intentionally disagree.

**How fast is it?**
On the same machine, with every commit fsynced: primary-key lookups are about 1.4× SQLite's speed, durable commits are equal (both limited by the disk), and inserts, index lookups, scans and aggregates are 0.35× to 0.7×. SQLite has 20+ years of tuning and runs precompiled bytecode; OakDB plans every query from scratch.

**Tell me about an optimisation you made.**
Profiling bulk inserts showed most time in garbage collection: every insert decoded and copied a whole B-tree node. I added a fast path that splices the new cell straight into the slotted page, plus the append split. Inserts went from 91k to about 490k rows per second. Then I found index lookups doing disk reads because the table didn't fit in the buffer pool. Half-empty leaves were the cause, which the append split fixed, and index-only scans made lookups 2.5× faster.

**What would you do next?**
MVCC for concurrent writers; a cost-based optimiser with table statistics and join reordering; subqueries; a prepared-plan cache; and group commit to batch fsyncs across connections.

## Honest framing

Say plainly that you built it with an AI pair programmer, then show you understand it: walk through a split, a recovery or a join on a whiteboard. Interviewers care far more that you can explain the trade-offs than who typed each line.
