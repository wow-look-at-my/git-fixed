package fsck

import (
	"bytes"
	"errors"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitpath"
)

// File modes git accepts in a tree.
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000
	modeGitlink = 0o160000
)

// TreeEntry is one line of a tree object, with the mode exactly as stored.
type TreeEntry struct {
	Mode uint32
	Name string
	OID  gitobj.OID
	// Raw is the entry's bytes, whose first byte tells a zero-padded mode.
	Raw []byte
}

// IsDir reports whether the entry names a subtree.
func (e TreeEntry) IsDir() bool { return e.Mode&0o170000 == modeDir }

// IsGitlink reports whether the entry names a submodule commit.
func (e TreeEntry) IsGitlink() bool { return e.Mode&0o170000 == modeGitlink }

// IsSymlink reports whether the entry names a symbolic link.
func (e TreeEntry) IsSymlink() bool { return e.Mode&0o170000 == modeSymlink }

// IsRegular reports whether the entry names a regular file.
func (e TreeEntry) IsRegular() bool { return e.Mode&0o170000 == modeRegular }

// errBadTree is git's decode_tree_entry() failure, which fsck turns into one
// "cannot be parsed as a tree".
var errBadTree = errors.New("cannot be parsed as a tree")

// ParseTree decodes a whole tree object. It stops at the first malformed entry
// and returns what it read up to that point, which is what git reports on.
func ParseTree(buf []byte, algo *gitobj.Algo) ([]TreeEntry, error) {
	var out []TreeEntry
	rest := buf
	for len(rest) > 0 {
		e, next, err := decodeTreeEntry(rest, algo)
		if err != nil {
			return out, err
		}
		out = append(out, e)
		rest = next
	}
	return out, nil
}

// decodeTreeEntry is git's decode_tree_entry(): "<octal mode> <name>\0<hash>".
func decodeTreeEntry(buf []byte, algo *gitobj.Algo) (TreeEntry, []byte, error) {
	rawsz := algo.RawSize
	if len(buf) < rawsz+3 {
		return TreeEntry{}, nil, errBadTree
	}
	sp := bytes.IndexByte(buf, ' ')
	if sp <= 0 {
		return TreeEntry{}, nil, errBadTree
	}
	var mode uint32
	for _, c := range buf[:sp] {
		if c < '0' || c > '7' {
			return TreeEntry{}, nil, errBadTree
		}
		mode = mode<<3 + uint32(c-'0')
	}
	nul := bytes.IndexByte(buf[sp+1:], 0)
	if nul < 0 {
		return TreeEntry{}, nil, errBadTree
	}
	if nul == 0 {
		// git calls this "empty filename in tree entry".
		return TreeEntry{}, nil, errBadTree
	}
	nameEnd := sp + 1 + nul
	if nameEnd+1+rawsz > len(buf) {
		return TreeEntry{}, nil, errBadTree
	}
	e := TreeEntry{
		Mode: mode,
		Name: string(buf[sp+1 : nameEnd]),
		OID:  algo.FromRaw(buf[nameEnd+1:]),
		Raw:  buf[:sp],
	}
	return e, buf[nameEnd+1+rawsz:], nil
}

// Tree runs every check git makes on a tree object.
func (o *Options) Tree(oid gitobj.OID, buf []byte) int {
	retval := 0
	var (
		hasNullSHA1   bool
		hasFullPath   bool
		hasEmptyName  bool
		hasDot        bool
		hasDotdot     bool
		hasDotgit     bool
		hasZeroPad    bool
		hasBadModes   bool
		hasDupEntries bool
		notSorted     bool
		hasLargeName  bool
	)
	entries, err := ParseTree(buf, o.Algo)
	if err != nil && len(entries) == 0 {
		return retval + o.report(oid, gitobj.TypeTree, MsgBadTree, "cannot be parsed as a tree")
	}

	var candidates []string
	var prev *TreeEntry
	for i := range entries {
		e := &entries[i]
		hasNullSHA1 = hasNullSHA1 || e.OID.IsNull()
		hasFullPath = hasFullPath || bytes.ContainsRune([]byte(e.Name), '/')
		hasEmptyName = hasEmptyName || e.Name == ""
		hasDot = hasDot || e.Name == "."
		hasDotdot = hasDotdot || e.Name == ".."
		hasDotgit = hasDotgit || gitpath.IsDotGit(e.Name)
		hasZeroPad = hasZeroPad || (len(e.Raw) > 0 && e.Raw[0] == '0')
		hasLargeName = hasLargeName || len(e.Name) > o.MaxTreeEntryLen

		if gitpath.IsDotGitmodules(e.Name) {
			if !e.IsSymlink() {
				o.foundGitmodules(e.OID)
			} else {
				retval += o.report(oid, gitobj.TypeTree, MsgGitmodulesSymlink,
					".gitmodules is a symbolic link")
			}
		}
		if gitpath.IsDotGitattributes(e.Name) {
			if !e.IsSymlink() {
				o.foundGitattributes(e.OID)
			} else {
				retval += o.report(oid, gitobj.TypeTree, MsgGitattributesSymlink,
					".gitattributes is a symlink")
			}
		}
		if e.IsSymlink() {
			if gitpath.IsDotGitignore(e.Name) {
				retval += o.report(oid, gitobj.TypeTree, MsgGitignoreSymlink,
					".gitignore is a symlink")
			}
			if gitpath.IsDotMailmap(e.Name) {
				retval += o.report(oid, gitobj.TypeTree, MsgMailmapSymlink,
					".mailmap is a symlink")
			}
		}

		// NTFS reads a backslash as a directory separator, so each
		// segment after one is a name in its own right there.
		for rest := e.Name; ; {
			k := bytes.IndexByte([]byte(rest), '\\')
			if k < 0 {
				break
			}
			rest = rest[k+1:]
			hasDotgit = hasDotgit || gitpath.IsNTFSDotGit(rest)
			if gitpath.IsNTFSDotGitmodules(rest) {
				if !e.IsSymlink() {
					o.foundGitmodules(e.OID)
				} else {
					retval += o.report(oid, gitobj.TypeTree, MsgGitmodulesSymlink,
						".gitmodules is a symbolic link")
				}
			}
		}

		switch e.Mode {
		case modeRegular | 0o755, modeRegular | 0o644, modeSymlink, modeDir, modeGitlink:
		case modeRegular | 0o664:
			// Early git wrote this mode. It is only an error under
			// --strict.
			if o.Strict {
				hasBadModes = true
			}
		default:
			hasBadModes = true
		}

		if prev != nil {
			switch verifyOrdered(prev.Mode, prev.Name, e.Mode, e.Name, &candidates) {
			case treeUnordered:
				notSorted = true
			case treeHasDups:
				hasDupEntries = true
			}
		}
		prev = e
	}
	if err != nil {
		retval += o.report(oid, gitobj.TypeTree, MsgBadTree, "cannot be parsed as a tree")
	}

	if hasNullSHA1 {
		retval += o.report(oid, gitobj.TypeTree, MsgNullSha1, "contains entries pointing to null sha1")
	}
	if hasFullPath {
		retval += o.report(oid, gitobj.TypeTree, MsgFullPathname, "contains full pathnames")
	}
	if hasEmptyName {
		retval += o.report(oid, gitobj.TypeTree, MsgEmptyName, "contains empty pathname")
	}
	if hasDot {
		retval += o.report(oid, gitobj.TypeTree, MsgHasDot, "contains '.'")
	}
	if hasDotdot {
		retval += o.report(oid, gitobj.TypeTree, MsgHasDotdot, "contains '..'")
	}
	if hasDotgit {
		retval += o.report(oid, gitobj.TypeTree, MsgHasDotgit, "contains '.git'")
	}
	if hasZeroPad {
		retval += o.report(oid, gitobj.TypeTree, MsgZeroPaddedFilemode, "contains zero-padded file modes")
	}
	if hasBadModes {
		retval += o.report(oid, gitobj.TypeTree, MsgBadFilemode, "contains bad file modes")
	}
	if hasDupEntries {
		retval += o.report(oid, gitobj.TypeTree, MsgDuplicateEntries, "contains duplicate file entries")
	}
	if notSorted {
		retval += o.report(oid, gitobj.TypeTree, MsgTreeNotSorted, "not properly sorted")
	}
	if hasLargeName {
		retval += o.report(oid, gitobj.TypeTree, MsgLargePathname, "contains excessively large pathname")
	}
	return retval
}

// Tree ordering results, matching git's TREE_UNORDERED and TREE_HAS_DUPS.
const (
	treeOrdered = iota
	treeUnordered
	treeHasDups
)

func isLessThanSlash(c byte) bool { return c > 0 && c < '/' }

// verifyOrdered is git's verify_ordered(). Tree entries sort in path order,
// which means a directory sorts as though its name ended in a slash.
func verifyOrdered(mode1 uint32, name1 string, mode2 uint32, name2 string, candidates *[]string) int {
	l := min(len(name1), len(name2))
	switch cmp := bytes.Compare([]byte(name1[:l]), []byte(name2[:l])); {
	case cmp < 0:
		return treeOrdered
	case cmp > 0:
		return treeUnordered
	}
	var c1, c2 byte
	if len(name1) > l {
		c1 = name1[l]
	}
	if len(name2) > l {
		c2 = name2[l]
	}
	if c1 == 0 && c2 == 0 {
		// git-write-tree once wrote a tree with the same name twice,
		// one blob and one tree. Refuse it.
		return treeHasDups
	}
	if c1 == 0 && mode1&0o170000 == modeDir {
		c1 = '/'
	}
	if c2 == 0 && mode2&0o170000 == modeDir {
		c2 = '/'
	}
	// The implied slash creates duplicates that are not adjacent, as in
	// "foo", "foo.bar", "foo.bar/", "foo/". Remember each non-directory
	// candidate and test every directory against the ones still pending.
	if c1 == 0 && isLessThanSlash(c2) {
		*candidates = append(*candidates, name1)
	} else if c2 == '/' && isLessThanSlash(c1) {
		for len(*candidates) > 0 {
			f := (*candidates)[len(*candidates)-1]
			*candidates = (*candidates)[:len(*candidates)-1]
			if len(name2) < len(f) || name2[:len(f)] != f {
				continue
			}
			p := name2[len(f):]
			if p == "" {
				return treeHasDups
			}
			if isLessThanSlash(p[0]) {
				*candidates = append(*candidates, f)
				break
			}
		}
	}
	if c1 < c2 {
		return treeOrdered
	}
	return treeUnordered
}

func (o *Options) foundGitmodules(oid gitobj.OID) {
	o.mu.Lock()
	o.gitmodulesFound.Add(oid)
	o.mu.Unlock()
}

func (o *Options) foundGitattributes(oid gitobj.OID) {
	o.mu.Lock()
	o.gitattributesFound.Add(oid)
	o.mu.Unlock()
}
