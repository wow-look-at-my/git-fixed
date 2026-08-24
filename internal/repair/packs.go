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
	"github.com/wow-look-at-my/git-fixed/internal/progress"
)

// BadPack is a packfile that will not verify.
type BadPack struct {
	// Pack and Idx are absolute paths.
	Pack string
	Idx  string
	// Why is the first complaint the verification made.
	Why string
	// Objects is how many the index says the pack holds, which is what the extraction below has to account for.
	Objects int
}

// RescuedPack is one pack this run took out of the repository.
type RescuedPack struct {
	// Pack is the pack's path, as a person would recognise it.
	Pack string
	// Extracted is how many objects were written back as loose objects.
	Extracted int
	// Present is how many were already outside the pack and needed no copy.
	Present int
	// Lost is how many entries the pack would not decode.
	Lost int
}

// companionSuffixes are the files that belong to one pack.
var companionSuffixes = []string{".pack", ".idx", ".rev", ".bitmap", ".keep", ".promisor", ".mtimes"}

// scanPacks verifies the packs nobody has verified yet, and records the ones
// that fail.
//
// This runs the same check `git fsck` runs, so a pack it accepts is a pack git
// accepts. It costs a full read of the pack, which is the price of knowing
// rather than guessing -- and it is why a pack the caller's fsck has already
// read is left alone here. Reading it again asks a question that has an answer.
func (s *scanner) scanPacks(d *Damage) {
	var todo []*odb.Pack
	total := int64(0)
	for _, p := range s.db.Packs() {
		if s.trusted[p.File] {
			continue
		}
		todo = append(todo, p)
		if p.OpenErr == nil {
			total += int64(p.Num)
		}
	}
	if len(todo) == 0 {
		return
	}
	m := s.meters.start("Verifying packs", total)
	defer m.Finish()
	for _, p := range todo {
		if bad, ok := verifyPack(p, m); ok {
			d.Packs = append(d.Packs, bad)
			continue
		}
		s.trust(p.File)
	}
}

// verifyPack reports whether one pack is damaged, and how.
func verifyPack(p *odb.Pack, m *progress.Meter) (BadPack, bool) {
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
		Progress: m.Step,
		// Every core, as the fsck above uses.
	})
	if ok && first == "" {
		return BadPack{}, false
	}
	bad.Why = first
	bad.Objects = int(p.Num)
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
//
// A pack that yields nothing is never displaced. Moving it would take every
// object in it out of the repository and buy nothing, since there is no loose
// copy for it to stop shadowing. It stays where it is and the run reports it.
func rescuePack(repo *gitrepo.Repo, q *Quarantine, bad BadPack) (RescuedPack, error) {
	out := RescuedPack{Pack: repo.Shown(bad.Pack)}

	p, err := odb.OpenPack(bad.Idx, bad.Idx, repo.Algo)
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
		// One worker, so quarantining a corrupt loose copy and writing over it stay ordered without a lock.
		Workers: 1,
		// Zero, so every object arrives with its content.
		BigFileThreshold: 0,
	})
	if writeErr != nil {
		return out, writeErr
	}
	out.Lost = int(p.Num) - out.Extracted - out.Present
	if out.Extracted == 0 && out.Present == 0 && p.Num > 0 {
		// Nothing came out.
		return out, fmt.Errorf("%s holds %d object(s) and yielded none of them: %s",
			out.Pack, p.Num, bad.Why)
	}

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
		// ReadLoose answers from this one file, never from a pack.
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
