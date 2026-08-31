package gitrepo

import (
	"os"
	"path/filepath"
	"sort"
)

// Worktree is a checkout attached to a repository.
type Worktree struct {
	// Dir is where this worktree's own HEAD, index, and logs live.
	Dir string
	// Path is the checkout, empty for a bare repository.
	Path string
	// IsMain says whether this is the repository's own worktree.
	IsMain bool
	// ID is the directory name under worktrees/, empty for the main worktree.
	ID string
}

// IndexPath is the index file this worktree uses.
func (w *Worktree) IndexPath() string { return filepath.Join(w.Dir, "index") }

// Worktrees lists the main worktree before the linked ones, ordered by name,
// which is the order git's get_worktrees() returns.
func (r *Repo) Worktrees() []*Worktree {
	out := []*Worktree{{Dir: r.CommonDir, Path: r.WorkTree, IsMain: true}}
	entries, err := os.ReadDir(filepath.Join(r.CommonDir, "worktrees"))
	if err != nil {
		return out
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		dir := filepath.Join(r.CommonDir, "worktrees", n)
		if _, err := os.Stat(filepath.Join(dir, "gitdir")); err != nil {
			continue
		}
		wt := &Worktree{Dir: dir, ID: n}
		if data, err := os.ReadFile(filepath.Join(dir, "gitdir")); err == nil {
			wt.Path = filepath.Dir(trimSpace(string(data)))
		}
		out = append(out, wt)
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// RefName renders a per-worktree reference the way git's strbuf_worktree_ref()
// does: the main worktree uses the plain name, a linked worktree is prefixed.
func (w *Worktree) RefName(name string) string {
	if w.IsMain || w.ID == "" {
		return name
	}
	if name == "HEAD" || isPerWorktree(name) {
		return "worktrees/" + w.ID + "/" + name
	}
	return name
}
