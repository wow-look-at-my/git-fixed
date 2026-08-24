package zlibmsg

// code is one canonical Huffman alphabet, held the way zlib's own reference decoder holds it: a count of
// the codes of each length, then the symbols in order.
type code struct {
	counts [maxBits + 1]int
	// symbols lists every symbol that has a code, shortest code first.
	symbols []int
	// empty marks an alphabet that codes nothing.
	empty bool
}

// decodeStatus is what one read from an alphabet produced.
type decodeStatus int

const (
	decodeOK decodeStatus = iota
	// decodeInvalid marks a bit pattern the alphabet gives no symbol for.
	decodeInvalid
	// decodeShort marks the end of the input, which is not a fault.
	decodeShort
)

// build makes the alphabet a list of code lengths describes, and reports
// whether zlib would accept it. codeLengths marks the 19-symbol alphabet that
// carries the other two, which zlib holds to a stricter rule.
func build(lengths []int, codeLengths bool) (*code, bool) {
	c := &code{}
	for _, l := range lengths {
		if l < 0 || l > maxBits {
			return nil, false
		}
		c.counts[l]++
	}
	longest := 0
	for l := maxBits; l >= 1; l-- {
		if c.counts[l] > 0 {
			longest = l
			break
		}
	}
	if longest == 0 {
		c.empty = true
		return c, true
	}
	// Kraft's rule: each length halves what is left of the code space.
	left := 1
	for l := 1; l <= maxBits; l++ {
		left <<= 1
		left -= c.counts[l]
		if left < 0 {
			return nil, false
		}
	}
	// A set that leaves code space over is incomplete.
	if left > 0 && (codeLengths || longest != 1) {
		return nil, false
	}
	offsets := make([]int, maxBits+2)
	for l := 1; l <= maxBits; l++ {
		offsets[l+1] = offsets[l] + c.counts[l]
	}
	c.symbols = make([]int, len(lengths)-c.counts[0])
	for sym, l := range lengths {
		if l != 0 {
			c.symbols[offsets[l]] = sym
			offsets[l]++
		}
	}
	return c, true
}

// decode reads one symbol, one bit at a time. A canonical code lets each length
// be tested as it is reached, which is what makes an unused pattern detectable.
func (c *code) decode(b *bits) (int, decodeStatus) {
	if c.empty {
		return 0, decodeInvalid
	}
	value, first, index := 0, 0, 0
	for l := 1; l <= maxBits; l++ {
		bit, ok := b.take(1)
		if !ok {
			return 0, decodeShort
		}
		value |= int(bit)
		count := c.counts[l]
		if value-first < count {
			return c.symbols[index+value-first], decodeOK
		}
		index += count
		first = (first + count) << 1
		value <<= 1
	}
	return 0, decodeInvalid
}

// fixedLengths returns the two alphabets a fixed block uses. They are the same
// in every stream, so DEFLATE writes neither of them down.
func fixedLengths() tables {
	lengths := make([]int, 288)
	for sym := range lengths {
		switch {
		case sym < 144:
			lengths[sym] = 8
		case sym < 256:
			lengths[sym] = 9
		case sym < 280:
			lengths[sym] = 7
		default:
			lengths[sym] = 8
		}
	}
	// The fixed distance alphabet holds 32 codes, not 30: the last two name no distance.
	distances := make([]int, 32)
	for sym := range distances {
		distances[sym] = 5
	}
	l, lok := build(lengths, false)
	d, dok := build(distances, false)
	if !lok || !dok {
		panic("zlibmsg: DEFLATE's own fixed alphabets do not build")
	}
	return tables{lengths: l, distances: d}
}

// codeLenOrder is the order DEFLATE writes the code-length alphabet's own lengths in.
var codeLenOrder = [19]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// lengthBase and lengthExtra turn a length symbol into a match length.
var (
	lengthBase = [29]int{
		3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
		35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258,
	}
	lengthExtra = [29]uint{
		0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
	}
)

// distBase and distExtra turn a distance symbol into how far back a match
// reaches.
var (
	distBase = [30]int{
		1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
		257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145,
		8193, 12289, 16385, 24577,
	}
	distExtra = [30]uint{
		0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
		7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
	}
)
