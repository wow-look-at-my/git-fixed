// Package zlibmsg says what zlib would have complained about, for a stream Go's
// decompressor has already refused.
//
// git prints zlib's own message before its caller adds one, so a corrupt object
// produces a line like "inflate: data stream error (invalid block type)". Go's
// compress/flate collapses every one of those cases into one corrupt-input
// error, so the reason has to be worked out separately.
//
// Diagnose therefore runs only after a read has failed. It is a plain,
// unoptimised inflate whose only product is the first fault, and it never runs
// on a stream that decodes.
//
// see docs/zlib-messages.md
package zlibmsg

import (
	"encoding/binary"
	"fmt"
)

// windowSize is DEFLATE's largest distance, which is how far back a match may
// reach and therefore how much output has to be kept.
const windowSize = 32768

// maxBits is the longest Huffman code DEFLATE allows.
const maxBits = 15

// Whole asks Diagnose about the whole stream, for a caller that decompressed
// until the stream ended.
const Whole = -1

// Diagnose returns the line git prints for raw, such as
// "inflate: data stream error (invalid block type)". It returns "" when zlib
// finds nothing to complain about, which includes a stream that simply stops
// early: zlib asks for more input there rather than reporting an error.
//
// maxOut is how many bytes the caller had room for, or Whole. zlib stops as
// soon as it has filled the room it was given, so a fault further down the
// stream is one it never reaches: git reads a loose object's header into 32
// bytes and reports nothing about what follows until it reads the rest.
func Diagnose(raw []byte, maxOut int64) string {
	f := (&inflater{in: bits{data: raw}, sum: newAdler(), limit: maxOut}).run()
	switch f.kind {
	case faultNone:
		return ""
	case faultNeedDict:
		// zlib leaves no message of its own for a stream that wants a
		// dictionary, and git prints its placeholder instead.
		return "inflate: needs dictionary (no message)"
	default:
		return fmt.Sprintf("inflate: data stream error (%s)", f.msg)
	}
}

type faultKind int

const (
	faultNone faultKind = iota
	faultData
	faultNeedDict
)

// fault is the first thing zlib would object to.
type fault struct {
	kind faultKind
	msg  string
}

func dataFault(msg string) fault { return fault{kind: faultData, msg: msg} }

// bits reads a stream the way DEFLATE writes it: least significant bit first,
// within bytes taken in order.
type bits struct {
	data []byte
	pos  int
	hold uint64
	n    uint
	// short is set once a read has asked for more input than there is. The
	// caller stops and reports nothing, because zlib would ask for more
	// input rather than refuse the stream.
	short bool
}

// need fills the holding register with at least n bits.
func (b *bits) need(n uint) bool {
	for b.n < n {
		if b.pos >= len(b.data) {
			b.short = true
			return false
		}
		b.hold |= uint64(b.data[b.pos]) << b.n
		b.pos++
		b.n += 8
	}
	return true
}

// take reads n bits and consumes them.
func (b *bits) take(n uint) (uint32, bool) {
	if !b.need(n) {
		return 0, false
	}
	v := uint32(b.hold & (1<<n - 1))
	b.hold >>= n
	b.n -= n
	return v, true
}

// align drops the rest of the current byte, which a stored block starts on.
func (b *bits) align() {
	drop := b.n & 7
	b.hold >>= drop
	b.n -= drop
}

// byteAt returns the next whole byte, after align has been called.
func (b *bits) byteAt() (byte, bool) {
	v, ok := b.take(8)
	return byte(v), ok
}

// inflater decodes a zlib stream and keeps only what a fault needs: the window
// a match copies from, the number of bytes produced, and the running checksum.
type inflater struct {
	in     bits
	window [windowSize]byte
	// next is where the byte after the last one goes. A match reaches back
	// from it, and the window wraps around.
	next  int
	total int64
	sum   adler
	// limit is how much output the caller had room for, and full marks the
	// moment that room ran out. Everything past it belongs to a later read.
	limit int64
	full  bool
}

// room reports whether one more byte of output fits in what the caller had.
func (i *inflater) room() bool { return i.limit < 0 || i.total < i.limit }

// adler is the running Adler-32 of the output, which zlib writes after the last
// block. One byte at a time is slow, and this runs only on a stream that has
// already failed.
type adler struct{ a, b uint32 }

func newAdler() adler { return adler{a: 1} }

func (s *adler) add(c byte) {
	s.a = (s.a + uint32(c)) % 65521
	s.b = (s.b + s.a) % 65521
}

func (s adler) sum() uint32 { return s.b<<16 | s.a }

func (i *inflater) emit(c byte) {
	i.window[i.next] = c
	i.next = (i.next + 1) % windowSize
	i.total++
	i.sum.add(c)
}

// back returns the byte dist positions before the end of the output.
func (i *inflater) back(dist int) byte {
	pos := (i.next - dist + windowSize) % windowSize
	return i.window[pos]
}

// run walks the whole stream and returns the first thing zlib would refuse.
func (i *inflater) run() fault {
	if f, done := i.header(); done {
		return f
	}
	for {
		last, ok := i.in.take(1)
		if !ok {
			return fault{}
		}
		kind, ok := i.in.take(2)
		if !ok {
			return fault{}
		}
		var f fault
		switch kind {
		case 0:
			f = i.stored()
		case 1:
			f = i.block(fixedLengths())
		case 2:
			f = i.dynamic()
		default:
			return dataFault("invalid block type")
		}
		if f.kind != faultNone || i.in.short || i.full {
			return f
		}
		if last == 1 {
			return i.checksum()
		}
	}
}

// header reads the two bytes zlib puts in front of the DEFLATE data. The tests
// zlib runs over them are in this order, and the order decides which message a
// broken first byte produces.
func (i *inflater) header() (fault, bool) {
	if !i.in.need(16) {
		return fault{}, true
	}
	cmf := uint32(i.in.hold & 0xff)
	flg := uint32((i.in.hold >> 8) & 0xff)
	if (cmf<<8+flg)%31 != 0 {
		return dataFault("incorrect header check"), true
	}
	if cmf&0x0f != 8 {
		return dataFault("unknown compression method"), true
	}
	// zlib is asked for a 32 KiB window, so a stream that wants a larger
	// one is refused.
	if cmf>>4+8 > 15 {
		return dataFault("invalid window size"), true
	}
	i.in.hold >>= 16
	i.in.n -= 16
	if flg&0x20 != 0 {
		return fault{kind: faultNeedDict}, true
	}
	return fault{}, false
}

// stored copies a block that was not compressed.
func (i *inflater) stored() fault {
	i.in.align()
	length, ok := i.in.take(16)
	if !ok {
		return fault{}
	}
	inverse, ok := i.in.take(16)
	if !ok {
		return fault{}
	}
	if length != inverse^0xffff {
		return dataFault("invalid stored block lengths")
	}
	for n := uint32(0); n < length; n++ {
		if !i.room() {
			i.full = true
			return fault{}
		}
		c, ok := i.in.byteAt()
		if !ok {
			return fault{}
		}
		i.emit(c)
	}
	return fault{}
}

// dynamic reads a block's own two code tables, then decodes it.
func (i *inflater) dynamic() fault {
	nlen, ok := i.in.take(5)
	if !ok {
		return fault{}
	}
	ndist, ok := i.in.take(5)
	if !ok {
		return fault{}
	}
	ncode, ok := i.in.take(4)
	if !ok {
		return fault{}
	}
	nlen, ndist, ncode = nlen+257, ndist+1, ncode+4
	if nlen > 286 || ndist > 30 {
		return dataFault("too many length or distance symbols")
	}
	var codeLens [19]int
	for n := uint32(0); n < ncode; n++ {
		v, ok := i.in.take(3)
		if !ok {
			return fault{}
		}
		codeLens[codeLenOrder[n]] = int(v)
	}
	codes, ok := build(codeLens[:], true)
	if !ok {
		return dataFault("invalid code lengths set")
	}
	lens := make([]int, nlen+ndist)
	if !codes.empty {
		if f := i.readLengths(codes, lens); f.kind != faultNone || i.in.short {
			return f
		}
	}
	// A block with no code for the end of the block would never stop.
	if lens[256] == 0 {
		return dataFault("invalid code -- missing end-of-block")
	}
	lengths, ok := build(lens[:nlen], false)
	if !ok {
		return dataFault("invalid literal/lengths set")
	}
	distances, ok := build(lens[nlen:], false)
	if !ok {
		return dataFault("invalid distances set")
	}
	return i.block(tables{lengths: lengths, distances: distances})
}

// readLengths decodes the code lengths of one block's two alphabets, which are
// themselves Huffman coded.
func (i *inflater) readLengths(codes *code, lens []int) fault {
	for at := 0; at < len(lens); {
		sym, status := codes.decode(&i.in)
		switch {
		case status == decodeShort:
			return fault{}
		case status == decodeInvalid:
			return dataFault("invalid code lengths set")
		}
		if sym < 16 {
			lens[at] = sym
			at++
			continue
		}
		var repeat int
		value := 0
		switch sym {
		case 16:
			if at == 0 {
				return dataFault("invalid bit length repeat")
			}
			value = lens[at-1]
			n, ok := i.in.take(2)
			if !ok {
				return fault{}
			}
			repeat = 3 + int(n)
		case 17:
			n, ok := i.in.take(3)
			if !ok {
				return fault{}
			}
			repeat = 3 + int(n)
		default:
			n, ok := i.in.take(7)
			if !ok {
				return fault{}
			}
			repeat = 11 + int(n)
		}
		if at+repeat > len(lens) {
			return dataFault("invalid bit length repeat")
		}
		for ; repeat > 0; repeat-- {
			lens[at] = value
			at++
		}
	}
	return fault{}
}

// tables are the two code tables one block decodes with.
type tables struct {
	lengths   *code
	distances *code
}

// block decodes the body of a block, which is the same work whichever table it
// uses.
func (i *inflater) block(t tables) fault {
	for {
		sym, status := t.lengths.decode(&i.in)
		switch {
		case status == decodeShort:
			return fault{}
		case status == decodeInvalid:
			return dataFault("invalid literal/length code")
		}
		switch {
		case sym < 256:
			if !i.room() {
				i.full = true
				return fault{}
			}
			i.emit(byte(sym))
			continue
		case sym == 256:
			return fault{}
		case sym > 285:
			return dataFault("invalid literal/length code")
		}
		extra, ok := i.in.take(lengthExtra[sym-257])
		if !ok {
			return fault{}
		}
		length := int(lengthBase[sym-257]) + int(extra)

		dsym, status := t.distances.decode(&i.in)
		switch {
		case status == decodeShort:
			return fault{}
		case status == decodeInvalid || dsym > 29:
			return dataFault("invalid distance code")
		}
		dextra, ok := i.in.take(distExtra[dsym])
		if !ok {
			return fault{}
		}
		dist := int(distBase[dsym]) + int(dextra)
		// Nothing was ever written that far back, so the match names
		// bytes that do not exist.
		if int64(dist) > i.total {
			return dataFault("invalid distance too far back")
		}
		for ; length > 0; length-- {
			if !i.room() {
				i.full = true
				return fault{}
			}
			i.emit(i.back(dist))
		}
	}
}

// checksum reads the Adler-32 zlib puts after the last block.
func (i *inflater) checksum() fault {
	i.in.align()
	var want [4]byte
	for n := range want {
		c, ok := i.in.byteAt()
		if !ok {
			return fault{}
		}
		want[n] = c
	}
	if binary.BigEndian.Uint32(want[:]) != i.sum.sum() {
		return dataFault("incorrect data check")
	}
	return fault{}
}
