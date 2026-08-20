package fsckcmd

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// verifyMultiPackIndex checks one object directory's multi-pack-index, the way
// git's "multi-pack-index verify" does.
//
// see docs/multi-pack-index.md
func (r *run) verifyMultiPackIndex(objectDir string) bool {
	path := filepath.Join(objectDir, "pack", "multi-pack-index")
	data, err := os.ReadFile(path)
	if err != nil {
		return true // not having one is normal
	}
	key := sortKey{phase: phaseGraphs, group: 1}
	ok := true
	report := func(format string, args ...any) {
		ok = false
		r.rep.Errf(key, format, args...)
	}
	algo := r.repo.Algo
	rawsz := algo.RawSize
	if len(data) < 12+rawsz {
		r.rep.Errf(key, "error: multi-pack-index file %s is too small", path)
		return false
	}
	if string(data[0:4]) != "MIDX" {
		r.rep.Errf(key, "fatal: multi-pack-index signature 0x%08x does not match signature 0x%08x",
			binary.BigEndian.Uint32(data[0:4]), 0x4d494458)
		return false
	}
	if data[4] != 1 {
		r.rep.Errf(key, "fatal: multi-pack-index version %d not recognized", data[4])
		return false
	}
	if uint32(data[5]) != algo.Format {
		r.rep.Errf(key, "error: multi-pack-index hash version %d does not match version %d", data[5], algo.Format)
		return false
	}
	numChunks := int(data[6])
	numPacks := binary.BigEndian.Uint32(data[8:12])
	tableEnd := 12 + (numChunks+1)*12
	if len(data) < tableEnd+rawsz {
		r.rep.Errf(key, "error: multi-pack-index file %s is too small", path)
		return false
	}
	chunk := func(id string) []byte {
		for i := 0; i < numChunks; i++ {
			off := 12 + i*12
			if string(data[off:off+4]) != id {
				continue
			}
			start := binary.BigEndian.Uint64(data[off+4 : off+12])
			end := binary.BigEndian.Uint64(data[off+16 : off+24])
			if start > end || end > uint64(len(data)) {
				return nil
			}
			return data[start:end]
		}
		return nil
	}
	packNames := chunk("PNAM")
	fanout := chunk("OIDF")
	lookup := chunk("OIDL")
	offsets := chunk("OOFF")
	largeOffsets := chunk("LOFF")
	switch {
	case packNames == nil:
		r.rep.Errf(key, "fatal: multi-pack-index required pack-name chunk missing or corrupted")
		return false
	case len(fanout) < 256*4:
		r.rep.Errf(key, "fatal: multi-pack-index required OID fanout chunk missing or corrupted")
		return false
	case lookup == nil:
		r.rep.Errf(key, "fatal: multi-pack-index required OID lookup chunk missing or corrupted")
		return false
	case offsets == nil:
		r.rep.Errf(key, "fatal: multi-pack-index required object offsets chunk missing or corrupted")
		return false
	}
	names := bytes.Split(bytes.TrimRight(packNames, "\x00"), []byte{0})
	if uint32(len(names)) != numPacks {
		r.rep.Errf(key, "fatal: multi-pack-index pack-name chunk is too short")
		return false
	}
	for i := 1; i < len(names); i++ {
		if bytes.Compare(names[i-1], names[i]) >= 0 {
			r.rep.Errf(key, "fatal: multi-pack-index pack names out of order: '%s' before '%s'",
				names[i-1], names[i])
			return false
		}
	}

	h := algo.New()
	h.Write(data[:len(data)-rawsz])
	if !bytes.Equal(h.Sum(nil), data[len(data)-rawsz:]) {
		report("incorrect checksum")
	}

	packs := make([]*odb.Pack, len(names))
	for i, name := range names {
		idxPath := filepath.Join(objectDir, "pack", string(bytes.TrimSuffix(name, []byte(".idx")))+".idx")
		p, err := odb.OpenPack(idxPath, idxPath, algo, false)
		if err != nil {
			report("failed to load pack in position %d", i)
			continue
		}
		defer p.Close()
		packs[i] = p
	}

	numObjects := binary.BigEndian.Uint32(fanout[255*4:])
	for i := 0; i < 255; i++ {
		a := binary.BigEndian.Uint32(fanout[i*4:])
		b := binary.BigEndian.Uint32(fanout[(i+1)*4:])
		if a > b {
			report("oid fanout out of order: fanout[%d] = %x > %x = fanout[%d]", i, a, b, i+1)
		}
	}
	if numObjects == 0 {
		report("the midx contains no oid")
		return ok
	}
	if uint64(numObjects)*uint64(rawsz) > uint64(len(lookup)) ||
		uint64(numObjects)*8 > uint64(len(offsets)) {
		r.rep.Errf(key, "error: multi-pack-index file %s is too small", path)
		return false
	}
	var prev gitobj.OID
	for i := uint32(0); i < numObjects; i++ {
		cur := algo.FromRaw(lookup[int(i)*rawsz:])
		if i > 0 && prev.Compare(cur) >= 0 {
			report("oid lookup out of order: oid[%d] = %s >= %s = oid[%d]", i-1, prev, cur, i)
		}
		prev = cur
	}
	for i := uint32(0); i < numObjects; i++ {
		cur := algo.FromRaw(lookup[int(i)*rawsz:])
		packInt := binary.BigEndian.Uint32(offsets[int(i)*8:])
		offset := uint64(binary.BigEndian.Uint32(offsets[int(i)*8+4:]))
		if offset&0x80000000 != 0 {
			pos := int(offset&0x7fffffff) * 8
			if largeOffsets == nil || pos+8 > len(largeOffsets) {
				report("multi-pack-index large offset out of bounds")
				continue
			}
			offset = binary.BigEndian.Uint64(largeOffsets[pos:])
		}
		if packInt >= numPacks || packs[packInt] == nil {
			report("failed to load pack entry for oid[%d] = %s", i, cur)
			continue
		}
		p := packs[packInt]
		pos, found := p.Find(cur)
		if !found {
			report("failed to load pack entry for oid[%d] = %s", i, cur)
			continue
		}
		if want := uint64(p.OffsetAt(pos)); want != offset {
			report("incorrect object offset for oid[%d] = %s: %x != %x", i, cur, offset, want)
		}
	}
	return ok
}
