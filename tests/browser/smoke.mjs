// Smoke test for the WebAssembly build that powers the browser playground.
// It loads the same files the page loads (memfs.js, wasm_exec.js, oakdb.wasm),
// runs SQL through the real engine, then kills the process after tearing the
// WAL mid-commit and checks that recovery keeps exactly the committed prefix.
//
//   GOOS=js GOARCH=wasm go build -o docs/demo/oakdb.wasm ./cmd/oakdb
//   node tests/browser/smoke.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const demo = fileURLToPath(new URL('../../docs/demo/', import.meta.url));

// The page's globals, minus the DOM.
const ready = new Promise((resolve) => {
  globalThis.dispatchEvent = (e) => e.type === 'oakdb-ready' && resolve();
});
globalThis.Event = class { constructor(type) { this.type = type; } };
vm.runInThisContext(readFileSync(demo + 'memfs.js', 'utf8'));
vm.runInThisContext(readFileSync(demo + 'wasm_exec.js', 'utf8'));

const go = new Go();
const { instance } = await WebAssembly.instantiate(readFileSync(demo + 'oakdb.wasm'), go.importObject);
go.run(instance);
await ready;
const db = globalThis.oakdb;
assert.equal(db.error, undefined, `engine failed to open: ${db.error}`);
const q = (sql) => db.exec(sql);

let passed = 0;
const check = (name, fn) => { fn(); passed++; console.log(`ok  ${name}`); };

check('creates, indexes and inserts', () => {
  assert.match(q('CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, city TEXT);'), /CREATE TABLE/);
  assert.match(q('CREATE INDEX users_city ON users (city);'), /CREATE INDEX/);
  assert.match(q("INSERT INTO users (name, city) VALUES ('Ada','London'),('Linus','Helsinki'),('Alan','London');"), /INSERT 0 3/);
});

check('planner uses an index-only scan', () => {
  assert.match(q("EXPLAIN SELECT count(*) FROM users WHERE city = 'London';"), /Index Only Scan using users_city/);
});

check('aggregates', () => {
  assert.match(q('SELECT city, count(*) FROM users GROUP BY city ORDER BY 2 DESC;'), /London\s+\|\s+2/);
});

check('rollback discards work', () => {
  q('BEGIN; DELETE FROM users;');
  q('ROLLBACK;');
  assert.match(q('SELECT count(*) FROM users;'), /^\s+3\s*$/m);
});

check('errors carry PostgreSQL codes', () => {
  assert.match(q('SELECT * FROM missing;'), /ERROR \(42P01\)/);
});

check('torn WAL write: recovery keeps exactly the committed prefix', () => {
  q('CREATE TABLE log (id INTEGER PRIMARY KEY, txn INTEGER NOT NULL);');
  let lastStart = 0;
  for (let t = 1; t <= 5; t++) {
    lastStart = memfs.size('/oak.db-wal');
    q(`BEGIN; INSERT INTO log (txn) VALUES ${Array(10).fill(`(${t})`).join(',')}; COMMIT;`);
  }
  const end = memfs.size('/oak.db-wal');
  assert.ok(end > lastStart, 'expected the last transaction in the WAL');
  memfs.tear('/oak.db-wal', lastStart + Math.floor((end - lastStart) / 2));
  assert.equal(db.crash(), 'ok');
  const out = q('SELECT txn, count(*) FROM log GROUP BY txn ORDER BY txn;');
  const rows = [...out.matchAll(/^\s+(\d+)\s+\|\s+(\d+)\s*$/gm)].map((m) => [Number(m[1]), Number(m[2])]);
  assert.deepEqual(rows, [[1, 10], [2, 10], [3, 10], [4, 10]]);
  assert.match(q('SELECT count(*) FROM users;'), /^\s+3\s*$/m, 'earlier data must survive too');
});

check('clean kill keeps every commit', () => {
  q('INSERT INTO log (txn) VALUES (6);');
  assert.equal(db.crash(), 'ok');
  assert.match(q('SELECT count(*) FROM log;'), /^\s+41\s*$/m);
});

console.log(`\n${passed} passed`);
process.exit(0);
