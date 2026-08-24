// Package gitpath decides whether a tree entry name can reach a repository's
// own control files once it is checked out.
//
// A filesystem that folds, normalizes, or shortens names lets one name reach a
// different file. A tree entry that reaches .git, .gitmodules, .gitattributes,
// .gitignore, or .mailmap is how several path-traversal attacks on git start,
// so fsck refuses the entry for every filesystem it knows about.
//
// see docs/alias-detection.md
package gitpath

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// needle is one of the control names, always given in lower case ASCII.
type needle string

// The control names fsck protects.
const (
	dotGit        needle = "git"
	dotGitmodules needle = "gitmodules"
	dotGitignore  needle = "gitignore"
	dotGitattrs   needle = "gitattributes"
	dotMailmap    needle = "mailmap"
)

// IsDotGit reports whether name reaches ".git" on any filesystem git knows.
func IsDotGit(name []byte) bool { return matches(name, dotGit) }

// IsDotGitmodules reports whether name reaches ".gitmodules".
func IsDotGitmodules(name []byte) bool { return matches(name, dotGitmodules) }

// IsDotGitignore reports whether name reaches ".gitignore".
func IsDotGitignore(name []byte) bool { return matches(name, dotGitignore) }

// IsDotGitattributes reports whether name reaches ".gitattributes".
func IsDotGitattributes(name []byte) bool { return matches(name, dotGitattrs) }

// IsDotMailmap reports whether name reaches ".mailmap".
func IsDotMailmap(name []byte) bool { return matches(name, dotMailmap) }

// IsNTFSDotGit reports only the NTFS spelling of ".git".
func IsNTFSDotGit(name []byte) bool { return couldReach(name) && isNTFSDotGit(name) }

// IsNTFSDotGitmodules reports only the NTFS spelling of ".gitmodules".
func IsNTFSDotGitmodules(name []byte) bool {
	return couldReach(name) && isNTFSDotGeneric(name, dotGitmodules)
}

// couldReach rules a name out on its first two bytes.
func couldReach(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	c := name[0]
	if c == '.' || c == '~' || c >= utf8.RuneSelf {
		return true
	}
	// An 8.3 short name is at least three characters, and its second may be
	// the tilde that introduces the disambiguating number.
	if len(name) < 2 {
		return false
	}
	switch toLowerASCII(c) {
	case 'g':
		c = toLowerASCII(name[1])
		return c == 'i' || c == '~'
	case 'm':
		c = toLowerASCII(name[1])
		return c == 'a' || c == '~'
	}
	return false
}

func matches(name []byte, n needle) bool {
	if !couldReach(name) {
		return false
	}
	if n == dotGit {
		if isHFSDotGeneric(name, n) || isNTFSDotGit(name) {
			return true
		}
	} else if isHFSDotGeneric(name, n) || isNTFSDotGeneric(name, n) {
		return true
	}
	if isASCII(name) {
		// Normalization leaves an ASCII name alone, and ext4's case fold over ASCII is the fold the HFS check above.
		return false
	}
	return isExt4DotGeneric(name, n) || isZFSDotGeneric(name, n)
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ntfsShortnamePrefix is the fall-back 8.3 short name NTFS derives for each
// control name. git hard-codes the same table.
func ntfsShortnamePrefix(n needle) string {
	switch n {
	case dotGitmodules:
		return "gi7eba"
	case dotGitignore:
		return "gi250a"
	case dotGitattrs:
		return "gi7d29"
	case dotMailmap:
		return "maba30"
	}
	return ""
}

// hfsIgnorable lists the code points HFS+ drops from a name entirely, so that
// ".gi<ZWNJ>t" and ".git" name the same file.
func hfsIgnorable(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0x200e, 0x200f,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f,
		0xfeff:
		return true
	}
	return false
}

// nextHFSChar returns the next code point HFS+ would compare, skipping the
// ignorable ones. It reports ok=false on malformed UTF-8, which is enough for
// the caller to conclude the name is not a control name.
func nextHFSChar(s []byte) (r rune, rest []byte, ok bool) {
	for {
		if len(s) == 0 {
			return 0, nil, true
		}
		r, size := utf8.DecodeRune(s)
		if r == utf8.RuneError && size <= 1 {
			return 0, nil, false
		}
		s = s[size:]
		if hfsIgnorable(r) {
			continue
		}
		return r, s, true
	}
}

func isHFSDotGeneric(name []byte, n needle) bool {
	r, rest, ok := nextHFSChar(name)
	if !ok || r != '.' {
		return false
	}
	for i := 0; i < len(n); i++ {
		r, rest, ok = nextHFSChar(rest)
		if !ok || r > 127 || toLowerASCII(byte(r)) != n[i] {
			return false
		}
	}
	r, _, ok = nextHFSChar(rest)
	if !ok {
		return false
	}
	return r == 0 || r == '/' || r == '\\'
}

// isNTFSDotGit is git's is_ntfs_dotgit(): ".git" or the short name "git~1",
// either one followed only by spaces and periods.
func isNTFSDotGit(name []byte) bool {
	i := 0
	next := func() byte {
		if i >= len(name) {
			return 0
		}
		c := name[i]
		i++
		return c
	}
	c := next()
	switch {
	case c == '.':
		if !eqAnyCase(next(), 'g') || !eqAnyCase(next(), 'i') || !eqAnyCase(next(), 't') {
			return false
		}
	case c == 'g' || c == 'G':
		if !eqAnyCase(next(), 'i') || !eqAnyCase(next(), 't') || next() != '~' || next() != '1' {
			return false
		}
	default:
		return false
	}
	for {
		c = next()
		if c == 0 || c == '/' || c == '\\' || c == ':' {
			return true
		}
		if c != '.' && c != ' ' {
			return false
		}
	}
}

// isNTFSDotGeneric is git's is_ntfs_dot_generic(): the plain name, the regular
// 8.3 short name, or the fall-back short name, each followed only by spaces and
// periods.
func isNTFSDotGeneric(name []byte, n needle) bool {
	prefix := ntfsShortnamePrefix(n)
	if len(name) > 0 && name[0] == '.' && hasPrefixFold(name[1:], string(n)) {
		return onlySpacesAndPeriods(name, len(n)+1)
	}
	if len(n) >= 6 && hasPrefixFold(name, string(n)[:6]) && len(name) > 7 && name[6] == '~' &&
		name[7] >= '1' && name[7] <= '4' {
		return onlySpacesAndPeriods(name, 8)
	}
	sawTilde := false
	for i := 0; i < 8; i++ {
		if i >= len(name) || name[i] == 0 {
			return false
		}
		switch {
		case sawTilde:
			if name[i] < '0' || name[i] > '9' {
				return false
			}
		case name[i] == '~':
			i++
			if i >= len(name) || name[i] < '1' || name[i] > '9' {
				return false
			}
			sawTilde = true
		case i >= 6:
			return false
		case name[i]&0x80 != 0:
			return false
		case toLowerASCII(name[i]) != prefix[i]:
			return false
		}
	}
	return onlySpacesAndPeriods(name, 8)
}

func onlySpacesAndPeriods(name []byte, i int) bool {
	for ; i < len(name); i++ {
		c := name[i]
		if c == ':' {
			return true
		}
		if c != ' ' && c != '.' {
			return false
		}
	}
	return true
}

// isExt4DotGeneric reports whether a casefold ext4 directory resolves name to the control name.
//
// ext4 folds past ASCII: the long s U+017F folds to "s", so ".gitmoduleſ" opens .gitmodules there.
func isExt4DotGeneric(name []byte, n needle) bool {
	if !utf8.Valid(name) {
		return false
	}
	return bytes.EqualFold(name, dotted(n))
}

// isZFSDotGeneric reports whether a ZFS dataset that normalizes names would resolve name to the control name.
func isZFSDotGeneric(name []byte, n needle) bool {
	if !utf8.Valid(name) {
		return false
	}
	// A name already in normal form needs no copy, which is every name in
	// an ordinary repository.
	if norm.NFKD.IsNormal(name) {
		return bytes.EqualFold(name, dotted(n))
	}
	return bytes.EqualFold(norm.NFKD.Bytes(name), dotted(n))
}

// dottedNames holds each control name with its leading period, so the
// comparison below allocates nothing.
var dottedNames = map[needle][]byte{
	dotGit:        []byte(".git"),
	dotGitmodules: []byte(".gitmodules"),
	dotGitignore:  []byte(".gitignore"),
	dotGitattrs:   []byte(".gitattributes"),
	dotMailmap:    []byte(".mailmap"),
}

func dotted(n needle) []byte { return dottedNames[n] }

func eqAnyCase(c, lower byte) bool { return toLowerASCII(c) == lower }

func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func hasPrefixFold(s []byte, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if toLowerASCII(s[i]) != prefix[i] {
			return false
		}
	}
	return true
}
