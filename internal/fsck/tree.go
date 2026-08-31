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

// TreeEntry is a line of a tree object, with the mode exactly as stored.
type TreeEntry struct {
	Mode uint32
	Name []byte
	OID  gitobj.OID
	// Raw is the entry's bytes; the leading byte is what hasZeroPad below checks.
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

// WalkKind is the object a link walk follows this entry to, and whether it follows it at all.
func (e TreeEntry) WalkKind() (gitobj.Type, bool) {
	switch {
	case e.IsRegular(), e.IsSymlink():
		return gitobj.TypeBlob, true
	case e.IsDir():
		return gitobj.TypeTree, true
	}
	return gitobj.TypeNone, false
}

// errBadTree is git's decode_tree_entry() failure, which fsck turns into the message "cannot be parsed as a tree".
var errBadTree = errors.New("cannot be parsed as a tree")

// ParseTree decodes a whole tree object.
func ParseTree(buf []byte, algo *gitobj.Algo) ([]TreeEntry, error) {
	return ParseTreeInto(nil, buf, algo)
}

// ParseTreeInto decodes a tree into a truncated dst, so a caller that reads
// many trees reuses the same allocation instead of allocating fresh for each tree.
func ParseTreeInto(dst []TreeEntry, buf []byte, algo *gitobj.Algo) ([]TreeEntry, error) {
	out := dst[:0]
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

// decodeTreeEntry is git's decode_tree_entry(): "<octal mode> <name>" followed
// by a NUL byte and the hash.
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
		Name: buf[sp+1 : nameEnd],
		OID:  algo.FromRaw(buf[nameEnd+1:]),
		Raw:  buf[:sp],
	}
	return e, buf[nameEnd+1+rawsz:], nil
}

// Tree runs every check git makes on a tree object.
func (o *Options) Tree(ctx any, oid gitobj.OID, buf []byte) int {
	entries, err := ParseTree(buf, o.Algo)
	return o.TreeEntries(ctx, oid, entries, err)
}

// TreeEntries runs the tree checks over entries a caller already decoded, along
// with the error ParseTree gave it. Decoding a tree is the most expensive part
// of checking it, so a caller that needs the entries anyway passes them here
// instead of handing back the bytes.
func (o *Options) TreeEntries(ctx any, oid gitobj.OID, entries []TreeEntry, err error) int {
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
	if err != nil && len(entries) == 0 {
		return retval + o.report(ctx, oid, gitobj.TypeTree, MsgBadTree, "cannot be parsed as a tree")
	}

	var candidates [][]byte
	var prev *TreeEntry
	for i := range entries {
		e := &entries[i]
		hasNullSHA1 = hasNullSHA1 || e.OID.IsNull()
		hasFullPath = hasFullPath || bytes.IndexByte(e.Name, '/') >= 0
		hasEmptyName = hasEmptyName || len(e.Name) == 0
		hasDot = hasDot || string(e.Name) == "."
		hasDotdot = hasDotdot || string(e.Name) == ".."
		hasDotgit = hasDotgit || gitpath.IsDotGit(e.Name)
		hasZeroPad = hasZeroPad || (len(e.Raw) > 0 && e.Raw[0] == '0')
		hasLargeName = hasLargeName || len(e.Name) > o.MaxTreeEntryLen

		if gitpath.IsDotGitmodules(e.Name) {
			if !e.IsSymlink() {
				o.foundGitmodules(e.OID)
			} else {
				retval += o.report(ctx, oid, gitobj.TypeTree, MsgGitmodulesSymlink,
					".gitmodules is a symbolic link")
			}
		}
		if gitpath.IsDotGitattributes(e.Name) {
			if !e.IsSymlink() {
				o.foundGitattributes(e.OID)
			} else {
				retval += o.report(ctx, oid, gitobj.TypeTree, MsgGitattributesSymlink,
					".gitattributes is a symlink")
			}
		}
		if e.IsSymlink() {
			if gitpath.IsDotGitignore(e.Name) {
				retval += o.report(ctx, oid, gitobj.TypeTree, MsgGitignoreSymlink,
					".gitignore is a symlink")
			}
			if gitpath.IsDotMailmap(e.Name) {
				retval += o.report(ctx, oid, gitobj.TypeTree, MsgMailmapSymlink,
					".mailmap is a symlink")
			}
		}

		// NTFS reads a backslash as a directory separator, so each
		// segment after it is a name in its own right there.
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
					retval += o.report(ctx, oid, gitobj.TypeTree, MsgGitmodulesSymlink,
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
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgBadTree, "cannot be parsed as a tree")
	}

	if hasNullSHA1 {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgNullSha1, "contains entries pointing to null sha1")
	}
	if hasFullPath {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgFullPathname, "contains full pathnames")
	}
	if hasEmptyName {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgEmptyName, "contains empty pathname")
	}
	if hasDot {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgHasDot, "contains '.'")
	}
	if hasDotdot {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgHasDotdot, "contains '..'")
	}
	if hasDotgit {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgHasDotgit, "contains '.git'")
	}
	if hasZeroPad {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgZeroPaddedFilemode, "contains zero-padded file modes")
	}
	if hasBadModes {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgBadFilemode, "contains bad file modes")
	}
	if hasDupEntries {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgDuplicateEntries, "contains duplicate file entries")
	}
	if notSorted {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgTreeNotSorted, "not properly sorted")
	}
	if hasLargeName {
		retval += o.report(ctx, oid, gitobj.TypeTree, MsgLargePathname, "contains excessively large pathname")
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
func verifyOrdered(mode1 uint32, name1 []byte, mode2 uint32, name2 []byte, candidates *[][]byte) int {
	l := min(len(name1), len(name2))
	switch cmp := bytes.Compare(name1[:l], name2[:l]); {
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
		// git-write-tree could write a tree with the same name for both a blob and a tree entry.
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
			if len(name2) < len(f) || !bytes.Equal(name2[:len(f)], f) {
				continue
			}
			p := name2[len(f):]
			if len(p) == 0 {
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
