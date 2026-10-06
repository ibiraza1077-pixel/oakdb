# The query engine: parsing, planning and execution

Code: [`internal/sql`](../internal/sql), [`internal/engine`](../internal/engine)

A query goes through three stages:

```
"SELECT name FROM users WHERE id = 3"
        │  lexer + parser      (internal/sql)
        ▼
  AST: Select{Items: [name], From: [users], Where: Binary{=, id, 3}}
        │  planner             (engine/plan.go)
        ▼
  Project(name) ← Filter(id = 3) ← Primary Key Lookup(users, id = 3)
        │  executor            (engine/exec.go)
        ▼
  rows
```

## 1. Parsing

**The lexer** turns text into tokens: identifiers (lower-cased unless "quoted"), numbers, 'strings' (with `''` escapes), `$1` parameters, operators and comments.

**The parser** is hand-written **recursive descent**: one function per grammar rule (`selectStmt`, `fromClause`, `expr`, ...). Operator precedence uses one function per level, lowest first:

```
OR → AND → NOT → comparison (= < LIKE IN BETWEEN IS) → || → + - → * / % → unary - → :: cast → primary
```

So `a OR b AND c` parses as `a OR (b AND c)`, and `1 + 2 * 3` as `1 + (2 * 3)`. Each function parses the next-higher level first, then loops while it sees its own operator. Errors report the position: `syntax error at position 15: expected FROM, found end of input`.

A hand-written parser was chosen over a generator (like yacc) because the code is easy to read and debug, the error messages are good, and it is how many production databases do it. The parser is fuzzed with millions of random inputs to make sure no input can crash it.

## 2. Planning

The planner turns the AST into a tree of operators. OakDB's planner is **rule-based**: with no table statistics, it applies a fixed preference order that is right for the common cases.

### Access paths: how to read one table

For the first table, the planner looks at the `WHERE` conditions that mention only that table, written as `column op value` (in either order) where the value doesn't depend on the table itself:

| Priority | Condition | Plan | Cost |
|---|---|---|---|
| 1 | `pk = v` | **Primary key lookup** | one tree descent |
| 2 | `a = v [AND b = w ...]` with an index on `(a, b, ...)` | **Index scan** with the longest matching prefix, plus an optional range on the next column | descent + matching entries |
| 3 | `pk IN (...)` or `indexed_col IN (...)` | **Point lookups**, one per distinct value, in key order | one descent each |
| 4 | `pk > v`, `BETWEEN`, ... | **Key range scan** | descent + rows in range |
| 5 | anything else | **Sequential scan** | every row |

Crucially, **every `WHERE` condition is still checked by a filter above the access path.** The access path only narrows which rows are looked at; it never decides which rows are returned. That makes it impossible for an index choice to change a result, and the differential tests confirm it.

**Index-only scans.** If an index (plus the primary key, which every index entry ends with) contains every column the query uses, the executor builds rows straight from the index entries and never touches the table. `SELECT count(*) FROM orders WHERE user_id = 7` reads only the index. This made indexed lookups 2.5× faster.

**Avoiding sorts.** A table's B-tree is ordered by primary key, so `ORDER BY pk` on a single-table plan reads rows in order and the sort is skipped. With `LIMIT 10`, only the first ten rows are ever read.

### Joins

Tables are joined in the order written. For each new table, the planner gathers the usable conditions (the `ON` clause, plus `WHERE` conditions for inner joins) and picks:

1. **Index nested-loop join:** if a condition like `orders.user_id = users.id` lets the new table be probed by key or index with values from the current row, re-open an index lookup for each outer row. Cost: *n* × one descent.
2. **Hash join:** for any other equality between the two sides, read the new table once into a hash table keyed on its join columns, then stream the outer rows and probe. Cost: *n* + *m*. NULL keys are skipped because NULL never equals anything.
3. **Nested-loop join:** otherwise, compare every pair. Cost: *n* × *m*.

`LEFT JOIN` emits the outer row padded with NULLs when no inner row matched. `WHERE` conditions on the right table are applied after the join, not pushed into it, which is the difference between `ON` and `WHERE` for outer joins.

### Aggregation

When a query has aggregates or `GROUP BY`, the planner:

1. collects every aggregate call in `SELECT`, `HAVING` and `ORDER BY` (de-duplicated, so `count(*)` written twice is computed once);
2. builds a **hash aggregate**: for each input row, compute the group key, find or create its group, and update each aggregate's state (count, running sum with overflow detection, min/max, a set for `DISTINCT`);
3. compiles the output expressions against the post-aggregation row `[group values..., aggregate values...]`.

To recognise that `SELECT upper(city) ... GROUP BY upper(city)` refers to the group key, expressions are compared by a **fingerprint**: their SQL text with every column reference resolved to its position. A bare column that is neither grouped nor aggregated is an error (`42803`), as in PostgreSQL.

## 3. Execution

### The iterator ("Volcano") model

Every operator implements `next() → row`. Calling `next()` on the root pulls one row through the whole tree:

```
Limit.next() → Project.next() → Filter.next() → IndexScan.next() → B-tree cursor
```

Operators that can stream (scans, filters, projections, nested-loop joins, limits) never hold more than one row. Only operators that genuinely need all their input (sort, hash aggregate, the build side of a hash join) materialise it. That is why `LIMIT` stops work early.

### Compiled expressions

Walking an AST for every row is slow (type switches, name lookups). Instead, each expression is **compiled once** into a Go closure, `func(row) (value, error)`, with column names already resolved to positions:

```go
// "price * 2 > 10" becomes, roughly:
func(row Row) (Value, error) {
    a := row[3]                  // price, resolved at plan time
    b, _ := arith("*", a, two)   // constant folded into the closure
    return compare(">", b, ten)
}
```

### SQL's three-valued logic

`NULL` means "unknown", so comparisons with `NULL` are `NULL`, not false. `AND` and `OR` follow the truth tables (`FALSE AND NULL = FALSE`, `TRUE OR NULL = TRUE`, `TRUE AND NULL = NULL`), `NOT IN` with a `NULL` in the list can never be true, and `WHERE` keeps only rows where the condition is TRUE. Integer arithmetic checks for overflow, and division by zero is an error (`22012`).

## Writes

`UPDATE` and `DELETE` first collect every matching row's key, using the same access paths, **before changing anything**. Otherwise `UPDATE t SET id = id + 1` could meet its own updated rows again further along the scan (the classic "Halloween problem"). Each change then updates the table B-tree and every index: delete the old index entries, insert the new ones, and check unique constraints.

## Parameter type inference

Prepared statements arrive as `SELECT * FROM users WHERE id = $1`, and the driver asks "what type is `$1`?" so it can encode the value. OakDB infers it from context, as PostgreSQL does: compared with an integer column, it is an integer; in `INSERT INTO t (name) VALUES ($1)` it takes `name`'s type; in `LIMIT $1` it is an integer; otherwise it is text.
