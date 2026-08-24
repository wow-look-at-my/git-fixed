package odb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// varint encodes a size the way a delta header does.
func varint(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			return out
		}
	}
}

// copyOp builds a copy instruction with the smallest encoding that fits.
func copyOp(off, size uint32) []byte {
	cmd := byte(0x80)
	var body []byte
	for i := range uint(4) {
		if b := byte(off >> (8 * i)); b != 0 {
			cmd |= 1 << i
			body = append(body, b)
		}
	}
	for i := range uint(3) {
		if b := byte(size >> (8 * i)); b != 0 {
			cmd |= 0x10 << i
			body = append(body, b)
		}
	}
	return append([]byte{cmd}, body...)
}

// insertOp builds a literal-insert instruction.
func insertOp(data string) []byte {
	return append([]byte{byte(len(data))}, data...)
}

// delta assembles a whole delta stream against a base of the given length.
func delta(baseLen, outLen int, ops ...[]byte) []byte {
	d := append(varint(uint64(baseLen)), varint(uint64(outLen))...)
	for _, op := range ops {
		d = append(d, op...)
	}
	return d
}

func TestDeltaVarint(t *testing.T) {
	for _, want := range []uint64{0, 1, 127, 128, 300, 1 << 20} {
		v, rest, ok := deltaVarint(append(varint(want), 'x'))
		require.True(t, ok)
		assert.Equal(t, want, v)
		assert.Equal(t, "x", string(rest))
	}
	_, _, ok := deltaVarint(nil)
	assert.False(t, ok, "an empty stream has no size")
	_, _, ok = deltaVarint([]byte{0x80, 0x80})
	assert.False(t, ok, "a size that never ends is not a size")
	_, _, ok = deltaVarint([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	assert.False(t, ok, "a size wider than 64 bits is refused")
}

func TestApplyDelta(t *testing.T) {
	base := []byte("the quick brown fox")

	out, err := applyDelta(base, delta(len(base), 9, copyOp(4, 5), insertOp("cat!")))
	require.NoError(t, err)
	assert.Equal(t, "quickcat!", string(out))

	// A copy with a zero size means 0x10000 bytes, which git encodes by leaving every size byte out.
	big := make([]byte, 0x10000)
	for i := range big {
		big[i] = byte(i)
	}
	out, err = applyDelta(big, delta(len(big), 0x10000, []byte{0x80}))
	require.NoError(t, err)
	assert.Equal(t, big, out)

	// A whole-object insert needs no base at all.
	out, err = applyDelta(nil, delta(0, 5, insertOp("hello")))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(out))
}

func TestApplyDeltaErrors(t *testing.T) {
	base := []byte("0123456789")
	for name, d := range map[string][]byte{
		"no source size":     nil,
		"wrong source size":  delta(len(base)+1, 1, insertOp("x")),
		"no result size":     varint(uint64(len(base))),
		"copy past the base": delta(len(base), 4, copyOp(8, 4)),
		"truncated copy":     append(delta(len(base), 4), 0x91),
		"truncated insert":   append(delta(len(base), 4), 0x04, 'a'),
		"zero opcode":        append(delta(len(base), 4), 0x00),
		"short result":       delta(len(base), 9, insertOp("x")),
		"long result":        delta(len(base), 1, insertOp("xyz")),
	} {
		_, err := applyDelta(base, d)
		assert.ErrorIs(t, err, errBadDelta, name)
	}
}
