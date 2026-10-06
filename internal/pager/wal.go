package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"hash/crc64"
	"os"
)

// WAL file layout
//
//	header (32 bytes): magic[8] pageSize u32 reserved u32 salt u64 reserved u64
//	frame:             pageNo u32 flags u32 salt u64 checksum u64 | page data
//
// The checksum is a running CRC-64 that starts at the salt and covers every
// frame header (first 16 bytes) and page body up to and including this frame.
// A frame is valid only if its salt matches the header and its checksum
// matches the chain. A torn or half-written frame therefore breaks the chain,
// and everything from that point on is ignored.
const (
	walMagic       = "OAKWAL01"
	walHeaderSize  = 32
	frameHeaderLen = 24
	frameSize      = frameHeaderLen + PageSize
	flagCommit     = 1
)

var crcTable = crc64.MakeTable(crc64.ECMA)

type wal struct {
	f        *os.File
	doSync   bool
	salt     uint64
	checksum uint64 // running checksum after the last committed frame
}

// openWAL opens the WAL and recovers it. It returns the page numbers of the
// committed frames, in frame order.
func openWAL(path string, doSync bool) (*wal, []uint32, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	w := &wal{f: f, doSync: doSync}
	frames, err := w.recover()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return w, frames, nil
}

func (w *wal) recover() ([]uint32, error) {
	info, err := w.f.Stat()
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, walHeaderSize)
	if info.Size() < walHeaderSize {
		return nil, w.reset()
	}
	if _, err := w.f.ReadAt(hdr, 0); err != nil {
		return nil, err
	}
	if string(hdr[:8]) != walMagic || binary.LittleEndian.Uint32(hdr[8:]) != PageSize {
		return nil, w.reset()
	}
	w.salt = binary.LittleEndian.Uint64(hdr[16:])

	var committed []uint32
	var pending []uint32
	sum := w.salt
	committedSum := sum
	buf := make([]byte, frameSize)
	n := (info.Size() - walHeaderSize) / frameSize
	for i := int64(0); i < n; i++ {
		if _, err := w.f.ReadAt(buf, walHeaderSize+i*frameSize); err != nil {
			break
		}
		if binary.LittleEndian.Uint64(buf[8:]) != w.salt {
			break
		}
		sum = crc64.Update(sum, crcTable, buf[:16])
		sum = crc64.Update(sum, crcTable, buf[frameHeaderLen:])
		if binary.LittleEndian.Uint64(buf[16:]) != sum {
			break
		}
		pending = append(pending, binary.LittleEndian.Uint32(buf[0:]))
		if binary.LittleEndian.Uint32(buf[4:])&flagCommit != 0 {
			committed = append(committed, pending...)
			pending = pending[:0]
			committedSum = sum
		}
	}
	w.checksum = committedSum
	// Drop the tail of any unfinished transaction so new frames append
	// cleanly after the last commit.
	if err := w.f.Truncate(walHeaderSize + int64(len(committed))*frameSize); err != nil {
		return nil, err
	}
	return committed, nil
}

// reset empties the WAL and gives it a new salt, so stale frames left over
// from an earlier generation can never be mistaken for valid ones.
func (w *wal) reset() error {
	var s [8]byte
	if _, err := rand.Read(s[:]); err != nil {
		return err
	}
	w.salt = binary.LittleEndian.Uint64(s[:])
	w.checksum = w.salt
	hdr := make([]byte, walHeaderSize)
	copy(hdr, walMagic)
	binary.LittleEndian.PutUint32(hdr[8:], PageSize)
	binary.LittleEndian.PutUint64(hdr[16:], w.salt)
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	if _, err := w.f.WriteAt(hdr, 0); err != nil {
		return err
	}
	if w.doSync {
		return w.f.Sync()
	}
	return nil
}

// appendCommit writes one transaction's pages as frames starting at frame
// number first, marks the last one as the commit, and fsyncs.
func (w *wal) appendCommit(first uint64, pageNos []uint32, pages [][]byte) error {
	if len(pageNos) == 0 {
		return errors.New("empty commit")
	}
	buf := make([]byte, len(pageNos)*frameSize)
	sum := w.checksum
	for i, n := range pageNos {
		fr := buf[i*frameSize : (i+1)*frameSize]
		binary.LittleEndian.PutUint32(fr[0:], n)
		if i == len(pageNos)-1 {
			binary.LittleEndian.PutUint32(fr[4:], flagCommit)
		}
		binary.LittleEndian.PutUint64(fr[8:], w.salt)
		copy(fr[frameHeaderLen:], pages[i])
		sum = crc64.Update(sum, crcTable, fr[:16])
		sum = crc64.Update(sum, crcTable, fr[frameHeaderLen:])
		binary.LittleEndian.PutUint64(fr[16:], sum)
	}
	off := walHeaderSize + int64(first-1)*frameSize
	if _, err := w.f.WriteAt(buf, off); err != nil {
		w.f.Truncate(off) // best effort: remove the partial write
		return err
	}
	if w.doSync {
		if err := w.f.Sync(); err != nil {
			w.f.Truncate(off)
			return err
		}
	}
	w.checksum = sum
	return nil
}

func (w *wal) readFrame(frame uint64, dst []byte) error {
	_, err := w.f.ReadAt(dst[:PageSize], walHeaderSize+int64(frame-1)*frameSize+frameHeaderLen)
	return err
}

func (w *wal) close() error { return w.f.Close() }
