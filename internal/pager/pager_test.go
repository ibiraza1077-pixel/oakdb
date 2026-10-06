package pager

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string, opts Options) *Pager {
	t.Helper()
	opts.NoSync = true
	p, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeVal(t *testing.T, tx *WriteTx, n uint32, v uint32) {
	t.Helper()
	b, err := tx.Mutable(n)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(b[100:], v)
}

func readVal(t *testing.T, get func(uint32) ([]byte, error), n uint32) uint32 {
	t.Helper()
	b, err := get(n)
	if err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint32(b[100:])
}

func TestCommitRollbackAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := open(t, path, Options{})

	tx := p.BeginWrite()
	n, err := tx.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	writeVal(t, tx, n, 42)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx = p.BeginWrite()
	writeVal(t, tx, n, 99)
	tx.Rollback()

	s := p.BeginRead()
	if got := readVal(t, s.Page, n); got != 42 {
		t.Fatalf("after rollback got %d, want 42", got)
	}
	s.Release()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p = open(t, path, Options{})
	defer p.Close()
	s = p.BeginRead()
	defer s.Release()
	if got := readVal(t, s.Page, n); got != 42 {
		t.Fatalf("after reopen got %d, want 42", got)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	p := open(t, filepath.Join(t.TempDir(), "db"), Options{})
	defer p.Close()
	tx := p.BeginWrite()
	n, _ := tx.Alloc()
	writeVal(t, tx, n, 1)
	tx.Commit()

	old := p.BeginRead()
	tx = p.BeginWrite()
	writeVal(t, tx, n, 2)
	// Uncommitted changes are invisible to readers.
	if got := readVal(t, old.Page, n); got != 1 {
		t.Fatalf("reader saw uncommitted value %d", got)
	}
	tx.Commit()
	// Committed changes made after the snapshot are invisible too.
	if got := readVal(t, old.Page, n); got != 1 {
		t.Fatalf("old snapshot saw newer value %d", got)
	}
	// Upgrading the stale snapshot must fail.
	if _, err := old.Upgrade(); !errors.Is(err, ErrSerialization) {
		t.Fatalf("upgrade of stale snapshot: %v", err)
	}
	fresh := p.BeginRead()
	defer fresh.Release()
	if got := readVal(t, fresh.Page, n); got != 2 {
		t.Fatalf("new snapshot got %d, want 2", got)
	}
}

func TestFreeListReusesPages(t *testing.T) {
	p := open(t, filepath.Join(t.TempDir(), "db"), Options{})
	defer p.Close()
	tx := p.BeginWrite()
	a, _ := tx.Alloc()
	b, _ := tx.Alloc()
	if err := tx.Free(a); err != nil {
		t.Fatal(err)
	}
	c, _ := tx.Alloc()
	if c != a {
		t.Fatalf("expected freed page %d to be reused, got %d (b=%d)", a, c, b)
	}
	tx.Commit()
}

func TestCheckpointKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := open(t, path, Options{CheckpointFrames: 10, CachePages: 4})
	var pages []uint32
	for i := 0; i < 50; i++ {
		tx := p.BeginWrite()
		n, _ := tx.Alloc()
		writeVal(t, tx, n, uint32(i))
		pages = append(pages, n)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if p.Stats().Checkpoints == 0 {
		t.Fatal("expected at least one checkpoint")
	}
	s := p.BeginRead()
	for i, n := range pages {
		if got := readVal(t, s.Page, n); got != uint32(i) {
			t.Fatalf("page %d: got %d want %d", n, got, i)
		}
	}
	s.Release()
	p.Close()
}

// TestRecoveryFromTornWAL simulates a crash at every possible byte offset in
// the WAL: whatever survives, the database must open and show exactly a
// prefix of the committed transactions.
func TestRecoveryFromTornWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	p := open(t, path, Options{CheckpointFrames: 1 << 30})
	tx := p.BeginWrite()
	n, _ := tx.Alloc()
	writeVal(t, tx, n, 0)
	tx.Commit()
	// Force the first commit into the main file so the WAL only holds the
	// later commits.
	p.mu.Lock()
	if err := p.checkpointLocked(); err != nil {
		t.Fatal(err)
	}
	p.mu.Unlock()
	const commits = 5
	for i := 1; i <= commits; i++ {
		tx := p.BeginWrite()
		writeVal(t, tx, n, uint32(i))
		tx.Commit()
	}
	walBytes, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	dbBytes, _ := os.ReadFile(path)
	// Abandon p without closing it, as a crash would.

	rng := rand.New(rand.NewSource(1))
	cuts := []int{0, walHeaderSize, len(walBytes)}
	for i := 0; i < 40; i++ {
		cuts = append(cuts, rng.Intn(len(walBytes)+1))
	}
	for _, cut := range cuts {
		d := t.TempDir()
		cp := filepath.Join(d, "db")
		os.WriteFile(cp, dbBytes, 0o644)
		os.WriteFile(cp+"-wal", walBytes[:cut], 0o644)
		q := open(t, cp, Options{})
		s := q.BeginRead()
		got := readVal(t, s.Page, n)
		s.Release()
		q.Close()
		full := 0
		if cut > walHeaderSize {
			full = (cut - walHeaderSize) / frameSize
		}
		// Every commit here touches exactly one page, so committed
		// transactions = complete frames.
		if int(got) != full {
			t.Fatalf("cut at %d: recovered value %d, want %d", cut, got, full)
		}
	}

	// A flipped bit in the last frame must invalidate only that frame.
	corrupt := append([]byte(nil), walBytes...)
	corrupt[len(corrupt)-10] ^= 0xFF
	d := t.TempDir()
	cp := filepath.Join(d, "db")
	os.WriteFile(cp, dbBytes, 0o644)
	os.WriteFile(cp+"-wal", corrupt, 0o644)
	q := open(t, cp, Options{})
	s := q.BeginRead()
	if got := readVal(t, s.Page, n); got != commits-1 {
		t.Fatalf("corrupt last frame: got %d want %d", got, commits-1)
	}
	s.Release()
	q.Close()
}
