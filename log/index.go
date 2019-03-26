package log

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var byteOrder = binary.LittleEndian

type index struct {
	cap      uint64
	n        uint64
	dataSize int64
	f        *mmapFile
	dirty    bool
}

func newIndex(file string, cap uint64) (*index, error) {
	exists, err := fileExists(file)
	if err != nil {
		return nil, err
	}
	if !exists {
		fileSize := (int64(cap) + 2) * 8
		if err = createFile(file, fileSize, make([]byte, 16)); err != nil {
			return nil, err
		}
	}

	// fix cap if necessary
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("log: stat %s: %v", file, err)
	}
	if fcap := uint64(indexCap(int(info.Size()))); fcap > cap {
		cap = fcap
	}

	f, err := openFile(file)
	if err != nil {
		return nil, err
	}
	n := f.readUint64(0)

	idx := &index{cap: cap, n: n, f: f}
	idx.dataSize = idx.offset(idx.n)
	return idx, nil
}

func (idx *index) offset(i uint64) int64 {
	return int64(idx.f.readUint64((i + 1) * 8))
}

func (idx *index) isFull() bool {
	return idx.n == idx.cap
}

func (idx *index) append(newEntrySize int) error {
	off := idx.dataSize + int64(newEntrySize)
	if err := idx.f.writeUint64(uint64(off), int64((idx.n+2)*8)); err != nil {
		return err
	}
	idx.n++
	idx.dataSize = off
	idx.dirty = true
	return nil
}

func (idx *index) truncate(n uint64) error {
	if n >= 0 && n < idx.n {
		if err := idx.f.writeUint64(n, 0); err != nil {
			return err
		}
		off := idx.offset(n)
		idx.n = n
		idx.dataSize = off
	}
	return nil
}

func (idx *index) sync() error {
	if idx.dirty {
		if err := idx.f.syncData(); err != nil {
			return err
		}
		if err := idx.f.writeUint64(idx.n, 0); err != nil {
			return err
		}
		if err := idx.f.syncData(); err != nil {
			return err
		}
		idx.dirty = false
	}
	return nil
}

func (idx *index) close() error {
	err := idx.sync()
	if e := idx.f.Close(); err == nil {
		err = e
	}
	return err
}

func (idx *index) remove() error {
	return os.Remove(idx.f.Name())
}

// helpers -------------------------------------

func indexFile(dir string, off uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.index", off))
}

// indexes return list of indexes by its offset in increasing order.
func indexes(dir string) ([]uint64, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.index"))
	if err != nil {
		return nil, err
	}
	var offs []uint64
	for _, m := range matches {
		m = filepath.Base(m)
		m = strings.TrimSuffix(m, ".index")
		i, err := strconv.ParseUint(m, 10, 64)
		if err != nil {
			return nil, err
		}
		offs = append(offs, i)
	}
	sort.Slice(offs, func(i, j int) bool {
		return offs[i] < offs[j]
	})
	return offs, nil
}

func indexCap(fileSize int) int {
	return fileSize/8 - 2
}
