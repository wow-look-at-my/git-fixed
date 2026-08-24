package gittest

// Packs written by hand, for a test about the shape of a delta tree.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// PackObject is one object to put in a hand-built pack.
//
// Base is an earlier object in the same pack to delta against, or -1 for an object stored whole.
type PackObject struct {
	Type gitobj.Type
	Data []byte
	Base int
	// ByName stores the delta against its base's name rather than its offset, which is git's other delta encoding.
	ByName bool
}

// WritePack writes these objects as a packfile under .git/objects/pack and has
// git build the index for it. It returns the path of the pack and where each
// object starts in it, which is what a test that damages one needs.
//
// git rejects a pack it cannot read, so a pack that comes back with an index is
// a pack that says what the test meant it to say.
func (r *Repo) WritePack(name string, objs []PackObject) (string, []int64) {
	r.t.Helper()
	var buf bytes.Buffer
	buf.WriteString("PACK")
	writeBE32(&buf, 2)
	writeBE32(&buf, uint32(len(objs)))

	offsets := make([]int64, len(objs))
	for i, o := range objs {
		offsets[i] = int64(buf.Len())
		payload := o.Data
		typ := o.Type
		if o.Base >= 0 {
			payload = deltaBetween(objs[o.Base].Data, o.Data)
			typ = gitobj.TypeOfsDelta
			if o.ByName {
				typ = gitobj.TypeRefDelta
			}
		}
		writeObjHeader(&buf, typ, int64(len(payload)))
		switch {
		case o.Base < 0:
		case o.ByName:
			base := objs[o.Base]
			buf.Write(odb.HashLiteral(r.Algo, base.Type.Name(), base.Data).Raw())
		default:
			writeOfsDelta(&buf, offsets[i]-offsets[o.Base])
		}
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			r.t.Fatalf("compressing pack object %d: %v", i, err)
		}
		if err := zw.Close(); err != nil {
			r.t.Fatalf("finishing pack object %d: %v", i, err)
		}
	}
	sum := r.Algo.New()
	sum.Write(buf.Bytes())
	buf.Write(sum.Sum(nil))

	dir := filepath.Join(r.GitDir(), "objects", "pack")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		r.t.Fatalf("making the pack directory: %v", err)
	}
	path := filepath.Join(dir, name+".pack")
	if err := os.WriteFile(path, buf.Bytes(), 0o666); err != nil {
		r.t.Fatalf("writing %s: %v", path, err)
	}
	r.Git("index-pack", path)
	return path, offsets
}

func writeBE32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// writeObjHeader writes the type and size a pack entry starts with: four bits
// of size in the first byte, then seven at a time.
func writeObjHeader(buf *bytes.Buffer, typ gitobj.Type, size int64) {
	c := byte(typ)<<4 | byte(size&0xf)
	size >>= 4
	for size > 0 {
		buf.WriteByte(c | 0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(c)
}

// writeOfsDelta writes how far back the base is, in the encoding git's
// unpack_object_header_buffer reads back.
func writeOfsDelta(buf *bytes.Buffer, back int64) {
	var tmp [16]byte
	i := len(tmp) - 1
	tmp[i] = byte(back & 0x7f)
	for back >>= 7; back > 0; back >>= 7 {
		back--
		i--
		tmp[i] = 0x80 | byte(back&0x7f)
	}
	buf.Write(tmp[i:])
}

// deltaBetween encodes want as a delta on base: copy whatever the two share at
// the front, then insert the rest.
func deltaBetween(base, want []byte) []byte {
	var out []byte
	out = appendDeltaVarint(out, uint64(len(base)))
	out = appendDeltaVarint(out, uint64(len(want)))
	shared := 0
	for shared < len(base) && shared < len(want) && base[shared] == want[shared] {
		shared++
	}
	// A copy op names an offset and a size, each byte of them optional, and
	// the low bits of the command say which bytes follow. This one writes
	// every byte of both rather than work out which it could leave out.
	for done := 0; done < shared; {
		n := min(shared-done, 0xffffff)
		out = append(out, 0x80|0x0f|0x70,
			byte(done), byte(done>>8), byte(done>>16), byte(done>>24),
			byte(n), byte(n>>8), byte(n>>16))
		done += n
	}
	// An insert op is a length under 128 and then that many bytes.
	for rest := want[shared:]; len(rest) > 0; {
		n := min(len(rest), 127)
		out = append(out, byte(n))
		out = append(out, rest[:n]...)
		rest = rest[n:]
	}
	return out
}

// appendDeltaVarint writes one of the two sizes a delta stream starts with.
func appendDeltaVarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}
