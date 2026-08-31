package gitobj

import "fmt"

// Type mirrors git's enum object_type, including the delta encodings that live only inside a packfile.
type Type int

// The object types, with the same numeric values git uses on the wire.
const (
	TypeBad      Type = -1
	TypeNone     Type = 0
	TypeCommit   Type = 1
	TypeTree     Type = 2
	TypeBlob     Type = 3
	TypeTag      Type = 4
	TypeOfsDelta Type = 6
	TypeRefDelta Type = 7
	// TypeAny is git's OBJ_ANY: the caller accepts whatever type it finds.
	TypeAny Type = -2
)

// Name returns the on-disk spelling of the type, or "" for a type that has no
// spelling. This is git's type_name().
func (t Type) Name() string {
	switch t {
	case TypeNone:
		return "none"
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	case TypeOfsDelta:
		return "ofs-delta"
	case TypeRefDelta:
		return "ref-delta"
	}
	return ""
}

// TypeFromName parses an on-disk type name. It is git's
// type_from_string_gently() with gentle set: an unknown name gives TypeBad.
func TypeFromName(s string) Type {
	switch s {
	case "commit":
		return TypeCommit
	case "tree":
		return TypeTree
	case "blob":
		return TypeBlob
	case "tag":
		return TypeTag
	}
	return TypeBad
}

// Header renders the loose-object header that precedes the payload.
func (t Type) Header(size int64) []byte {
	return []byte(fmt.Sprintf("%s %d\x00", t.Name(), size))
}
