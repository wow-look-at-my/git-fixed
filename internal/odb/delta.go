package odb

import "errors"

// errBadDelta marks a delta that does not decode against its base. git reports
// the same condition as "cannot unpack".
var errBadDelta = errors.New("corrupt delta")

// deltaHeader reads one varint from the front of a delta stream.
func deltaVarint(d []byte) (uint64, []byte, bool) {
	var v uint64
	var shift uint
	for i := 0; i < len(d); i++ {
		c := d[i]
		v |= uint64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return v, d[i+1:], true
		}
		if shift > 63 {
			return 0, nil, false
		}
	}
	return 0, nil, false
}

// applyDelta reconstructs an object from its base and a git delta stream.
func applyDelta(base, delta []byte) ([]byte, error) {
	srcSize, delta, ok := deltaVarint(delta)
	if !ok || srcSize != uint64(len(base)) {
		return nil, errBadDelta
	}
	dstSize, delta, ok := deltaVarint(delta)
	if !ok {
		return nil, errBadDelta
	}
	out := make([]byte, 0, dstSize)
	for len(delta) > 0 {
		cmd := delta[0]
		delta = delta[1:]
		switch {
		case cmd&0x80 != 0:
			var off, size uint64
			for i := uint(0); i < 4; i++ {
				if cmd&(1<<i) != 0 {
					if len(delta) == 0 {
						return nil, errBadDelta
					}
					off |= uint64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if cmd&(0x10<<i) != 0 {
					if len(delta) == 0 {
						return nil, errBadDelta
					}
					size |= uint64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size < off || off+size > uint64(len(base)) {
				return nil, errBadDelta
			}
			out = append(out, base[off:off+size]...)
		case cmd != 0:
			if int(cmd) > len(delta) {
				return nil, errBadDelta
			}
			out = append(out, delta[:cmd]...)
			delta = delta[cmd:]
		default:
			// A zero command byte is reserved and git rejects it.
			return nil, errBadDelta
		}
	}
	if uint64(len(out)) != dstSize {
		return nil, errBadDelta
	}
	return out, nil
}
