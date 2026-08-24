package repair

// Repairing a .git/index that will not parse.
//
// see docs/repair.md

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/go-containers/set"
)

// BadIndex is an index file git will not read.
type BadIndex struct {
	// Path is the index file, absolute.
	Path string
	// WorktreeDir is where this index's own HEAD lives, which is the repository directory for the main worktree.
	WorktreeDir string
	// Why is what git says about it.
	Why string
}

// RepairedIndex is what one index rebuild came to.
type RepairedIndex struct {
	// Path is the index that was rewritten.
	Path string
	// Salvaged is how many entries came out of the damaged file.
	Salvaged int
	// FromHead is how many more came from the commit HEAD names.
	FromHead int
	// Claimed is how many entries the damaged file said it held.
	Claimed int
	// Why is what was wrong with the old file.
	Why string
}

// StagedWorkLost reports whether the rebuild could not account for every entry the old index claimed.
func (r RepairedIndex) StagedWorkLost() int { return max(r.Claimed-r.Salvaged, 0) }

// scanIndexes finds every worktree's index that will not parse.
func (s *scanner) scanIndexes(d *Damage) {
	for _, wt := range s.repo.Worktrees() {
		path := filepath.Join(wt.Dir, "index")
		if _, err := os.Stat(path); err != nil {
			// No index at all is a normal state for a bare repository, and git makes one on demand anywhere else.
			continue
		}
		_, _, err := s.repo.ReadIndex(path)
		if err == nil {
			continue
		}
		d.Index = &BadIndex{Path: path, WorktreeDir: wt.Dir, Why: err.Error()}
		// One is enough to report. A second pass picks up the next.
		return
	}
}

// repairIndex writes a whole index from what the damaged one still yields,
// filling the rest in from the commit HEAD names.
//
// The index is not a derived file, whatever its name suggests: it records which
// paths are staged, at which mode, holding which content. Rebuilding it from
// HEAD alone would silently unstage everything. So the damaged file is read as
// far as it goes and every entry that survives is kept, HEAD supplies only the
// paths the salvage did not reach, and the original goes to quarantine whole.
func repairIndex(repo *gitrepo.Repo, db *odb.DB, q *Quarantine, bad *BadIndex) (RepairedIndex, error) {
	out := RepairedIndex{Path: repo.Shown(bad.Path), Why: bad.Why}

	salvaged, err := repo.SalvageIndex(bad.Path)
	if err != nil {
		return out, err
	}
	out.Claimed = salvaged.Count
	out.Salvaged = len(salvaged.Entries)

	entries := salvaged.Entries
	have := set.New[string]()
	for _, e := range entries {
		have.Add(e.Name)
	}

	// HEAD's tree covers the tracked paths the salvage did not reach.
	head := headEntries(repo, db, bad.WorktreeDir)
	for _, e := range head {
		if !have.Add(e.Name) {
			continue
		}
		entries = append(entries, e)
		out.FromHead++
	}

	if err := q.Take(bad.Path, "an index file that would not parse"); err != nil {
		return out, err
	}
	if err := repo.WriteIndex(bad.Path, entries); err != nil {
		return out, fmt.Errorf("writing %s: %w", bad.Path, err)
	}
	return out, nil
}

// headEntries lists what the commit HEAD names has at every path.
//
// It returns nothing when HEAD does not resolve or its tree cannot be read.
// That is not a failure here: an index with no HEAD to fall back on is still
// rebuilt from whatever the salvage produced.
func headEntries(repo *gitrepo.Repo, db *odb.DB, worktreeDir string) []gitrepo.IndexEntry {
	_, oid, ok := repo.Head(worktreeDir)
	if !ok {
		return nil
	}
	typ, data, err := db.Read(oid)
	if err != nil || typ != gitobj.TypeCommit {
		return nil
	}
	var tree gitobj.OID
	for _, line := range strings.Split(string(data), "\n") {
		if hex, ok := strings.CutPrefix(line, "tree "); ok {
			if t, ok := repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				tree = t
			}
			break
		}
	}
	if !tree.Valid() {
		return nil
	}
	var out []gitrepo.IndexEntry
	walkTreeEntries(db, repo, tree, "", &out, 0)
	return out
}

// walkTreeEntries flattens a tree into index entries.
func walkTreeEntries(db *odb.DB, repo *gitrepo.Repo, tree gitobj.OID, prefix string, out *[]gitrepo.IndexEntry, depth int) {
	if depth > 100 {
		// A tree cannot legitimately nest this deep, and a cycle here would not terminate.
		return
	}
	typ, data, err := db.Read(tree)
	if err != nil || typ != gitobj.TypeTree {
		return
	}
	entries, _ := fsck.ParseTree(data, repo.Algo)
	for _, e := range entries {
		name := prefix + string(e.Name)
		switch {
		case e.IsGitlink():
			*out = append(*out, gitrepo.IndexEntry{Mode: e.Mode, OID: e.OID, Name: name})
		case e.IsDir():
			walkTreeEntries(db, repo, e.OID, name+"/", out, depth+1)
		default:
			*out = append(*out, gitrepo.IndexEntry{Mode: e.Mode, OID: e.OID, Name: name})
		}
	}
}
