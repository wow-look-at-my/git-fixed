package gitrepo

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Ref is one reference.
type Ref struct {
	Name   string
	OID    gitobj.OID
	Symref string // the target, for a symbolic reference
	Broken bool
}

// perWorktreePrefixes are the references a linked worktree keeps to itself.
var perWorktreePrefixes = []string{"refs/bisect/", "refs/worktree/", "refs/rewritten/"}

func isPerWorktree(name string) bool {
	for _, p := range perWorktreePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Refs lists every reference under refs/, sorted by name, with loose files
// shadowing the packed table.
func (r *Repo) Refs(worktreeDir string) []Ref {
	byName := map[string]Ref{}
	for _, ref := range r.packedRefs() {
		byName[ref.Name] = ref
	}
	for _, ref := range r.looseRefs(filepath.Join(r.CommonDir, "refs"), "refs", r.Algo, r.CommonDir) {
		if worktreeDir != r.CommonDir && isPerWorktree(ref.Name) {
			continue
		}
		byName[ref.Name] = ref
	}
	if worktreeDir != "" && worktreeDir != r.CommonDir {
		for _, ref := range r.looseRefs(filepath.Join(worktreeDir, "refs"), "refs", r.Algo, worktreeDir) {
			if isPerWorktree(ref.Name) {
				byName[ref.Name] = ref
			}
		}
	}
	out := make([]Ref, 0, len(byName))
	for _, ref := range byName {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// noteBadPackedLine records the first line of packed-refs that git's own reader
// would refuse, as the message git dies with. The run prints it and stops, as
// git does.
func (r *Repo) noteBadPackedLine(kind, line string) {
	if r.PackedRefsFatal == "" {
		r.PackedRefsFatal = kind + " line in " + filepath.Join(r.DisplayGitDir, "packed-refs") + ": " + line
	}
}

// packedRefs reads the packed-refs table.
func (r *Repo) packedRefs() []Ref {
	data, err := os.ReadFile(filepath.Join(r.CommonDir, "packed-refs"))
	if err != nil {
		return nil
	}
	var out []Ref
	for off, first := 0, true; off < len(data); first = false {
		wasFirst := first
		var line []byte
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			line, off = data[off:off+i], off+i+1
		} else {
			// git reads the file whole, so a last line with no newline is one it refuses rather than one it ignores.
			line, off = data[off:], len(data)
			r.noteBadPackedLine("unterminated", string(line))
		}
		if len(line) == 0 {
			continue
		}
		if line[0] == '#' {
			// Only the header line may start with a comment, and only
			// with the text git writes there.
			if !wasFirst || !bytes.HasPrefix(line, []byte("# pack-refs with: ")) {
				r.noteBadPackedLine("unexpected", string(line))
			}
			continue
		}
		if line[0] == '^' {
			// A peeled line carries what the tag above it points at.
			if _, ok := r.Algo.ParseHexBytes(line[1:]); !ok || len(line) != r.Algo.HexSize+1 {
				r.noteBadPackedLine("unexpected", string(line))
			}
			continue
		}
		if len(line) < r.Algo.HexSize+2 {
			r.noteBadPackedLine("unexpected", string(line))
			continue
		}
		oid, ok := r.Algo.ParseHexBytes(line)
		if !ok || line[r.Algo.HexSize] != ' ' {
			r.noteBadPackedLine("unexpected", string(line))
			continue
		}
		ref := Ref{Name: string(line[r.Algo.HexSize+1:]), OID: oid}
		if !fsck.CheckRefnameFormat(ref.Name, 0) {
			ref.Broken = true
			ref.OID = r.Algo.Null()
		}
		out = append(out, ref)
	}
	return out
}

// looseRefs walks a refs directory. root is the store the reference belongs to,
// which is where a symbolic reference resolves against.
func (r *Repo) looseRefs(dir, prefix string, algo *gitobj.Algo, root string) []Ref {
	var out []Ref
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not a reference
		}
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			// Opening a device or a pipe here would block forever.
			return nil
		}
		if base := filepath.Base(path); base[0] == '.' {
			// git's directory walk never yields a dot file, so such a file is not a reference at all.
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		ref := r.readRefFile(path, name, algo, root, 0)
		if !fsck.CheckRefnameFormat(name, 0) {
			// A name no reference may carry makes the reference itself broken, whatever the file holds.
			ref.Broken = true
			ref.OID = algo.Null()
		}
		out = append(out, ref)
		return nil
	})
	return out
}

// readRefFile reads one reference file, following a symbolic reference. A file
// it cannot make sense of leaves the null object name, which is what git
// reports for a reference that resolves to nothing.
func (r *Repo) readRefFile(path, name string, algo *gitobj.Algo, root string, depth int) Ref {
	ref := Ref{Name: name, Broken: true, OID: algo.Null()}
	data, err := os.ReadFile(path)
	if err != nil {
		return ref
	}
	line := bytes.TrimRight(data, " \t\r\n")
	if target, ok := bytes.CutPrefix(line, []byte("ref:")); ok {
		ref.Symref = string(bytes.TrimSpace(target))
		if depth > 5 {
			return ref
		}
		next := r.readRefFile(filepath.Join(root, filepath.FromSlash(ref.Symref)), name, algo, root, depth+1)
		if next.Broken {
			// The target has no loose file, so the packed table is where it is.
			if oid, ok := r.packedMap()[ref.Symref]; ok {
				ref.OID = oid
				ref.Broken = false
				return ref
			}
		}
		ref.OID = next.OID
		ref.Broken = next.Broken
		return ref
	}
	oid, ok := algo.ParseHexBytes(line)
	if !ok {
		return ref
	}
	// git accepts anything after the name as long as a space separates it.
	if rest := line[algo.HexSize:]; len(rest) > 0 && !isSpace(rest[0]) {
		return ref
	}
	ref.OID = oid
	ref.Broken = false
	return ref
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// Head resolves one worktree's HEAD without following it to an object.
func (r *Repo) Head(worktreeDir string) (target string, oid gitobj.OID, ok bool) {
	if worktreeDir == "" {
		worktreeDir = r.CommonDir
	}
	data, err := os.ReadFile(filepath.Join(worktreeDir, "HEAD"))
	if err != nil {
		return "", gitobj.OID{}, false
	}
	line := strings.TrimRight(string(data), " \t\r\n")
	if rest, found := strings.CutPrefix(line, "ref:"); found {
		target = strings.TrimSpace(rest)
		store := worktreeDir
		if !isPerWorktree(target) && target != "HEAD" {
			store = r.CommonDir
		}
		if oid, ok := r.Resolve(store, target, 0); ok {
			return target, oid, true
		}
		// The branch does not exist yet, which is not an error on its own: git calls it an unborn branch.
		return target, r.Algo.Null(), true
	}
	oid, valid := r.Algo.ParseHexBytes([]byte(line))
	if !valid || len(line) != r.Algo.HexSize {
		return "", gitobj.OID{}, false
	}
	// A detached HEAD resolves to itself, which is how git spots one.
	return "HEAD", oid, true
}

// packedMap builds a lookup of the packed reference table, so a symbolic
// reference can resolve to a target that has no file of its own.
func (r *Repo) packedMap() map[string]gitobj.OID {
	r.packedOnce.Do(func() {
		r.packed = map[string]gitobj.OID{}
		for _, ref := range r.packedRefs() {
			r.packed[ref.Name] = ref.OID
		}
	})
	return r.packed
}

// Resolve follows a reference name to an object, through symbolic references
// and through the packed table. It reports ok=false for a name that leads
// nowhere, which is what git calls a broken reference.
func (r *Repo) Resolve(store, name string, depth int) (gitobj.OID, bool) {
	if depth > 5 {
		return gitobj.OID{}, false
	}
	ref := r.readRefFile(filepath.Join(store, filepath.FromSlash(name)), name, r.Algo, store, 0)
	if !ref.Broken {
		return ref.OID, true
	}
	if ref.Symref != "" {
		return r.Resolve(store, ref.Symref, depth+1)
	}
	if oid, ok := r.packedMap()[name]; ok {
		return oid, true
	}
	return gitobj.OID{}, false
}
