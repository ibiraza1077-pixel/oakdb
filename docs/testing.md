# Testing a database

A bug in a web app shows the wrong page. A bug in a database silently loses or corrupts someone's data, sometimes months later. So OakDB is tested in layers, from single functions up to killing the whole process.

```bash
go test ./...          # everything, about 15 seconds
go test -race ./...    # with the race detector
```

## 1. Unit tests per layer

Each package tests itself through its public API:

- **pager:** commit, rollback, reopen, snapshot isolation, free-list reuse, checkpoints under a tiny buffer pool.
- **btree:** thousands of random operations against a Go `map` as the model, with `Inspect` checking every structural invariant; delete-everything must free every page; variable key sizes; overflow chains; the append split packing leaves full.
- **record:** a property test showing that byte order of encoded keys equals value order, across tens of thousands of random pairs, including NULLs, extreme integers, negative reals and text with zero bytes.
- **sql:** operator precedence, every clause, error positions, and statements that must be rejected.
- **engine:** SQL behaviour (NULL logic, aggregates, joins, constraints, updates that move rows, transactions, parameters), plus `EXPLAIN` checks that the planner actually uses indexes.

## 2. Fuzzing

Go's built-in fuzzer generates random inputs and keeps the ones that reach new code:

```bash
go test ./internal/sql   -run XXX -fuzz FuzzParse  -fuzztime 60s
go test ./internal/btree -run XXX -fuzz FuzzTree   -fuzztime 60s
```

The parser must never panic on any input. The B-tree must keep its invariants and agree with the model for any sequence of operations.

## 3. Differential testing against SQLite

`tests/differential_test.go` is the strongest correctness check. A random generator produces `INSERT`, `UPDATE`, `DELETE` and `SELECT` statements (filters with `AND` / `OR` / `NOT`, `IN`, `BETWEEN`, `LIKE`, `IS NULL`, joins, left joins, `GROUP BY` / `HAVING`, `DISTINCT`, `ORDER BY` / `LIMIT`, arithmetic, `CASE` and string functions). Each statement runs on **both** OakDB and SQLite:

- every `SELECT` must return identical rows (as a multiset unless the query fully orders its output);
- after every write, both tables are compared in full, so a divergence is reported at the exact statement that caused it;
- the integrity checker runs at the end.

The generator stays within the large part of SQL where the two databases agree. Where they deliberately differ, it avoids the difference. For example, PostgreSQL (and OakDB) treat `\` as the escape character in `LIKE`, while SQLite does not; the test found exactly that difference before it was excluded.

```bash
OAKDB_DIFF_ITERS=20000 go test ./tests -run Differential -v   # about 30,000 queries
```

## 4. Crash testing

`tests/crash_test.go` re-runs the test binary as a child process that commits transactions as fast as it can, printing `COMMITTED n` after each `COMMIT` returns. Its workload mixes small rows, rows big enough for overflow pages, deletes that free pages, and a small log size so checkpoints happen constantly. The parent kills the child with **SIGKILL** at a random moment, then:

1. reopens the database, which runs recovery;
2. runs `db.Check()`, verifying every B-tree invariant and that every index matches its table exactly;
3. checks **atomicity**: every batch has all of its rows or none;
4. checks **durability**: every batch the child reported as committed is present. At most one extra batch may exist, the one in flight when it was killed.

It repeats this many times on the same file.

```bash
OAKDB_CRASH_ROUNDS=200 go test ./tests -run Crash -v
```

`pager_test.go` complements it at the byte level: the log is cut at random offsets and has bits flipped, and recovery must produce exactly a prefix of the committed transactions.

## 5. Concurrency

`tests/concurrency_test.go` runs the bank invariant: writers transfer money (retrying on serialization failures) while readers continuously check that the total never changes. It runs under `-race`.

## 6. Real clients

`pgx` (Go) and `psycopg` (Python) connect over TCP and exercise prepared statements, binary values, NULLs, transactions and typed errors. The Python test is skipped if `uv` is not installed.

## Continuous integration

GitHub Actions runs `gofmt`, `go vet`, the full test suite with the race detector, short fuzzing runs and a longer differential run on every push.
