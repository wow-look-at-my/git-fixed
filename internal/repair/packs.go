package repair

// Repairing a packfile the object database cannot read end to end.
//
// see docs/repair.md

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// BadPack is a packfile that will not verify.
type BadPack struct {
	// Pack and Idx are absolute paths.
	Pack string
	Idx  string
	// Why is the first complaint the verification made.
	Why string
	// Readable says the index maps and the pack's header agrees with it, so
	// the objects inside can still be listed and decoded one by one. A pack
	// that fails this is never displaced: without its index there is no way
	// to get the objects out, and moving it away would lose them.
	Readable bool
}

// RescuedPack is one pack this run took out of the repository.
type RescuedPack struct {
	// Pack is the pack's path, as a person would recognise it.
	Pack string
	// Extracted is how many objects were written back as loose objects.
	Extracted int
	// Present is how many were already outside the pack and needed no copy.
	Present int
	// Lost is how many entries the pack would not decode. Each one is left
	// for the recovery ladder, and reported if no source has it.
	Lost int
}

// companionSuffixes are the files that belong to one pack. All of them travel
// with it: an index or a reverse index left behind describes a pack that is no
// longer there, which is a second fault to repair.
var companionSuffixes = []string{".pack", ".idx", ".rev", ".bitmap", ".keep", ".promisor", ".mtimes"}

// scanPacks verifies every pack and records the ones that fail.
//
// This runs the same check `git fsck` runs, so a pack it accepts is a pack git
// accepts. It costs a full read of every pack, which is the price of knowing
// rather than guessing.
func (s *scanner) scanPacks(d *Damage) {
	for _, p := range s.db.Packs() {
		if bad, ok := verifyPack(p); ok {
			d.Packs = append(d.Packs, bad)
		}
	}
}

// verifyPack reports whether one pack is damaged, and how.
func verifyPack(p *odb.Pack) (BadPack, bool) {
	bad := BadPack{Pack: p.File, Idx: p.IdxFile}
	if p.OpenErr != nil {
		bad.Why = p.OpenErr.Error()
		return bad, true
	}
	var first string
	ok := p.Verify(odb.VerifyOpts{
		Emit: func(_ gitobj.OID, text string) {
			if first == "" {
				first = text
			}
		},
		Workers: 1,
	})
	if ok && first == "" {
		return BadPack{}, false
	}
	bad.Why = first
	// The index mapped and the pack's own header agreed with it, or Verify
	// would have stopped before reaching any entry. So the entries can still
	// be listed, which is what makes extraction possible.
	bad.Readable = true
	return bad, true
}

// rescuePack writes out everything the pack still holds, then displaces it.
//
// The order is the whole point. A corrupt entry in a pack SHADOWS a good loose
// copy of the same object, because the database answers from packs first, so
// the object keeps reading as damaged however many times it is put back. Only
// removing the pack clears that. But removing a pack removes every object in
// it, so each one has to be on disk as a loose object first. Extract, then
// displace: never the other way round.
func rescuePack(repo *gitrepo.Repo, q *Quarantine, bad BadPack) (RescuedPack, error) {
	out := RescuedPack{Pack: displayPack(repo, bad.Pack)}
	if !bad.Readable {
		// Nothing can be got out of it, so it stays exactly where it is.
		// A pack left in place is a pack the owner can still hand to another
		// tool; a pack moved away with no copy of its objects is data lost by
		// the repair itself.
		return out, fmt.Errorf("%s: %s", out.Pack, bad.Why)
	}

	p, err := odb.OpenPack(bad.Idx, bad.Idx, repo.Algo, true)
	if err != nil {
		return out, fmt.Errorf("%s: %w", out.Pack, err)
	}
	defer p.Close()

	var writeErr error
	p.Verify(odb.VerifyOpts{
		Emit: func(gitobj.OID, string) {},
		Object: func(oid gitobj.OID, typ gitobj.Type, _ int64, data []byte) {
			if writeErr != nil {
				return
			}
			kept, err := keepLoose(repo, q, oid, typ, data)
			switch {
			case err != nil:
				writeErr = err
			case kept:
				out.Extracted++
			default:
				out.Present++
			}
		},
		// One worker, so quarantining a corrupt loose copy and writing over
		// it stay ordered without a lock. A repair is not a hot path.
		Workers: 1,
		// Zero, so every object arrives with its content. Above the
		// threshold Verify hashes a large blob by streaming and hands the
		// callback a nil payload, which is enough to check a pack and not
		// enough to rewrite one.
		BigFileThreshold: 0,
	})
	if writeErr != nil {
		return out, writeErr
	}
	out.Lost = int(p.Num) - out.Extracted - out.Present

	for _, path := range companions(bad.Pack) {
		if err := q.Take(path, "part of a packfile that would not verify"); err != nil {
			return out, err
		}
	}
	return out, nil
}

// keepLoose makes sure one object survives its pack, and reports whether it had
// to be written.
//
// An object that already has a readable loose copy needs nothing: that copy
// outlives the pack. A loose file that will NOT read back is quarantined first,
// because leaving it in place would shadow the copy being written and put the
// object right back where it started.
func keepLoose(repo *gitrepo.Repo, q *Quarantine, oid gitobj.OID, typ gitobj.Type, data []byte) (bool, error) {
	name := oid.String()
	path := filepath.Join(repo.ObjectsDir, name[:2], name[2:])
	if _, err := os.Stat(path); err == nil {
		// ReadLoose answers from this one file, never from a pack, so it is
		// the only check here that says anything about the copy that will be
		// left behind.
		res := odb.ReadLoose(path, path, oid, repo.Algo, 0)
		if !res.Failed {
			return false, nil
		}
		if err := q.Take(path, "a corrupt loose object, replaced from its pack"); err != nil {
			return false, err
		}
	}
	if _, err := odb.WriteLoose(repo.ObjectsDir, repo.Algo, typ, data, oid); err != nil {
		return false, fmt.Errorf("writing %s out of a packfile: %w", oid, err)
	}
	return true, nil
}

// companions lists the files that belong to one pack and are present.
func companions(packPath string) []string {
	base := strings.TrimSuffix(packPath, ".pack")
	var out []string
	for _, suffix := range companionSuffixes {
		path := base + suffix
		if _, err := os.Lstat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

// displayPack names a pack the way a person would recognise it, relative to the
// git directory rather than as the absolute path this process opened.
func displayPack(repo *gitrepo.Repo, path string) string {
	if rel, err := filepath.Rel(repo.CommonDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(filepath.Join(repo.DisplayGitDir, rel))
	}
	return path
}
