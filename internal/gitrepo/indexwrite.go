package gitrepo

// Salvaging a damaged index, and writing a whole one back.
//
// see docs/repair.md

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// SalvagedIndex is what could still be read out of a damaged index file.
type SalvagedIndex struct {
	// Entries are the paths that parsed, in the order the file held them.
	Entries []IndexEntry
	// Stopped is what ended the read, empty when the file parsed whole.
	Stopped string
	// Count is how many entries the header claimed, which says how many were
	// left behind when Stopped is set.
	Count int
}

// SalvageIndex reads as much of an index as the file allows.
//
// ReadIndex is the check git makes, and it stops at the first fault because a
// tool that is about to USE an index must not act on half of one. This is the
// other question: a repair is going to displace the file, so everything still
// legible in it is worth having. A truncated index usually loses only its tail.
//
// The checksum is deliberately not consulted. A file with the wrong checksum
// still holds real entries, and refusing to look at them would throw away the
// staged work this is here to keep.
func (r *Repo) SalvageIndex(path string) (*SalvagedIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rawsz := r.Algo.RawSize
	if len(data) < 12 {
		return &SalvagedIndex{Stopped: "the file is too short to hold a header"}, nil
	}
	if string(data[0:4]) != "DIRC" {
		return &SalvagedIndex{Stopped: "the file does not start with DIRC"}, nil
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < indexFormatLB || version > indexFormatUB {
		return &SalvagedIndex{Stopped: fmt.Sprintf("index version %d is not one git writes", version)}, nil
	}
	out := &SalvagedIndex{Count: int(binary.BigEndian.Uint32(data[8:12]))}
	// The entries end before the trailing checksum, when there is room for
	// one. A truncated file has no trailer, so the entries run to the end.
	end := len(data)
	if end > 12+rawsz {
		end -= rawsz
	}
	pos, prevName := 12, ""
	for i := 0; i < out.Count; i++ {
		if pos >= end {
			out.Stopped = fmt.Sprintf("the file ends after %d of %d entries", i, out.Count)
			break
		}
		e, next, err := r.readIndexEntry(data[:end], pos, version, prevName)
		if err != nil {
			out.Stopped = fmt.Sprintf("entry %d of %d will not parse", i+1, out.Count)
			break
		}
		out.Entries = append(out.Entries, e)
		prevName, pos = e.Name, next
	}
	return out, nil
}

// WriteIndex writes entries as an index file, replacing whatever is there.
//
// It writes version 2, which every git since 1.5 reads. A rewritten index is
// not a place to be clever: the newer versions save space, and space is not
// what a repair is short of.
func (r *Repo) WriteIndex(path string, entries []IndexEntry) error {
	sorted := make([]IndexEntry, len(entries))
	copy(sorted, entries)
	// git requires this order and refuses an index without it, which is what
	// check_ce_order() reports.
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].Stage < sorted[j].Stage
	})

	var buf bytes.Buffer
	buf.WriteString("DIRC")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(sorted)))
	rawsz := r.Algo.RawSize
	for _, e := range sorted {
		start := buf.Len()
		// The mode is a field inside the stat block, at the offset git's own
		// reader takes it from. Writing it here rather than trusting the
		// block is what lets an entry rebuilt from a tree carry a mode at
		// all: it has no stat data, so its block is forty zero bytes.
		stat := e.Stat
		binary.BigEndian.PutUint32(stat[24:28], e.Mode)
		buf.Write(stat[:])
		// Always rawsz bytes: a zero OID reports a length of zero, and a
		// short name field would shift every following entry.
		var oid [gitobj.MaxRawSize]byte
		copy(oid[:], e.OID.Raw())
		buf.Write(oid[:rawsz])
		nameLen := len(e.Name)
		if nameLen > 0x0fff {
			// The field only holds twelve bits. git stores the maximum and
			// reads the name up to its terminator instead.
			nameLen = 0x0fff
		}
		binary.Write(&buf, binary.BigEndian, uint16(e.Stage)<<12|uint16(nameLen))
		buf.WriteString(e.Name)
		// At least one terminator, then padding to an eight-byte boundary
		// measured from the start of the entry.
		buf.WriteByte(0)
		for (buf.Len()-start)%8 != 0 {
			buf.WriteByte(0)
		}
	}
	h := r.Algo.New()
	h.Write(buf.Bytes())
	buf.Write(h.Sum(nil))

	// Write beside the index and rename, so no reader ever sees half of one.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "index_tmp_*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
