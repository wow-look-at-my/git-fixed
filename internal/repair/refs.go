package repair

// Repairing a reference whose own file is broken.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// RepairedRef is one reference put back, and what it was set to.
type RepairedRef struct {
	Name string
	OID  gitobj.OID
	// From says where the value came from, for the report.
	From string
}

// repairRef restores one malformed reference from its reflog.
//
// The reflog records every value the ref has held, newest last. The newest one
// whose object the repository still has is the value the ref had when it was
// last usable, so restoring it loses nothing.
func repairRef(repo *gitrepo.Repo, db *odb.DB, q *Quarantine, bad BadRef) (RepairedRef, error) {
	worktreeDir, name := splitRefName(repo, bad.Name)
	entries := repo.Reflog(worktreeDir, name)
	for i := len(entries) - 1; i >= 0; i-- {
		oid := entries[i].New
		if !oid.Valid() || !db.Has(oid) {
			continue
		}
		if _, _, err := db.Read(oid); err != nil {
			continue
		}
		if err := writeRef(repo, q, worktreeDir, name, bad, oid); err != nil {
			return RepairedRef{}, err
		}
		return RepairedRef{Name: bad.Name, OID: oid, From: "its reflog"}, nil
	}
	return RepairedRef{}, fmt.Errorf("nothing in the reflog for %s still resolves", bad.Name)
}

// writeRef replaces a ref file, quarantining whatever was there first.
func writeRef(repo *gitrepo.Repo, q *Quarantine, worktreeDir, name string, bad BadRef, oid gitobj.OID) error {
	path := bad.Path
	if path == "" {
		dir := worktreeDir
		if dir == "" {
			dir = repo.CommonDir
		}
		path = filepath.Join(dir, filepath.FromSlash(name))
	}
	if err := q.Take(path, "the ref file would not parse"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(oid.String()+"\n"), 0o666)
}

// splitRefName separates a worktree-qualified ref name into the worktree it
// belongs to and the name that worktree knows it by.
//
// A linked worktree's own refs are printed as "worktrees/<id>/<name>", which is
// how fsck names them and how the scan records them.
func splitRefName(repo *gitrepo.Repo, name string) (worktreeDir, refName string) {
	for _, wt := range repo.Worktrees() {
		if wt.IsMain {
			continue
		}
		prefix := "worktrees/" + wt.ID + "/"
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			return wt.Dir, name[len(prefix):]
		}
	}
	main := repo.Worktrees()
	if len(main) > 0 {
		return main[0].Dir, name
	}
	return repo.CommonDir, name
}
