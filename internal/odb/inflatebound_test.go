package odb

// A size in a header is a claim, and this tool is pointed at repositories where
// that claim is as likely to be damage as anything else.

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlausibleSize(t *testing.T) {
	for _, c := range []struct {
		name             string
		size, compressed int64
		want             bool
	}{
		{"an object smaller than its own stream", 100, 100, true},
		{"a stream that compressed well", 1 << 20, 4096, true},
		{"the empty object", 0, 20, true},
		{"a whole pack could not hold it", 1 << 40, 1 << 20, false},
		{"a size that would overflow the multiplication", math.MaxInt64, 1 << 30, false},
		{"a negative size", -1, 1 << 20, false},
		{"a negative length", 100, -1, false},
	} {
		assert.Equal(t, c.want, plausibleSize(c.size, c.compressed), c.name)
	}
}

// TestPlausibleSizeAcceptsWhatDeflateCanDo keeps the bound clear of anything a
// real stream produces. Refusing a valid object would be far worse than the
// allocation this exists to stop, so the ratio has room above the format's own
// worst-case compression ratio and nothing near it is refused.
func TestPlausibleSizeAcceptsWhatDeflateCanDo(t *testing.T) {
	const compressed = 1 << 20
	assert.True(t, plausibleSize(1032*compressed, compressed),
		"deflate's own limit must be well inside the bound")
	assert.True(t, plausibleSize(maxInflateRatio*compressed, compressed),
		"the bound itself is a size, not a size to refuse")
	assert.False(t, plausibleSize(maxInflateRatio*compressed+maxInflateRatio, compressed))
}

func TestMaxDeltaOutput(t *testing.T) {
	// A single-byte delta is a copy command with no offset and no size.
	assert.Equal(t, uint64(0x10000), maxDeltaOutput(1))
	assert.Equal(t, uint64(0), maxDeltaOutput(0))
}
