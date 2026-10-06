"""Exercise OakDB through psycopg 3, the standard Python PostgreSQL driver."""
import sys

import psycopg

url = sys.argv[1]
with psycopg.connect(url, autocommit=True) as conn:
    cur = conn.cursor()
    cur.execute("CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INT, rating REAL)")
    cur.executemany(
        "INSERT INTO books (title, year, rating) VALUES (%s, %s, %s)",
        [("SICP", 1985, 4.7), ("CLRS", 1990, 4.5), ("DDIA", 2017, 4.8), ("TAOCP", 1968, None)],
    )
    cur.execute("SELECT title, year FROM books WHERE year > %s ORDER BY year", (1980,))
    assert cur.fetchall() == [("SICP", 1985), ("CLRS", 1990), ("DDIA", 2017)], "filtered rows"
    cur.execute("SELECT count(*), avg(rating), max(title) FROM books")
    count, avg, top = cur.fetchone()
    assert count == 4 and abs(avg - 4.666666) < 1e-3 and top == "TAOCP", (count, avg, top)
    cur.execute("SELECT rating FROM books WHERE title = %s", ("TAOCP",))
    assert cur.fetchone() == (None,), "NULL round trip"

    # Transactions through the driver's context manager.
    conn.autocommit = False
    try:
        with conn.transaction():
            cur.execute("DELETE FROM books")
            raise RuntimeError("roll me back")
    except RuntimeError:
        pass
    cur.execute("SELECT count(*) FROM books")
    assert cur.fetchone() == (4,), "rollback"

    # Errors carry PostgreSQL SQLSTATE codes.
    try:
        cur.execute("INSERT INTO books (id, title) VALUES (1, 'dup')")
        raise AssertionError("expected a unique violation")
    except psycopg.errors.UniqueViolation:
        conn.rollback()
print("psycopg ok")
