// Package gitobj holds the object identifier and object type primitives that
// every other package builds on.
package gitobj

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// MaxRawSize is the raw byte length of the widest hash git supports.
const MaxRawSize = 32

// Algo describes one of git's object hash algorithms.
type Algo struct {
	Name      string
	Format    uint32 // the value git stores in extensions.objectFormat order
	RawSize   int
	HexSize   int
	New       func() hash.Hash
	Empty     OID // hash of the empty string
	EmptyTree OID
}

// SHA1 and SHA256 are the two algorithms git repositories may use.
var (
	SHA1 = &Algo{
		Name:    "sha1",
		Format:  1,
		RawSize: 20,
		HexSize: 40,
		New:     sha1.New,
	}
	SHA256 = &Algo{
		Name:    "sha256",
		Format:  2,
		RawSize: 32,
		HexSize: 64,
		New:     sha256.New,
	}
)

func init() {
	SHA1.Empty = mustParse(SHA1, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")
	SHA1.EmptyTree = mustParse(SHA1, "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	SHA256.Empty = mustParse(SHA256, "473a0f4c3be8a93681a267e3b1e9a7dcda1185436fe141f7749120a303721813")
	SHA256.EmptyTree = mustParse(SHA256, "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321")
}

// AlgoByName returns the algorithm git names in extensions.objectFormat.
func AlgoByName(name string) *Algo {
	switch name {
	case "sha1":
		return SHA1
	case "sha256":
		return SHA256
	}
	return nil
}

// OID is a git object name.
type OID struct {
	H [MaxRawSize]byte
	N uint8
}

// FromBytes builds an OID from raw hash bytes.
func FromBytes(b []byte) OID {
	var o OID
	o.N = uint8(copy(o.H[:], b))
	return o
}

// Null returns the all-zero object name for the algorithm.
func (a *Algo) Null() OID { return OID{N: uint8(a.RawSize)} }

// Raw returns the significant bytes of the object name.
func (o OID) Raw() []byte { return o.H[:o.N] }

// String renders the object name in lower-case hex.
func (o OID) String() string { return hex.EncodeToString(o.H[:o.N]) }

// IsNull reports whether every byte of the object name is zero.
func (o OID) IsNull() bool {
	for _, c := range o.H[:o.N] {
		if c != 0 {
			return false
		}
	}
	return o.N > 0
}

// Valid reports whether the object name carries a hash at all.
func (o OID) Valid() bool { return o.N > 0 }

// Compare orders two object names bytewise, like git's oidcmp().
func (o OID) Compare(b OID) int {
	for i := range o.H {
		if o.H[i] != b.H[i] {
			if o.H[i] < b.H[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Parse decodes a full hex object name of this algorithm's width.
func (a *Algo) Parse(s string) (OID, bool) {
	if len(s) != a.HexSize {
		return OID{}, false
	}
	var o OID
	b, err := hex.DecodeString(s)
	if err != nil {
		return OID{}, false
	}
	o.N = uint8(copy(o.H[:], b))
	return o, true
}

// ParsePrefix decodes an object name at the front of s and returns what follows it.
func (a *Algo) ParsePrefix(s string) (OID, string, bool) {
	o, ok := a.ParseHexBytes([]byte(s))
	if !ok {
		return OID{}, s, false
	}
	return o, s[a.HexSize:], true
}

// ParseHexBytes decodes the first HexSize bytes of buf as an object name.
func (a *Algo) ParseHexBytes(buf []byte) (OID, bool) {
	if len(buf) < a.HexSize {
		return OID{}, false
	}
	var o OID
	for i := 0; i < a.RawSize; i++ {
		hi, ok1 := unhex(buf[2*i])
		lo, ok2 := unhex(buf[2*i+1])
		if !ok1 || !ok2 {
			return OID{}, false
		}
		o.H[i] = hi<<4 | lo
	}
	o.N = uint8(a.RawSize)
	return o, true
}

// FromRaw builds an OID from exactly RawSize bytes.
func (a *Algo) FromRaw(b []byte) OID {
	var o OID
	copy(o.H[:], b[:a.RawSize])
	o.N = uint8(a.RawSize)
	return o
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func mustParse(a *Algo, s string) OID {
	o, ok := a.Parse(s)
	if !ok {
		panic("gitobj: bad constant object name " + s)
	}
	return o
}
