package odb

// What a compressed stream could possibly hold.

// maxInflateRatio is the bound, with room either side of the format's own 1032 to 1.
const maxInflateRatio = 2048

// plausibleSize reports whether a stream of the given compressed length could
// inflate to size bytes.
func plausibleSize(size, compressed int64) bool {
	if size < 0 || compressed < 0 {
		return false
	}
	// Overflow would turn an impossible size into a plausible one, so the division goes the other way.
	return size/maxInflateRatio <= compressed
}
