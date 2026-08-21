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

func (m *mapping) close() {
	if m.b == nil {
		return
	}
	_ = m.b.Unmap()
	m.b = nil
}
