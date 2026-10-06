# The PostgreSQL wire protocol

Code: [`internal/pgwire/server.go`](../internal/pgwire/server.go)

Rather than invent a protocol and write client libraries for it, OakDB speaks **PostgreSQL's frontend/backend protocol (version 3)**. Every PostgreSQL client in every language (`psql`, Python's `psycopg`, Go's `pgx`, JDBC, GUI tools) can then talk to it. This is the same approach CockroachDB, YugabyteDB and others take.

## Message format

After start-up, every message is:

```
┌──────┬────────────────────────┬──────────────┐
│ type │ length (int32, big-end)│ body         │
│ 1 B  │ includes itself        │ length - 4 B │
└──────┴────────────────────────┴──────────────┘
```

## Start-up

1. The client may first send an **SSLRequest**. OakDB answers `N` ("no TLS"), and the client continues in plain text.
2. The client sends a **StartupMessage** with protocol version 3.0 and parameters (user, database, application name).
3. OakDB replies `AuthenticationOk`, several `ParameterStatus` messages (`server_version`, `client_encoding`, `standard_conforming_strings`, ...), `BackendKeyData`, and `ReadyForQuery`.

`ReadyForQuery` carries the transaction status: `I` (idle), `T` (in a transaction) or `E` (in a failed transaction). Drivers use it to manage transactions.

## Simple query protocol

The client sends `Query` with SQL text, which may hold several statements separated by semicolons. For each statement OakDB sends `RowDescription` (column names and type OIDs) and one `DataRow` per row if it returns rows, then `CommandComplete` with a tag like `SELECT 3`, `INSERT 0 1` or `UPDATE 2`. A final `ReadyForQuery` ends the exchange. An error sends `ErrorResponse` and stops processing the rest of the string.

## Extended query protocol

This is what drivers use for prepared statements and parameters. It splits a query into steps:

| Message | What OakDB does |
|---|---|
| `Parse` (name, SQL, parameter types) | parse once, store as a named prepared statement, and infer types for any parameter the client left unspecified |
| `Bind` (statement, parameter values, result formats) | decode the parameters (text or binary) into a **portal**, a statement ready to run |
| `Describe` (statement or portal) | `ParameterDescription` with the parameter types, plus `RowDescription` for the result columns, found by planning without running |
| `Execute` (portal) | run it and send the `DataRow`s and `CommandComplete` |
| `Sync` | end of the batch: send `ReadyForQuery` |
| `Close` / `Flush` / `Terminate` | free a statement or portal / flush output / disconnect |

**Error handling.** After an error in the extended protocol, the server must ignore every message until the next `Sync`. Drivers pipeline several messages at once, and continuing would run statements the client no longer expects.

**Binary format.** Drivers like `pgx` send integers and floats as raw big-endian bytes and ask for binary results. OakDB supports binary `int2`, `int4`, `int8`, `float4`, `float8`, `bool` and text.

## Types

| OakDB type | PostgreSQL OID |
|---|---|
| INTEGER | 20 (`int8`) |
| REAL | 701 (`float8`) |
| TEXT | 25 (`text`) |
| BOOLEAN | 16 (`bool`) |

## Errors

Every error carries a **SQLSTATE** code: `23505` unique violation, `23502` not-null violation, `42P01` undefined table, `42703` undefined column, `42601` syntax error, `22012` division by zero, `40001` serialization failure, `25P02` failed transaction. Drivers turn these into typed exceptions; for example, Python's `psycopg` raises `psycopg.errors.UniqueViolation`, and a test checks exactly that.

## Tested with real drivers

- `internal/pgwire/server_test.go` runs `pgx` in four modes: cached prepared statements, describe-then-execute, the simple protocol and exec-only. It covers every type, NULLs, aggregates, transactions, errors and multi-statement queries.
- `tests/clients/psycopg_check.py` runs a Python program using `psycopg` 3, including `executemany`, transactions as context managers, and typed exceptions.
