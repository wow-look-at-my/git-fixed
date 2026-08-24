package odb

import (
	"errors"
	"fmt"

	mmap "github.com/wow-look-at-my/go-mmap"
)

// mapping is one read-only view of a pack or index file.
type mapping struct {
	b mmap.MMap
}

func mapReadOnly(path string) (mapping, error) {
	m, err := mmap.MapFile(path)
	if err != nil {
		if errors.Is(err, mmap.ErrZeroLength) {
			return mapping{}, fmt.Errorf("%s is empty", path)
		}
		return mapping{}, err
	}
	return mapping{b: m}, nil
}

func (m *mapping) bytes() []byte { return m.b }

// release drops this process's copy of the mapped pages. A read after it faults
// the page back from the page cache. see docs/memory.md
func (m *mapping) release() {
	if m.b == nil {
		return
	}
	_ = m.b.Advise(mmap.AdvDontNeed)
}

func (m *mapping) close() {
	if m.b == nil {
		return
	}
	_ = m.b.Unmap()
	m.b = nil
}
