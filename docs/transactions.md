# Transactions and isolation

Code: [`internal/pager/pager.go`](../internal/pager/pager.go) (snapshots), [`internal/engine/db.go`](../internal/engine/db.go) (sessions)

## The model: many readers, one writer

OakDB allows any number of read transactions at once, plus **one write transaction at a time**. This is the same model as SQLite, and it is a deliberate choice: it makes every transaction serializable without any locks on rows or pages, and it keeps the code small enough to reason about completely.

## Snapshots

When a transaction starts reading, it takes a **snapshot**: it records how many frames were committed to the [write-ahead log](durability.md) at that moment. From then on, every page it reads is "the newest version committed at or before my snapshot".

Because committed page versions are never modified (writers copy pages before changing them), a snapshot needs no locks at all. Its pages simply cannot change underneath it. This gives three properties:

- **Readers never block writers, and writers never block readers.**
- **No dirty reads:** a writer's changes live only in its private dirty pages until it commits.
- **Repeatable reads:** a transaction sees the same data every time it looks, even if others commit in between.

The test in `engine_test.go` shows this directly: session B cannot see A's uncommitted transfer; after A commits, a transaction that started earlier still sees the old balance.

## Writing: upgrade or fail

A transaction that has only read so far may later want to write (`BEGIN; SELECT ...; UPDATE ...`). It must become the writer, but it can only do so safely if **nothing has committed since its snapshot**. Otherwise it would be updating data based on a stale view.

So `Upgrade` takes the writer lock and checks the commit count:

- unchanged: the transaction becomes the writer and carries on;
- changed: it fails with **SQLSTATE 40001 (serialization failure)**, and the application retries.

This is exactly what PostgreSQL does at its `SERIALIZABLE` level. A transaction that has not read anything yet simply starts a fresh write transaction, so `BEGIN; INSERT ...` never fails this way.

Why is this serializable? Writers run one at a time, so they are trivially serial. Each read-only transaction sees a consistent snapshot, which is the database at one point in the serial order. A read-then-write transaction is only allowed to commit if nothing happened between its snapshot and its writes, so it too fits at one point in the order.

## Sessions and PostgreSQL semantics

`engine.Session` implements the transaction rules clients expect:

- **Autocommit:** a statement outside `BEGIN` runs in its own transaction. Writes start as the writer directly (waiting if another writer is active), so they never get serialization errors.
- **Explicit transactions:** `BEGIN` takes a snapshot; `COMMIT` commits; `ROLLBACK` discards.
- **Failed transactions:** after an error inside `BEGIN`, every statement fails with `25P02 current transaction is aborted` until `ROLLBACK`, and `COMMIT` turns into a rollback. This is PostgreSQL's behaviour, and drivers rely on it.
- **Transactional DDL:** table definitions are stored in pages like everything else, so `CREATE TABLE` inside a rolled-back transaction never happened.

## The bank test

`tests/concurrency_test.go` is the classic isolation test. Four goroutines move random amounts between 20 accounts inside read-then-write transactions, retrying on serialization failures. Meanwhile four reader goroutines repeatedly sum every balance. If any reader ever saw half a transfer, the total would be wrong. It runs under Go's race detector: thousands of transfers, tens of thousands of reads, and the total never changes.

## What a production database would add

- **MVCC with row versions** (as in PostgreSQL) so that many writers can work at once, detecting conflicts per row instead of per database.
- **Lock timeouts:** a writer currently waits as long as the active writer takes.
- **Long-reader management:** a checkpoint waits until no snapshot is active, so a reader that never finishes lets the log grow. Production systems checkpoint around readers or warn about them.
