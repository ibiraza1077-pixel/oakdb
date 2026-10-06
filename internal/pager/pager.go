// Package pager stores the database as fixed-size pages and makes changes
// to them atomic and durable.
//
// Design (the same idea as SQLite's WAL mode):
//
//   - The database file is an array of 4 KB pages. Page 0 is the header.
//   - A write transaction never touches the database file. It copies the
//     pages it changes into a private "dirty" map (copy-on-write).
//   - On commit, every dirty page is appended to the write-ahead log (WAL)
//     as a checksummed frame. The last frame carries a commit flag. One
//     fsync of the WAL makes the transaction durable.
//   - Readers see a snapshot: "the newest committed frame for each page that
//     existed when I started", falling back to the database file. Committed
//     page images never change, so readers need no locks while reading.
//   - When the WAL grows large and no reader is active, a checkpoint copies
//     the newest version of each page into the database file and resets the
//     WAL.
//   - On open, the WAL is scanned. Frames are kept up to the last frame with
//     a valid commit flag and valid checksum chain; anything after that was
//     an unfinished transaction and is discarded.
//
// Only one write transaction runs at a time; any number of readers run in
// parallel with it.
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// PageSize is the size of every page in bytes.
const PageSize = 4096

const (
	headerMagic = "OAKDB001"
	// Offsets of the header fields inside page 0.
	offMagic       = 0
	offPageSize    = 8
	offPageCount   = 12
	offFreeList    = 16
	offCatalogRoot = 20
	offSchemaToken = 24
)

// ErrSerialization is returned when a read transaction tries to become a
// write transaction after another transaction has committed. Its snapshot is
// stale, so the caller must retry.
var ErrSerialization = errors.New("could not serialize access due to a concurrent update")

// Options tune a Pager.
type Options struct {
	// NoSync skips fsync. Only for tests and benchmarks: a power cut can
	// lose committed transactions (a process crash cannot).
	NoSync bool
	// CachePages is the buffer pool size in pages. Default 4096 (16 MB).
	CachePages int
	// CheckpointFrames is the WAL size, in frames, that triggers a
	// checkpoint. Default 1000.
	CheckpointFrames int
}

// Pager owns the database file and the WAL.
type Pager struct {
	opts Options

	db  *os.File
	wal *wal

	writeMu sync.Mutex // held for the whole of a write transaction

	mu      sync.Mutex          // guards the fields below
	index   map[uint32][]uint64 // page -> committed WAL frame numbers, ascending
	frames  uint64              // number of committed frames in the WAL
	readers int                 // active read snapshots
	cache   *lru
	dbPages uint32 // pages present in the database file
	closed  bool
	stats   Stats
}

// Stats are counters exposed on the monitoring page.
type Stats struct {
	CacheHits   uint64
	CacheMisses uint64
	Commits     uint64
	Rollbacks   uint64
	Checkpoints uint64
	WALFrames   uint64
	PageCount   uint32
	CachedPages int
	ActiveReads int
}

// Open opens or creates the database at path. The WAL lives next to it in
// path + "-wal".
func Open(path string, opts Options) (*Pager, error) {
	if opts.CachePages <= 0 {
		opts.CachePages = 4096
	}
	if opts.CheckpointFrames <= 0 {
		opts.CheckpointFrames = 1000
	}
	db, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	p := &Pager{opts: opts, db: db, index: map[uint32][]uint64{}, cache: newLRU(opts.CachePages)}
	if err := p.initFile(); err != nil {
		db.Close()
		return nil, err
	}
	w, frames, err := openWAL(path+"-wal", !opts.NoSync)
	if err != nil {
		db.Close()
		return nil, err
	}
	p.wal = w
	for i, f := range frames {
		p.index[f] = append(p.index[f], uint64(i+1))
	}
	p.frames = uint64(len(frames))
	// Fold any recovered frames into the database file straight away so the
	// WAL starts empty.
	if p.frames > 0 {
		if err := p.checkpointLocked(); err != nil {
			p.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Pager) initFile() error {
	info, err := p.db.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		page := make([]byte, PageSize)
		copy(page[offMagic:], headerMagic)
		binary.LittleEndian.PutUint32(page[offPageSize:], PageSize)
		binary.LittleEndian.PutUint32(page[offPageCount:], 1)
		if _, err := p.db.WriteAt(page, 0); err != nil {
			return err
		}
		if err := p.sync(p.db); err != nil {
			return err
		}
		p.dbPages = 1
		return nil
	}
	page := make([]byte, PageSize)
	if _, err := p.db.ReadAt(page, 0); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	if string(page[offMagic:offMagic+8]) != headerMagic {
		return errors.New("not an OakDB database file")
	}
	if binary.LittleEndian.Uint32(page[offPageSize:]) != PageSize {
		return errors.New("unsupported page size")
	}
	p.dbPages = uint32(info.Size() / PageSize)
	return nil
}

func (p *Pager) sync(f *os.File) error {
	if p.opts.NoSync {
		return nil
	}
	return f.Sync()
}

// Close checkpoints and closes the files.
func (p *Pager) Close() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var err error
	if p.frames > 0 && p.readers == 0 {
		err = p.checkpointLocked()
	}
	if e := p.wal.close(); err == nil {
		err = e
	}
	if e := p.db.Close(); err == nil {
		err = e
	}
	return err
}

// Stats returns a copy of the counters.
func (p *Pager) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.WALFrames = p.frames
	s.CachedPages = p.cache.len()
	s.ActiveReads = p.readers
	if page, err := p.readLocked(0, p.frames); err == nil {
		s.PageCount = binary.LittleEndian.Uint32(page[offPageCount:])
	}
	return s
}

// readLocked returns the committed image of page n as of maxFrame. The
// returned slice must not be modified. p.mu must be held.
func (p *Pager) readLocked(n uint32, maxFrame uint64) ([]byte, error) {
	var frame uint64
	if fs := p.index[n]; len(fs) > 0 {
		// Newest frame not after the snapshot. Frames are ascending.
		for i := len(fs) - 1; i >= 0; i-- {
			if fs[i] <= maxFrame {
				frame = fs[i]
				break
			}
		}
	}
	key := cacheKey{page: n, frame: frame}
	if b, ok := p.cache.get(key); ok {
		p.stats.CacheHits++
		return b, nil
	}
	p.stats.CacheMisses++
	b := make([]byte, PageSize)
	if frame > 0 {
		if err := p.wal.readFrame(frame, b); err != nil {
			return nil, err
		}
	} else if n < p.dbPages {
		if _, err := p.db.ReadAt(b, int64(n)*PageSize); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	p.cache.put(key, b)
	return b, nil
}

// --- read transactions -----------------------------------------------------

// Snapshot is a consistent, read-only view of the database.
type Snapshot struct {
	p        *Pager
	maxFrame uint64
	done     atomic.Bool
}

// BeginRead starts a read snapshot. Call Release when finished.
func (p *Pager) BeginRead() *Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readers++
	return &Snapshot{p: p, maxFrame: p.frames}
}

// Page returns page n. The slice is shared and must not be modified.
func (s *Snapshot) Page(n uint32) ([]byte, error) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	return s.p.readLocked(n, s.maxFrame)
}

// Release ends the snapshot.
func (s *Snapshot) Release() {
	if s.done.Swap(true) {
		return
	}
	s.p.mu.Lock()
	s.p.readers--
	s.p.mu.Unlock()
}

// Upgrade turns the snapshot into a write transaction. It fails with
// ErrSerialization if anything committed since the snapshot was taken. The
// snapshot is released either way.
func (s *Snapshot) Upgrade() (*WriteTx, error) {
	s.p.writeMu.Lock()
	s.p.mu.Lock()
	stale := s.p.frames != s.maxFrame
	s.p.mu.Unlock()
	s.Release()
	if stale {
		s.p.writeMu.Unlock()
		return nil, ErrSerialization
	}
	return s.p.newWriteTx(), nil
}

// --- write transactions ----------------------------------------------------

// WriteTx is the single active write transaction.
type WriteTx struct {
	p        *Pager
	maxFrame uint64
	dirty    map[uint32][]byte
	order    []uint32
	done     bool
}

// BeginWrite waits for any other writer to finish and starts a write
// transaction on the latest committed state.
func (p *Pager) BeginWrite() *WriteTx {
	p.writeMu.Lock()
	return p.newWriteTx()
}

func (p *Pager) newWriteTx() *WriteTx {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &WriteTx{p: p, maxFrame: p.frames, dirty: map[uint32][]byte{}}
}

// Page returns page n for reading. Within a write transaction it reflects
// the transaction's own changes.
func (t *WriteTx) Page(n uint32) ([]byte, error) {
	if b, ok := t.dirty[n]; ok {
		return b, nil
	}
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	return t.p.readLocked(n, t.maxFrame)
}

// Mutable returns a private, writable copy of page n.
func (t *WriteTx) Mutable(n uint32) ([]byte, error) {
	if b, ok := t.dirty[n]; ok {
		return b, nil
	}
	src, err := t.Page(n)
	if err != nil {
		return nil, err
	}
	b := make([]byte, PageSize)
	copy(b, src)
	t.dirty[n] = b
	t.order = append(t.order, n)
	return b, nil
}

func (t *WriteTx) header() ([]byte, error) { return t.Mutable(0) }

// PageCount is the number of pages in the database, including free ones.
func (t *WriteTx) PageCount() (uint32, error) {
	h, err := t.Page(0)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(h[offPageCount:]), nil
}

// Alloc returns a zeroed page, reusing a free page when one exists.
func (t *WriteTx) Alloc() (uint32, error) {
	h, err := t.header()
	if err != nil {
		return 0, err
	}
	if free := binary.LittleEndian.Uint32(h[offFreeList:]); free != 0 {
		b, err := t.Mutable(free)
		if err != nil {
			return 0, err
		}
		binary.LittleEndian.PutUint32(h[offFreeList:], binary.LittleEndian.Uint32(b[0:4]))
		clear(b)
		return free, nil
	}
	n := binary.LittleEndian.Uint32(h[offPageCount:])
	binary.LittleEndian.PutUint32(h[offPageCount:], n+1)
	t.dirty[n] = make([]byte, PageSize)
	t.order = append(t.order, n)
	return n, nil
}

// Free puts page n on the free list.
func (t *WriteTx) Free(n uint32) error {
	if n == 0 {
		return errors.New("cannot free the header page")
	}
	h, err := t.header()
	if err != nil {
		return err
	}
	b, err := t.Mutable(n)
	if err != nil {
		return err
	}
	clear(b)
	binary.LittleEndian.PutUint32(b[0:4], binary.LittleEndian.Uint32(h[offFreeList:]))
	binary.LittleEndian.PutUint32(h[offFreeList:], n)
	return nil
}

// CatalogRoot returns the root page of the system catalog (0 if none yet).
func (t *WriteTx) CatalogRoot() (uint32, error) {
	h, err := t.Page(0)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(h[offCatalogRoot:]), nil
}

// SetCatalogRoot records the root page of the system catalog.
func (t *WriteTx) SetCatalogRoot(root uint32) error {
	h, err := t.header()
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(h[offCatalogRoot:], root)
	return nil
}

// SetSchemaToken stores a new schema version token. Readers use it to know
// when their cached catalog is out of date.
func (t *WriteTx) SetSchemaToken(tok uint64) error {
	h, err := t.header()
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(h[offSchemaToken:], tok)
	return nil
}

// Commit makes the transaction's changes durable and visible.
func (t *WriteTx) Commit() error {
	if t.done {
		return errors.New("transaction already finished")
	}
	t.done = true
	p := t.p
	defer p.writeMu.Unlock()
	if len(t.order) == 0 {
		return nil
	}
	pages := make([][]byte, len(t.order))
	for i, n := range t.order {
		pages[i] = t.dirty[n]
	}
	p.mu.Lock()
	first := p.frames + 1
	p.mu.Unlock()
	if err := p.wal.appendCommit(first, t.order, pages); err != nil {
		return fmt.Errorf("write-ahead log: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, n := range t.order {
		f := first + uint64(i)
		p.index[n] = append(p.index[n], f)
		p.cache.put(cacheKey{page: n, frame: f}, pages[i])
	}
	p.frames += uint64(len(t.order))
	p.stats.Commits++
	if p.frames >= uint64(p.opts.CheckpointFrames) && p.readers == 0 {
		if err := p.checkpointLocked(); err != nil {
			// The commit itself is durable in the WAL; a failed checkpoint
			// is retried on the next commit or on open.
			return nil
		}
	}
	return nil
}

// Rollback discards the transaction's changes.
func (t *WriteTx) Rollback() {
	if t.done {
		return
	}
	t.done = true
	t.p.mu.Lock()
	t.p.stats.Rollbacks++
	t.p.mu.Unlock()
	t.p.writeMu.Unlock()
}

// Done reports whether the transaction has been committed or rolled back.
func (t *WriteTx) Done() bool { return t.done }

// checkpointLocked copies the newest version of every page from the WAL into
// the database file, then resets the WAL. Caller holds p.mu, no readers are
// active and no write transaction is mid-flight (or it is the caller).
func (p *Pager) checkpointLocked() error {
	buf := make([]byte, PageSize)
	maxPage := p.dbPages
	for n, fs := range p.index {
		if err := p.wal.readFrame(fs[len(fs)-1], buf); err != nil {
			return err
		}
		if _, err := p.db.WriteAt(buf, int64(n)*PageSize); err != nil {
			return err
		}
		if n+1 > maxPage {
			maxPage = n + 1
		}
	}
	if err := p.sync(p.db); err != nil {
		return err
	}
	// Only after the database file is durable may the WAL be reset. If we
	// crash before this point, recovery simply replays the same frames.
	if err := p.wal.reset(); err != nil {
		return err
	}
	p.dbPages = maxPage
	p.index = map[uint32][]uint64{}
	p.frames = 0
	p.cache.clear()
	p.stats.Checkpoints++
	return nil
}

// Meta is the database header stored in page 0.
type Meta struct {
	PageCount   uint32
	FreeList    uint32
	CatalogRoot uint32
	SchemaToken uint64
}

// ReadMeta decodes the header from page 0.
func ReadMeta(page0 []byte) Meta {
	return Meta{
		PageCount:   binary.LittleEndian.Uint32(page0[offPageCount:]),
		FreeList:    binary.LittleEndian.Uint32(page0[offFreeList:]),
		CatalogRoot: binary.LittleEndian.Uint32(page0[offCatalogRoot:]),
		SchemaToken: binary.LittleEndian.Uint64(page0[offSchemaToken:]),
	}
}
