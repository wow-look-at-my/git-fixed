package odb

// What a compressed stream could possibly hold.
//
// Every object git stores says how big it is before it says what it is, and
// this tool is pointed at repositories where that number is as likely to be
// damage as anything else. Reserving a claimed size before reading a byte of
// the payload is how a corrupt header turns into an allocation of whatever
// number happened to land there: a size field of four wrong bytes asks for a
// terabyte, and the run dies without saying which object did it.
//
// A deflate stream cannot expand by more than 1032 to 1. That is a property of
// the format, so a size past it is not a size that stream could have produced,
// whatever else is wrong with the file. Refusing it costs a comparison and
// turns an unexplained death into the line git prints for an entry that will
// not decode.

// maxInflateRatio is the bound, with room either side of the format's own 1032
// to 1. The point is to refuse a size no stream could have made, not to
// second-guess one that could: a valid object must never come near this.
const maxInflateRatio = 2048

// plausibleSize reports whether a stream of the given compressed length could
// inflate to size bytes.
func plausibleSize(size, compressed int64) bool {
	if size < 0 || compressed < 0 {
		return false
	}
	// Overflow would turn an impossible size into a plausible one, so the
	// division goes the other way.
	return size/maxInflateRatio <= compressed
}
