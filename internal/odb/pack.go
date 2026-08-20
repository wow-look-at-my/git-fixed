package odb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/zlibmsg"
)

const (
	idxMagic   = "\xfftOc"
	fanoutSize = 256 * 4
)

// Pack is one packfile and its index, both mapped read-only.
type Pack struct {
	Path    string // the .pack path as git prints it
	IdxFile string // the index path this process opens
	File    string // the pack path this process opens
	IdxPath string // the index path as git prints it
	Algo    *gitobj.Algo
	Num     uint32
	IdxVer  int
	// OpenErr is set when the index or the pack could not be mapped. The
	// pack still appears in the list so the run reports it instead of
	// checking one fewer pack in silence.
	OpenErr error

	idxMap   mapping
	dataMap  mapping
	idx      []byte
	data     []byte
	idxSize  int64
	dataSize int64

	// Offsets into idx of each table, for index version 2.
	oidTable    int
	crcTable    int
	offTable    int
	bigOffTable int

	revOnce sync.Once
	revIdx  []uint32
}

// OpenPack maps a pack. idxFile is the path this process opens; idxPath is the
// path git would print, which differs because git works from the top of the
// worktree and prints ".git/objects/..." where this process holds an absolute
// name. A sequential hint tells the kernel to read ahead, which a full object
// check wants.
func OpenPack(idxFile, idxPath string, algo *gitobj.Algo, sequential bool) (*Pack, error) {
	p := &Pack{
		IdxPath: idxPath,
		IdxFile: idxFile,
		Path:    strings.TrimSuffix(idxPath, ".idx") + ".pack",
		File:    strings.TrimSuffix(idxFile, ".idx") + ".pack",
		Algo:    algo,
	}
	var err error
	if p.idxMap, err = mapReadOnly(idxFile, hintRandom); err != nil {
		return nil, err
	}
	p.idx = p.idxMap.bytes()
	p.idxSize = int64(len(p.idx))
	if err := p.parseIdx(); err != nil {
		p.Close()
		return nil, err
	}
	hint := hintRandom
	if sequential {
		hint = hintSequential
	}
	if p.dataMap, err = mapReadOnly(p.File, hint); err != nil {
		p.Close()
		return nil, err
	}
	p.data = p.dataMap.bytes()
	p.dataSize = int64(len(p.data))
	return p, nil
}

// Close releases both mappings.
func (p *Pack) Close() {
	p.idxMap.close()
	p.dataMap.close()
	p.idx, p.data = nil, nil
}
func (p *Pack) parseIdx() error {
	rawsz := p.Algo.RawSize
	if len(p.idx) < 4+fanoutSize+2*rawsz {
		return fmt.Errorf("index file %s is too small", p.IdxPath)
	}
	base := 0
	p.IdxVer = 1
	if string(p.idx[0:4]) == idxMagic {
		ver := binary.BigEndian.Uint32(p.idx[4:8])
		if ver != 2 {
			return fmt.Errorf("index file %s is version %d, not 2", p.IdxPath, ver)
		}
		p.IdxVer = 2
		base = 8
	}
	fanout := p.idx[base : base+fanoutSize]
	p.Num = binary.BigEndian.Uint32(fanout[255*4:])
	for i := 1; i < 256; i++ {
		if binary.BigEndian.Uint32(fanout[(i-1)*4:]) > binary.BigEndian.Uint32(fanout[i*4:]) {
			return fmt.Errorf("non-monotonic index %s", p.IdxPath)
		}
	}
	n := int64(p.Num)
	if p.IdxVer == 1 {
		p.oidTable = base + fanoutSize
		want := int64(p.oidTable) + n*int64(4+rawsz) + int64(2*rawsz)
		if int64(len(p.idx)) < want {
			return fmt.Errorf("wrong index v1 file size in %s", p.IdxPath)
		}
		return nil
	}
	p.oidTable = base + fanoutSize
	p.crcTable = p.oidTable + int(n)*rawsz
	p.offTable = p.crcTable + int(n)*4
	p.bigOffTable = p.offTable + int(n)*4
	if int64(len(p.idx)) < int64(p.bigOffTable)+int64(2*rawsz) {
		return fmt.Errorf("wrong index v2 file size in %s", p.IdxPath)
	}
	return nil
}

// OIDAt returns the object name stored at index-order position i.
func (p *Pack) OIDAt(i uint32) gitobj.OID {
	rawsz := p.Algo.RawSize
	if p.IdxVer == 1 {
		off := p.oidTable + int(i)*(4+rawsz) + 4
		return p.Algo.FromRaw(p.idx[off:])
	}
	return p.Algo.FromRaw(p.idx[p.oidTable+int(i)*rawsz:])
}

// OffsetAt returns the pack offset of the object at index-order position i.
func (p *Pack) OffsetAt(i uint32) int64 {
	if p.IdxVer == 1 {
		off := p.oidTable + int(i)*(4+p.Algo.RawSize)
		return int64(binary.BigEndian.Uint32(p.idx[off:]))
	}
	v := binary.BigEndian.Uint32(p.idx[p.offTable+int(i)*4:])
	if v&0x80000000 == 0 {
		return int64(v)
	}
	big := int(v & 0x7fffffff)
	off := p.bigOffTable + big*8
	if off+8 > len(p.idx) {
		return -1
	}
	return int64(binary.BigEndian.Uint64(p.idx[off:]))
}

// CRCAt returns the index's recorded CRC32 for position i. Index version 1
// stores no CRCs, and the caller must not ask for one.
func (p *Pack) CRCAt(i uint32) uint32 {
	return binary.BigEndian.Uint32(p.idx[p.crcTable+int(i)*4:])
}

// Find locates an object name in the index, in index order.
func (p *Pack) Find(oid gitobj.OID) (uint32, bool) {
	if p.Num == 0 {
		return 0, false
	}
	base := 0
	if p.IdxVer == 2 {
		base = 8
	}
	fanout := p.idx[base:]
	lo := uint32(0)
	if oid.H[0] > 0 {
		lo = binary.BigEndian.Uint32(fanout[(int(oid.H[0])-1)*4:])
	}
	hi := binary.BigEndian.Uint32(fanout[int(oid.H[0])*4:])
	for lo < hi {
		mid := lo + (hi-lo)/2
		switch cmp := p.OIDAt(mid).Compare(oid); {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid
		default:
			return mid, true
		}
	}
	return 0, false
}

// TrailerOffset is the offset of the pack's own checksum, which is also the end
// of the last object.
func (p *Pack) TrailerOffset() int64 { return p.dataSize - int64(p.Algo.RawSize) }

// Data exposes the mapped packfile.
func (p *Pack) Data() []byte { return p.data }

// Idx exposes the mapped index file.
func (p *Pack) Idx() []byte { return p.idx }

// ObjHeader describes one entry as the packfile itself stores it.
type ObjHeader struct {
	Type    gitobj.Type
	Size    int64
	DataOff int64      // offset of the zlib stream
	BaseOff int64      // absolute offset of the base, for an ofs-delta
	BaseOID gitobj.OID // base name, for a ref-delta
}

// ReadHeader decodes the entry header at a pack offset without inflating.
func (p *Pack) ReadHeader(off int64) (ObjHeader, error) {
	var h ObjHeader
	if off < 12 || off >= p.TrailerOffset() {
		return h, fmt.Errorf("offset %d is outside %s", off, p.Path)
	}
	pos := off
	c, err := p.byteAt(pos)
	if err != nil {
		return h, err
	}
	pos++
	h.Type = gitobj.Type((c >> 4) & 7)
	size := int64(c & 15)
	shift := uint(4)
	for c&0x80 != 0 {
		if c, err = p.byteAt(pos); err != nil {
			return h, err
		}
		pos++
		size |= int64(c&0x7f) << shift
		shift += 7
		if shift > 63 {
			return h, fmt.Errorf("object header at %d in %s is malformed", off, p.Path)
		}
	}
	h.Size = size
	switch h.Type {
	case gitobj.TypeOfsDelta:
		if c, err = p.byteAt(pos); err != nil {
			return h, err
		}
		pos++
		delta := int64(c & 0x7f)
		for c&0x80 != 0 {
			if c, err = p.byteAt(pos); err != nil {
				return h, err
			}
			pos++
			delta = (delta+1)<<7 | int64(c&0x7f)
		}
		if delta <= 0 || delta > off {
			return h, fmt.Errorf("delta base offset out of bound at %d in %s", off, p.Path)
		}
		h.BaseOff = off - delta
	case gitobj.TypeRefDelta:
		if pos+int64(p.Algo.RawSize) > p.TrailerOffset() {
			return h, fmt.Errorf("delta base name is truncated at %d in %s", off, p.Path)
		}
		h.BaseOID = p.Algo.FromRaw(p.data[pos:])
		pos += int64(p.Algo.RawSize)
	case gitobj.TypeCommit, gitobj.TypeTree, gitobj.TypeBlob, gitobj.TypeTag:
	default:
		return h, fmt.Errorf("unknown object type %d at offset %d in %s", h.Type, off, p.Path)
	}
	h.DataOff = pos
	return h, nil
}

func (p *Pack) byteAt(off int64) (byte, error) {
	if off < 0 || off >= int64(len(p.data)) {
		return 0, fmt.Errorf("read past the end of %s", p.Path)
	}
	return p.data[off], nil
}

// Inflater holds the scratch a goroutine needs to decompress pack entries. Each
// goroutine keeps its own.
type Inflater struct {
	zr  io.ReadCloser
	br  bytes.Reader
	pad [1]byte
}

// Inflate decompresses exactly size bytes of the stream that starts at dataOff.
func (in *Inflater) Inflate(p *Pack, dataOff, size int64) ([]byte, error) {
	if dataOff < 0 || dataOff > int64(len(p.data)) {
		return nil, fmt.Errorf("read past the end of %s", p.Path)
	}
	if !plausibleSize(size, int64(len(p.data))-dataOff) {
		// size is the entry header's word for it, and this pack does not
		// hold enough bytes to inflate to that however it is read.
		// see inflatebound.go
		return nil, fmt.Errorf("object at %d in %s claims a size no stream there could hold", dataOff, p.Path)
	}
	in.br.Reset(p.data[dataOff:])
	if in.zr == nil {
		zr, err := zlib.NewReader(&in.br)
		if err != nil {
			return nil, err
		}
		in.zr = zr
	} else if err := in.zr.(zlib.Resetter).Reset(&in.br, nil); err != nil {
		return nil, err
	}
	out := make([]byte, size)
	if _, err := io.ReadFull(in.zr, out); err != nil {
		return nil, err
	}
	// One more read reaches the end of the zlib stream, which is what checks
	// the adler32 trailer. Anything else here means the entry is corrupt.
	if n, err := in.zr.Read(in.pad[:]); n != 0 || err != io.EOF {
		if err == nil || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("inflated size mismatch in %s", p.Path)
		}
		return nil, err
	}
	return out, nil
}

// InflateStream returns a reader over the entry's payload, for an object too
// large to hold in memory.
// InflateMessage is the complaint git's decompressor prints when an entry will
// not decode, before its caller adds one of its own. git gives the read one
// byte more than the index promised, so that a payload longer than that is
// noticed rather than cut short.
func (p *Pack) InflateMessage(dataOff, size int64) string {
	if dataOff < 0 || dataOff > int64(len(p.data)) {
		return ""
	}
	return zlibmsg.Diagnose(p.data[dataOff:], size+1)
}

func (in *Inflater) InflateStream(p *Pack, dataOff int64) (io.Reader, error) {
	if dataOff < 0 || dataOff > int64(len(p.data)) {
		return nil, fmt.Errorf("read past the end of %s", p.Path)
	}
	return zlib.NewReader(bytes.NewReader(p.data[dataOff:]))
}
