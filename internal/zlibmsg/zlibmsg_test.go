package zlibmsg

import (
	"bytes"
	"compress/zlib"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writer builds a stream a bit at a time, which is the only way to reach some
// of zlib's complaints: a compressor never produces them.
type writer struct {
	out  []byte
	hold uint32
	n    uint
}

// bits writes n bits of v, with the least significant bit leading. DEFLATE
// writes its own fields this way.
func (w *writer) bits(v uint32, n uint) {
	w.hold |= v << w.n
	w.n += n
	for w.n >= 8 {
		w.out = append(w.out, byte(w.hold))
		w.hold >>= 8
		w.n -= 8
	}
}

// code writes an n-bit Huffman code, with the most significant bit leading.
// DEFLATE packs a code the other way round from its fields.
func (w *writer) code(v uint32, n uint) {
	for shift := int(n) - 1; shift >= 0; shift-- {
		w.bits(v>>uint(shift)&1, 1)
	}
}

// align pads to the next byte, which a stored block starts on.
func (w *writer) align() {
	if w.n > 0 {
		w.bits(0, 8-w.n)
	}
}

// bytes returns the stream, with a zlib header in front of it.
func (w *writer) bytes() []byte {
	w.align()
	return append([]byte{0x78, 0x01}, w.out...)
}

// fixedBlock starts a final block that uses DEFLATE's built-in alphabets.
func fixedBlock() *writer {
	w := &writer{}
	w.bits(1, 1) // last block
	w.bits(1, 2) // fixed alphabets
	return w
}

// literal writes a byte through the fixed length alphabet.
func (w *writer) literal(c byte) {
	if c < 144 {
		w.code(0x30+uint32(c), 8)
		return
	}
	w.code(0x190+uint32(c)-144, 9)
}

// dynamicBlock starts a final block that carries its own alphabets, whose sizes
// are given as written rather than as decoded.
func dynamicBlock(hlit, hdist, hclen uint32) *writer {
	w := &writer{}
	w.bits(1, 1) // last block
	w.bits(2, 2) // the block's own alphabets
	w.bits(hlit, 5)
	w.bits(hdist, 5)
	w.bits(hclen, 4)
	return w
}

// TestMessages pins every complaint zlib makes about a zlib stream, on a stream
// built to produce exactly that complaint. A compressor cannot write most of
// these, so each is assembled bit by bit.
func TestMessages(t *testing.T) {
	// Every length here is the shortest code length, claiming more of the code space than exists.
	overSubscribed := func(w *writer) {
		for range 4 {
			w.bits(1, 3)
		}
	}
	for _, c := range []struct {
		name string
		raw  []byte
		want string
	}{{
		name: "header check",
		// The header bytes together are a checksum, and these do not add up.
		raw:  []byte{0x78, 0x00},
		want: "inflate: data stream error (incorrect header check)",
	}, {
		name: "compression method",
		// The compression-method field names a method that is not DEFLATE, though it still passes the checksum.
		raw:  []byte{0x79, 0x18},
		want: "inflate: data stream error (unknown compression method)",
	}, {
		name: "window size",
		// The encoded window size exceeds windowSize, the largest zlib was asked for.
		raw:  []byte{0x88, 0x1c},
		want: "inflate: data stream error (invalid window size)",
	}, {
		name: "needs dictionary",
		// The header's dictionary bit promises a dictionary nobody has.
		raw:  []byte{0x78, 0x20},
		want: "inflate: needs dictionary (no message)",
	}, {
		name: "block type",
		raw:  []byte{0x78, 0x01, 0x07},
		want: "inflate: data stream error (invalid block type)",
	}, {
		name: "stored block lengths",
		// A stored block writes its length, then its complement, and they disagree here.
		raw:  []byte{0x78, 0x01, 0x01, 0x05, 0x00, 0x00, 0x00},
		want: "inflate: data stream error (invalid stored block lengths)",
	}, {
		name: "too many symbols",
		raw:  dynamicBlock(31, 0, 0).bytes(),
		want: "inflate: data stream error (too many length or distance symbols)",
	}, {
		name: "code lengths set",
		raw: func() []byte {
			w := dynamicBlock(0, 0, 0)
			overSubscribed(w)
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid code lengths set)",
	}, {
		name: "bit length repeat",
		raw: func() []byte {
			// The repeat-previous symbol lands on the lowest code, and repeats a length nothing has set.
			w := dynamicBlock(0, 0, 0)
			w.bits(1, 3) // repeat-previous: give it a short code
			w.bits(2, 3) // short empty-length run: give it a code
			w.bits(2, 3) // long empty-length run: give it a code
			w.bits(0, 3) // give the literal length-value symbol no code
			w.code(0, 1) // the repeat-previous code, written before any length exists to repeat
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid bit length repeat)",
	}, {
		name: "missing end-of-block",
		raw: func() []byte {
			// An alphabet with no codes at all leaves every length unset, so nothing codes the end of the block.
			w := dynamicBlock(0, 0, 0)
			for range 4 {
				w.bits(0, 3)
			}
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid code -- missing end-of-block)",
	}, {
		name: "literal/lengths set",
		raw:  litLenOverSubscribed(),
		want: "inflate: data stream error (invalid literal/lengths set)",
	}, {
		name: "distances set",
		raw:  distOverSubscribed(),
		want: "inflate: data stream error (invalid distances set)",
	}, {
		name: "literal/length code",
		raw: func() []byte {
			// The built-in alphabet still assigns a code past the valid range, which names no length.
			w := fixedBlock()
			w.code(0xc6, 8)
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid literal/length code)",
	}, {
		name: "distance code",
		raw: func() []byte {
			w := fixedBlock()
			w.literal('A')
			w.code(1, 7)  // the length code for the shortest possible match
			w.code(30, 5) // a distance symbol that names no distance
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid distance code)",
	}, {
		name: "distance too far back",
		raw: func() []byte {
			// A byte has been written, and the match reaches past it.
			w := fixedBlock()
			w.literal('A')
			w.code(1, 7) // the length code for the shortest possible match
			w.code(1, 5) // a distance that reaches further back than any byte written
			return w.bytes()
		}(),
		want: "inflate: data stream error (invalid distance too far back)",
	}, {
		name: "data check",
		raw:  brokenChecksum(t),
		want: "inflate: data stream error (incorrect data check)",
	}} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, Diagnose(c.raw, Whole))
		})
	}
}

// lengthAlphabet writes the fixed-width numbers that give the code-length symbols their lengths.
func (w *writer) lengthAlphabet() {
	for _, v := range [18]uint32{0, 2, 2, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2} {
		w.bits(v, 3)
	}
}

// The codes lengthAlphabet defines, in canonical order: the lowest code names the lowest symbol.
func (w *writer) zeroLength()      { w.code(0, 2) }
func (w *writer) oneLength()       { w.code(1, 2) }
func (w *writer) zeroRun(n uint32) { w.code(3, 2); w.bits(n-11, 7) }

// litLenOverSubscribed builds a block whose length alphabet claims more code
// space than exists: more symbols share the shortest codes than there are
// codes to share. The end-of-block symbol carries a length, because a block
// with none is refused before the alphabet is built.
func litLenOverSubscribed() []byte {
	w := dynamicBlock(0, 0, 14) // the smallest lengths and distance alphabets, and a large code-length table
	w.lengthAlphabet()
	w.oneLength() // the lowest length symbol
	w.oneLength() // the next length symbol
	w.zeroRun(138)
	w.zeroRun(116) // the rest of the literal symbols
	w.oneLength()  // the end-of-block symbol
	w.zeroLength() // the distance alphabet's only length
	return w.bytes()
}

// distOverSubscribed builds a block whose distance alphabet is over-subscribed
// while its length alphabet builds, so zlib reaches the distances.
func distOverSubscribed() []byte {
	w := dynamicBlock(0, 2, 14) // the smallest lengths alphabet, several distances, and a large code-length table
	w.lengthAlphabet()
	w.zeroRun(138)
	w.zeroRun(118) // every literal symbol
	// A short code left without a partner is an incomplete alphabet, which zlib allows.
	w.oneLength() // the end-of-block symbol
	w.oneLength()
	w.oneLength()
	w.oneLength() // distances sharing more short codes than there are to go around
	return w.bytes()
}

// brokenChecksum compresses something properly and then changes the checksum
// zlib writes after the last block.
func brokenChecksum(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write([]byte("the quick brown fox"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xff
	return raw
}

// TestGoodStream requires silence on a stream that decodes, and on every prefix
// of it: zlib asks for more input rather than refusing what it has.
func TestGoodStream(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write(bytes.Repeat([]byte("git fsck, in parallel. "), 200))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	raw := buf.Bytes()
	assert.Empty(t, Diagnose(raw, Whole))
	for cut := range len(raw) {
		assert.Empty(t, Diagnose(raw[:cut], Whole), "truncated to %d bytes", cut)
	}
}

// TestOutputLimit requires a fault past the caller's output buffer to go
// unreported. zlib stops when it has filled the room it was given, so a fault
// further down belongs to whoever reads the rest.
func TestOutputLimit(t *testing.T) {
	raw := brokenChecksum(t)
	assert.Equal(t, "inflate: data stream error (incorrect data check)", Diagnose(raw, Whole))
	// The decoded text is longer than the room given below, so that call never reaches the checksum.
	assert.Empty(t, Diagnose(raw, 4))
	assert.Equal(t, "inflate: data stream error (incorrect data check)", Diagnose(raw, 19))
}
