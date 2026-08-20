package gitrepo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/go-containers/set"
)

// Index format versions git accepts.
const (
	indexFormatLB = 2
	indexFormatUB = 4
)

// IndexEntry is one path recorded in the index.
type IndexEntry struct {
	Mode  uint32
	OID   gitobj.OID
	Name  string
	Stage int
	// Stat is the 40 bytes git records before the object name: the two
	// timestamps, the device, the inode, the mode, the uid, the gid and the
	// size. A rewritten index keeps them, so git does not have to read every
	// file in the worktree again to find out that nothing changed.
	Stat [40]byte
}

// CacheTree is one node of the cached tree object the index carries.
type CacheTree struct {
	Name       string
	EntryCount int
	OID        gitobj.OID
	Children   []*CacheTree
}

// ResolveUndo remembers the stages of a path whose conflict was resolved.
type ResolveUndo struct {
	Path string
	Mode [3]uint32
	OID  [3]gitobj.OID
}

// Index is a parsed index file.
type Index struct {
	Version     uint32
	Entries     []IndexEntry
	CacheTree   *CacheTree
	ResolveUndo []ResolveUndo
	// Ignored names the extensions that were skipped. git prints one line
	// per extension it does not know, and carries on.
	Ignored []string
}

// knownExtensions are the index extensions git reads. It skips one it does not
// know rather than refusing the index, so this has to know the same names to
// stay quiet about the same ones. read-cache.c lines 69 to 76.
//
// Only TREE and REUC are parsed here. The rest hold no object name, so an fsck
// has nothing to look at in them, and consuming them is the whole job.
var knownExtensions = set.Of("TREE", "REUC", "link", "UNTR", "FSMN", "EOIE", "IEOT", "sdir")

// FatalError is a condition git reports with "fatal:" and exit status 128.
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// ReadIndex parses an index file. It makes the checks fsck turns on: the
// trailing checksum and the ordering of the entries.
func (r *Repo) ReadIndex(path string) (*Index, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Index{}, nil, nil
		}
		return nil, nil, &FatalError{Msg: fmt.Sprintf("%s: index file open failed", r.Shown(path))}
	}
	rawsz := r.Algo.RawSize
	if len(data) < 12+rawsz {
		return nil, nil, &FatalError{Msg: fmt.Sprintf("%s: index file smaller than expected", r.Shown(path))}
	}
	var errs []string
	if string(data[0:4]) != "DIRC" {
		errs = append(errs, fmt.Sprintf("bad signature 0x%08x", binary.BigEndian.Uint32(data[0:4])))
		return nil, errs, &FatalError{Msg: "index file corrupt"}
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < indexFormatLB || version > indexFormatUB {
		errs = append(errs, fmt.Sprintf("bad index version %d", version))
		return nil, errs, &FatalError{Msg: "index file corrupt"}
	}
	trailer := data[len(data)-rawsz:]
	if !isZero(trailer) {
		h := r.Algo.New()
		h.Write(data[:len(data)-rawsz])
		if !bytes.Equal(h.Sum(nil), trailer) {
			errs = append(errs, "bad index file sha1 signature")
			return nil, errs, &FatalError{Msg: "index file corrupt"}
		}
	}

	idx := &Index{Version: version}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	prevName := ""
	for i := 0; i < count; i++ {
		e, next, err := r.readIndexEntry(data, pos, version, prevName)
		if err != nil {
			return nil, errs, err
		}
		idx.Entries = append(idx.Entries, e)
		prevName = e.Name
		pos = next
	}
	if err := checkEntryOrder(idx.Entries); err != nil {
		return nil, errs, err
	}
	for pos+8 <= len(data)-rawsz {
		sig := string(data[pos : pos+4])
		size := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > len(data)-rawsz {
			return nil, errs, &FatalError{Msg: "index file corrupt"}
		}
		body := data[pos : pos+size]
		pos += size
		switch {
		case sig == "TREE":
			idx.CacheTree = parseCacheTree(body, r.Algo)
		case sig == "REUC":
			idx.ResolveUndo = parseResolveUndo(body, r.Algo)
		case knownExtensions.Contains(sig):
			// git reads it and this does not need to. Consuming it is
			// enough, and it says nothing about it either.
		case sig[0] >= 'A' && sig[0] <= 'Z':
			// git's rule: a name that starts with a capital letter is
			// an OPTIONAL extension, so an unknown one is skipped with
			// a note. Reading this backwards refused every index with
			// an untracked cache in it.
			idx.Ignored = append(idx.Ignored, sig)
		default:
			return nil, errs, &FatalError{
				Msg: fmt.Sprintf("index uses %s extension, which we do not understand", sig),
			}
		}
	}
	return idx, errs, nil
}

func (r *Repo) readIndexEntry(data []byte, pos int, version uint32, prevName string) (IndexEntry, int, error) {
	rawsz := r.Algo.RawSize
	fixed := 40 + rawsz + 2
	if pos+fixed > len(data) {
		return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
	}
	var e IndexEntry
	copy(e.Stat[:], data[pos:pos+40])
	e.Mode = binary.BigEndian.Uint32(data[pos+24 : pos+28])
	e.OID = r.Algo.FromRaw(data[pos+40:])
	flags := binary.BigEndian.Uint16(data[pos+40+rawsz:])
	e.Stage = int(flags>>12) & 3
	nameLen := int(flags & 0x0fff)
	off := pos + fixed
	if flags&0x4000 != 0 {
		if version < 3 {
			return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
		}
		off += 2
	}
	if version < 4 {
		var name []byte
		if nameLen < 0x0fff {
			if off+nameLen > len(data) {
				return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
			}
			name = data[off : off+nameLen]
		} else {
			end := bytes.IndexByte(data[off:], 0)
			if end < 0 {
				return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
			}
			name = data[off : off+end]
		}
		e.Name = string(name)
		// Entries are padded so the next one starts on an 8-byte
		// boundary, counting from the start of this entry.
		entryLen := fixed + len(name)
		if flags&0x4000 != 0 {
			entryLen += 2
		}
		return e, pos + ((entryLen + 8) & ^7), nil
	}
	// Version 4 strips a suffix from the previous name and appends its own.
	strip, n := readVarint(data[off:])
	if n == 0 || int(strip) > len(prevName) {
		return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
	}
	off += n
	end := bytes.IndexByte(data[off:], 0)
	if end < 0 {
		return IndexEntry{}, 0, &FatalError{Msg: "index file corrupt"}
	}
	e.Name = prevName[:len(prevName)-int(strip)] + string(data[off:off+end])
	return e, off + end + 1, nil
}

// readVarint decodes git's offset encoding, the same one an ofs-delta uses.
func readVarint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	v := uint64(b[0] & 0x7f)
	i := 0
	for b[i]&0x80 != 0 {
		i++
		if i >= len(b) {
			return 0, 0
		}
		v = (v+1)<<7 | uint64(b[i]&0x7f)
	}
	return v, i + 1
}

// checkEntryOrder is git's check_ce_order(), which fsck always turns on.
func checkEntryOrder(entries []IndexEntry) error {
	for i := 1; i < len(entries); i++ {
		prev, next := entries[i-1], entries[i]
		switch {
		case prev.Name > next.Name:
			return &FatalError{Msg: "unordered stage entries in index"}
		case prev.Name == next.Name:
			if prev.Stage == 0 {
				return &FatalError{Msg: fmt.Sprintf("multiple stage entries for merged file '%s'", prev.Name)}
			}
			if prev.Stage > next.Stage {
				return &FatalError{Msg: fmt.Sprintf("unordered stage entries for '%s'", prev.Name)}
			}
		}
	}
	return nil
}

// parseCacheTree reads the TREE extension, which caches the tree object each
// directory in the index would write out to.
func parseCacheTree(body []byte, algo *gitobj.Algo) *CacheTree {
	pos := 0
	var read func() *CacheTree
	read = func() *CacheTree {
		nul := bytes.IndexByte(body[pos:], 0)
		if nul < 0 {
			return nil
		}
		node := &CacheTree{Name: string(body[pos : pos+nul])}
		pos += nul + 1
		nl := bytes.IndexByte(body[pos:], '\n')
		if nl < 0 {
			return nil
		}
		fields := bytes.Fields(body[pos : pos+nl])
		pos += nl + 1
		if len(fields) < 2 {
			return nil
		}
		entryCount, err1 := strconv.Atoi(string(fields[0]))
		subtrees, err2 := strconv.Atoi(string(fields[1]))
		if err1 != nil || err2 != nil {
			return nil
		}
		node.EntryCount = entryCount
		if entryCount >= 0 {
			if pos+algo.RawSize > len(body) {
				return nil
			}
			node.OID = algo.FromRaw(body[pos:])
			pos += algo.RawSize
		}
		for i := 0; i < subtrees; i++ {
			child := read()
			if child == nil {
				return node
			}
			node.Children = append(node.Children, child)
		}
		return node
	}
	return read()
}

// parseResolveUndo reads the REUC extension, which remembers the stages of a
// path whose conflict was resolved.
func parseResolveUndo(body []byte, algo *gitobj.Algo) []ResolveUndo {
	var out []ResolveUndo
	pos := 0
	for pos < len(body) {
		nul := bytes.IndexByte(body[pos:], 0)
		if nul < 0 {
			return out
		}
		ru := ResolveUndo{Path: string(body[pos : pos+nul])}
		pos += nul + 1
		for i := 0; i < 3; i++ {
			end := bytes.IndexByte(body[pos:], 0)
			if end < 0 {
				return out
			}
			mode, err := strconv.ParseUint(string(body[pos:pos+end]), 8, 32)
			if err != nil {
				mode = 0
			}
			ru.Mode[i] = uint32(mode)
			pos += end + 1
		}
		for i := 0; i < 3; i++ {
			if ru.Mode[i] == 0 {
				continue
			}
			if pos+algo.RawSize > len(body) {
				return out
			}
			ru.OID[i] = algo.FromRaw(body[pos:])
			pos += algo.RawSize
		}
		out = append(out, ru)
	}
	return out
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
