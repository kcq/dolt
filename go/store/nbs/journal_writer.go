// Copyright 2022 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nbs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/dolthub/dolt/go/store/hash"
)

const (
	chunkJournalFileSize = 256 * 1024 * 1024

	// todo(andy): buffer must be able to hold an entire record,
	//   but we don't have a hard limit on record size right now
	journalWriterBuffSize = 1024 * 1024

	chunkJournalAddr = "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"
)

var (
	journalAddr = addr(hash.Parse(chunkJournalAddr))
)

func isJournalAddr(a addr) bool {
	return a == journalAddr
}

func journalFileExists(path string) (bool, error) {
	var err error
	if path, err = filepath.Abs(path); err != nil {
		return false, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if info.IsDir() {
		return true, fmt.Errorf("expected file %s found directory", chunkJournalName)
	}
	return true, nil
}

func openJournalWriter(ctx context.Context, path string) (wr *journalWriter, exists bool, err error) {
	var f *os.File
	if path, err = filepath.Abs(path); err != nil {
		return nil, false, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	} else if info.IsDir() {
		return nil, true, fmt.Errorf("expected file %s found directory", chunkJournalName)
	}
	if f, err = os.OpenFile(path, os.O_RDWR, 0666); err != nil {
		return nil, true, err
	}

	return &journalWriter{
		buf:     make([]byte, 0, journalWriterBuffSize),
		lookups: make(map[addr]recLookup),
		file:    f,
		path:    path,
	}, true, nil
}

func createJournalWriter(ctx context.Context, path string) (wr *journalWriter, err error) {
	var f *os.File
	if path, err = filepath.Abs(path); err != nil {
		return nil, err
	}

	_, err = os.Stat(path)
	if err == nil {
		return nil, fmt.Errorf("journal file %s already exists", chunkJournalName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666); err != nil {
		return nil, err
	}
	const batch = 1024 * 1024
	b := make([]byte, batch)
	for i := 0; i < chunkJournalFileSize; i += batch {
		if _, err = f.Write(b); err != nil { // zero fill |f|
			return nil, err
		}
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if o, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	} else if o != 0 {
		return nil, fmt.Errorf("expected file journalOffset 0, got %d", o)
	}

	return &journalWriter{
		buf:     make([]byte, 0, journalWriterBuffSize),
		lookups: make(map[addr]recLookup),
		file:    f,
		path:    path,
	}, nil
}

type snapshotReader interface {
	io.ReaderAt
	// Snapshot returns an io.Reader that provides a consistent view
	// of the current state of the snapshotReader.
	Snapshot() (io.Reader, int64, error)

	// currentSize returns the current size.
	CurrentSize() int64
}

type journalWriter struct {
	buf     []byte
	lookups map[addr]recLookup
	file    *os.File
	off     int64
	uncmpSz uint64
	path    string
	lock    sync.RWMutex
}

var _ io.WriteCloser = &journalWriter{}
var _ snapshotReader = &journalWriter{}

func (wr *journalWriter) ReadAt(p []byte, off int64) (n int, err error) {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	var bp []byte
	if off < wr.off {
		// fill some or all of |p| from |wr.file|
		fread := int(wr.off - off)
		if len(p) > fread {
			// straddled read
			bp = p[fread:]
			p = p[:fread]
		}
		if n, err = wr.file.ReadAt(p, off); err != nil {
			return 0, err
		}
		off = 0
	} else {
		// fill all of |p| from |wr.buf|
		bp = p
		off -= wr.off
	}
	n += copy(bp, wr.buf[off:])
	return
}

func (wr *journalWriter) Snapshot() (io.Reader, int64, error) {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	if err := wr.flush(); err != nil {
		return nil, 0, err
	}
	// open a new file descriptor with an
	// independent lifecycle from |wr.file|
	f, err := os.Open(wr.path)
	if err != nil {
		return nil, 0, err
	}
	return io.LimitReader(f, wr.off), wr.off, nil
}

func (wr *journalWriter) CurrentSize() int64 {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	return wr.off
}

// todo: remove, this method is only used for testing
func (wr *journalWriter) Write(p []byte) (n int, err error) {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	if len(p) > len(wr.buf) {
		// write directly to |wr.file|
		if err = wr.flush(); err != nil {
			return 0, err
		}
		n, err = wr.file.WriteAt(p, wr.off)
		wr.off += int64(n)
		return
	}
	var buf []byte
	if buf, err = wr.getBytes(len(p)); err != nil {
		return 0, err
	}
	n = copy(buf, p)
	return
}

func (wr *journalWriter) ProcessJournal(ctx context.Context) (last hash.Hash, err error) {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	wr.off, err = processJournalRecords(ctx, wr.file, func(o int64, r journalRec) error {
		switch r.kind {
		case chunkJournalRecKind:
			wr.lookups[r.address] = recLookup{
				journalOff: o,
				recordLen:  r.length,
				payloadOff: r.payloadOffset(),
			}
			wr.uncmpSz += r.uncompressedPayloadSize()
		case rootHashJournalRecKind:
			last = hash.Hash(r.address)
		default:
			return fmt.Errorf("unknown journal record kind (%d)", r.kind)
		}
		return nil
	})
	if err != nil {
		return hash.Hash{}, err
	}
	return
}

func (wr *journalWriter) WriteChunk(cc CompressedChunk) error {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	l, o := chunkRecordSize(cc)
	rec := recLookup{
		journalOff: wr.offset(),
		recordLen:  l,
		payloadOff: o,
	}
	buf, err := wr.getBytes(int(rec.recordLen))
	if err != nil {
		return err
	}
	_ = writeChunkRecord(buf, cc)
	wr.lookups[addr(cc.H)] = rec
	return nil
}

func (wr *journalWriter) WriteRootHash(root hash.Hash) error {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	buf, err := wr.getBytes(rootHashRecordSize())
	if err != nil {
		return err
	}
	_ = writeRootHashRecord(buf, addr(root))

	if err = wr.flush(); err != nil {
		return err
	}
	return wr.file.Sync()
}

func (wr *journalWriter) Close() (err error) {
	wr.lock.Lock()
	defer wr.lock.Unlock()
	if err = wr.flush(); err != nil {
		return err
	}
	if cerr := wr.file.Sync(); cerr != nil {
		err = cerr
	}
	if cerr := wr.file.Close(); cerr != nil {
		err = cerr
	}
	return
}

func (wr *journalWriter) offset() int64 {
	return wr.off + int64(len(wr.buf))
}

func (wr *journalWriter) getBytes(n int) (buf []byte, err error) {
	c, l := cap(wr.buf), len(wr.buf)
	if n > c {
		err = fmt.Errorf("requested bytes (%d) exceeds capacity (%d)", n, c)
		return
	} else if n > c-l {
		if err = wr.flush(); err != nil {
			return
		}
	}
	l = len(wr.buf)
	wr.buf = wr.buf[:l+n]
	buf = wr.buf[l : l+n]
	return
}

func (wr *journalWriter) flush() (err error) {
	if _, err = wr.file.WriteAt(wr.buf, wr.off); err != nil {
		return err
	}
	wr.off += int64(len(wr.buf))
	wr.buf = wr.buf[:0]
	return
}

func (wr *journalWriter) has(h addr) (ok bool) {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	_, ok = wr.lookups[h]
	return
}

func (wr *journalWriter) getRange(h addr) (rng Range, ok bool) {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	var l recLookup
	l, ok = wr.lookups[h]
	if ok {
		rng = rangeFromLookup(l)
	}
	return
}

func (wr *journalWriter) getCompressed(h addr) (CompressedChunk, error) {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	l, ok := wr.lookups[h]
	if !ok {
		return CompressedChunk{}, nil
	}

	buf := make([]byte, l.recordLen)
	if _, err := wr.ReadAt(buf, l.journalOff); err != nil {
		return CompressedChunk{}, nil
	}

	rec, err := readJournalRecord(buf)
	if err != nil {
		return CompressedChunk{}, err
	} else if h != rec.address {
		err = fmt.Errorf("chunk record hash does not match (%s != %s)",
			h.String(), rec.address.String())
		return CompressedChunk{}, err
	}
	return NewCompressedChunk(hash.Hash(h), rec.payload)
}

func (wr *journalWriter) recordCount() uint32 {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	return uint32(len(wr.lookups))
}

func (wr *journalWriter) uncompressedSize() uint64 {
	wr.lock.RLock()
	defer wr.lock.RUnlock()
	return wr.uncmpSz
}
