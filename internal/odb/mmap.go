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

// mapFileHint says how the caller reads the mapping, so the kernel reads ahead
// for a full pack scan and stays out of the way for index lookups.
type mapFileHint int

const (
	hintRandom mapFileHint = iota
	hintSequential
)

func mapReadOnly(path string, hint mapFileHint) (mapping, error) {
	m, err := mmap.MapFile(path)
	if err != nil {
		if errors.Is(err, mmap.ErrZeroLength) {
			return mapping{}, fmt.Errorf("%s is empty", path)
		}
		return mapping{}, err
	}
	advice := mmap.AdvRandom
	if hint == hintSequential {
		advice = mmap.AdvSequential
	}
	// Advice is a hint. A platform that declines it costs read-ahead, not
	// correctness, so a failure here must not fail the run.
	_ = m.Advise(advice)
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
