package gitrepo

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Ref is one reference. A broken reference keeps a zero OID, which is what git
// hands its callbacks when it iterates references including the broken ones.
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
	for _, ref := range looseRefs(filepath.Join(r.CommonDir, "refs"), "refs", r.Algo, r.CommonDir) {
		if worktreeDir != r.CommonDir && isPerWorktree(ref.Name) {
			continue
		}
		byName[ref.Name] = ref
	}
	if worktreeDir != "" && worktreeDir != r.CommonDir {
		for _, ref := range looseRefs(filepath.Join(worktreeDir, "refs"), "refs", r.Algo, worktreeDir) {
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

// packedRefs reads the packed-refs table.
func (r *Repo) packedRefs() []Ref {
	f, err := os.Open(filepath.Join(r.CommonDir, "packed-refs"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Ref
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' || line[0] == '^' {
			continue
		}
		if len(line) < r.Algo.HexSize+2 {
			continue
		}
		oid, ok := r.Algo.ParseHexBytes(line)
		if !ok || line[r.Algo.HexSize] != ' ' {
			continue
		}
		out = append(out, Ref{Name: string(line[r.Algo.HexSize+1:]), OID: oid})
	}
	return out
}

// looseRefs walks a refs directory. root is the store the reference belongs to,
// which is where a symbolic reference resolves against.
func looseRefs(dir, prefix string, algo *gitobj.Algo, root string) []Ref {
	var out []Ref
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not a reference
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		ref := readRefFile(path, name, algo, root, 0)
		out = append(out, ref)
		return nil
	})
	return out
}

// readRefFile reads one reference file, following a symbolic reference.
func readRefFile(path, name string, algo *gitobj.Algo, root string, depth int) Ref {
	ref := Ref{Name: name, Broken: true}
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
		next := readRefFile(filepath.Join(root, filepath.FromSlash(ref.Symref)), name, algo, root, depth+1)
		ref.OID = next.OID
		ref.Broken = next.Broken
		return ref
	}
	oid, ok := algo.ParseHexBytes(line)
	if !ok || len(line) != algo.HexSize {
		return ref
	}
	ref.OID = oid
	ref.Broken = false
	return ref
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
		ref := readRefFile(filepath.Join(worktreeDir, filepath.FromSlash(target)), target, r.Algo, worktreeDir, 0)
		if ref.Broken {
			// The branch does not exist yet, which is not an error on
			// its own: git calls it an unborn branch.
			return target, r.Algo.Null(), true
		}
		return target, ref.OID, true
	}
	oid, valid := r.Algo.ParseHexBytes([]byte(line))
	if !valid || len(line) != r.Algo.HexSize {
		return "", gitobj.OID{}, false
	}
	// A detached HEAD resolves to itself, which is how git spots one.
	return "HEAD", oid, true
}
