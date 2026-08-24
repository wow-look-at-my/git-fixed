package fsckcmd

import (
	"fmt"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// link is one reference an object makes to another.
type link struct {
	oid  gitobj.OID
	typ  gitobj.Type // the type the reference implies, TypeAny for a tag target
	name string      // the readable name, empty unless --name-objects is on
	// viaTag marks a tag's target, which git accepts at any type.
	viaTag bool
}

// walkLinks returns what an object points at, along with the parse errors git
// would report while reading it.
func walkLinks(typ gitobj.Type, oid gitobj.OID, buf []byte, algo *gitobj.Algo, name string, named bool) ([]link, []string) {
	switch typ {
	case gitobj.TypeTree:
		return treeLinks(buf, algo, name, named)
	case gitobj.TypeCommit:
		return commitLinks(oid, buf, algo, name, named)
	case gitobj.TypeTag:
		return tagLinks(oid, buf, algo, name, named)
	}
	return nil, nil
}

func treeLinks(buf []byte, algo *gitobj.Algo, name string, named bool) ([]link, []string) {
	// git's tree walk stops at the entry it cannot decode.
	entries, _ := fsck.ParseTree(buf, algo)
	return treeLinksFrom(entries, name, named), nil
}

// treeLinksFrom builds the links of a tree that is already decoded.
func treeLinksFrom(entries []fsck.TreeEntry, name string, named bool) []link {
	out := make([]link, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		typ, follow := e.WalkKind()
		if !follow {
			// A submodule commit is not part of this repository. see fsck.TreeEntry.WalkKind.
			continue
		}
		l := link{oid: e.OID, typ: typ}
		if named && name != "" {
			l.name = name + string(e.Name)
			if typ == gitobj.TypeTree {
				l.name += "/"
			}
		}
		out = append(out, l)
	}
	return out
}

// treeEdges resolves a decoded tree straight into the compact edges the
// connectivity walk keeps, without the intermediate links. A tree entry is the
// most numerous thing in a repository, so this is the path that decides how
// much memory a run costs.
func (r *run) treeEdges(entries []fsck.TreeEntry) (span edgeSpan, edges []edge, broken bool) {
	span, edges = r.objs.arena.alloc(len(entries))
	edges = edges[:0]
	for i := range entries {
		e := &entries[i]
		typ, follow := e.WalkKind()
		if !follow {
			// A submodule commit is not part of this repository. see fsck.TreeEntry.WalkKind.
			continue
		}
		target, idx, ok := r.objs.Lookup(e.OID, typ)
		if !ok {
			broken = true
		} else {
			target.SetFlag(flagUsed)
		}
		edges = append(edges, makeEdge(idx, ok, typ, false))
	}
	return span, edges, broken
}

func commitLinks(oid gitobj.OID, buf []byte, algo *gitobj.Algo, name string, named bool) ([]link, []string) {
	hexsz := algo.HexSize
	// parse_commit_buffer insists on "tree <hex>\n" before anything else.
	if len(buf) <= hexsz+5 || string(buf[:5]) != "tree " || buf[5+hexsz] != '\n' {
		return nil, []string{fmt.Sprintf("bogus commit object %s", oid)}
	}
	tree, ok := algo.ParseHexBytes(buf[5:])
	if !ok {
		return nil, []string{fmt.Sprintf("bad tree pointer in commit %s", oid)}
	}
	var out []link
	l := link{oid: tree, typ: gitobj.TypeTree}
	if named && name != "" {
		l.name = name + ":"
	}
	out = append(out, l)
	buf = buf[6+hexsz:]

	// A parent's name follows from the commit's own: the first parent gets "^" or continues a "~<n>" run.
	generation, prefixLen := 0, 0
	if named && name != "" {
		n := len(name)
		if n > 0 && name[n-1] == '^' {
			generation, prefixLen = 1, n-1
		} else {
			power := 1
			for n > 0 && name[n-1] >= '0' && name[n-1] <= '9' {
				n--
				generation += power * int(name[n]-'0')
				power *= 10
			}
			if power > 1 && n > 0 && name[n-1] == '~' {
				prefixLen = n - 1
			} else {
				generation, prefixLen = 0, n
			}
		}
	}
	counter := 0
	for len(buf) > hexsz+7 && string(buf[:7]) == "parent " {
		parent, ok := algo.ParseHexBytes(buf[7:])
		if !ok || buf[7+hexsz] != '\n' {
			return out, []string{fmt.Sprintf("bad parents in commit %s", oid)}
		}
		pl := link{oid: parent, typ: gitobj.TypeCommit}
		if named && name != "" {
			switch {
			case counter > 0:
				pl.name = fmt.Sprintf("%s^%d", name, counter+1)
			case generation > 0:
				pl.name = fmt.Sprintf("%s~%d", name[:prefixLen], generation+1)
			default:
				pl.name = name + "^"
			}
		}
		counter++
		out = append(out, pl)
		buf = buf[8+hexsz:]
	}
	return out, nil
}

func tagLinks(oid gitobj.OID, buf []byte, algo *gitobj.Algo, name string, named bool) ([]link, []string) {
	hexsz := algo.HexSize
	if len(buf) < hexsz+24 {
		return nil, []string{fmt.Sprintf("bad tag pointer in %s", oid)}
	}
	if string(buf[:7]) != "object " {
		return nil, []string{fmt.Sprintf("bad tag pointer in %s", oid)}
	}
	target, ok := algo.ParseHexBytes(buf[7:])
	if !ok || buf[7+hexsz] != '\n' {
		return nil, []string{fmt.Sprintf("bad tag pointer in %s", oid)}
	}
	buf = buf[8+hexsz:]
	if len(buf) < 5 || string(buf[:5]) != "type " {
		return nil, []string{fmt.Sprintf("bad tag pointer in %s", oid)}
	}
	buf = buf[5:]
	nl := -1
	for i := range buf {
		if buf[i] == '\n' {
			nl = i
			break
		}
	}
	if nl < 0 || nl >= 20 {
		return nil, []string{fmt.Sprintf("bad tag pointer in %s", oid)}
	}
	typeName := string(buf[:nl])
	typ := gitobj.TypeFromName(typeName)
	if typ == gitobj.TypeBad {
		return nil, []string{fmt.Sprintf("unknown tag type '%s' in %s", typeName, oid)}
	}
	l := link{oid: target, typ: typ, name: name, viaTag: true}
	if !named {
		l.name = ""
	}
	return []link{l}, nil
}
