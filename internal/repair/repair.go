package repair

// The run: scan, recover, apply, verify.

import (
	"fmt"
	"io"
	"sort"

	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/go-containers/set"
)

// Options are one run's settings.
type Options struct {
	// Dir is where to look for the repository.
	Dir string
	// DryRun plans the repair and prints it without touching anything.
	DryRun bool
	// Run names the quarantine directory. The caller supplies it so a run is
	// reproducible and so the name can be printed before the work starts.
	Run string

	Stdout io.Writer
	Stderr io.Writer
}

// Result is what a run did.
type Result struct {
	// Derived are the rebuildable caches that were displaced.
	Derived []string
	// Objects are the objects put back, and where each came from.
	Objects []Recovered
	// Refs are the references restored.
	Refs []RepairedRef
	// Unrecovered are the objects no source had. A run with any of these has
	// failed, and nothing about them was deleted.
	Unrecovered []BadObject
	// RemoteError says why the remote could not answer, when one was needed
	// and could not be reached. An unreachable remote is a different thing
	// from an object that is gone, and the report must not confuse them.
	RemoteError error
	// Quarantine names the run's directory, empty when nothing was displaced.
	Quarantine string
	// Clean says whether a second scan found the repository whole.
	Clean bool
}

// Ok reports whether the run left the repository healthy.
func (r *Result) Ok() bool { return len(r.Unrecovered) == 0 && r.Clean }

// Nothing reports whether the run found a repository with nothing wrong.
func (r *Result) Nothing() bool { return r.Clean && r.idle() }

// FoundNothingToDo reports the awkward case: this tool saw nothing it repairs,
// and fsck is still unhappy. The damage is real and belongs to something not
// covered here -- a corrupt pack, an index that will not parse, a malformed
// packed-refs. Reporting it is the point; a quiet exit would read as health.
func (r *Result) FoundNothingToDo() bool { return !r.Clean && r.idle() }

// idle reports whether the run changed nothing and found nothing.
func (r *Result) idle() bool {
	return len(r.Objects) == 0 && len(r.Derived) == 0 &&
		len(r.Refs) == 0 && len(r.Unrecovered) == 0
}

// Run repairs the repository and reports what it did.
//
// It never deletes: a displaced file goes to the run's quarantine directory and
// can be put back. It never rewrites history, and it never moves a reference
// backwards to route around a missing object. An object no source has is
// reported and the run fails, because a repository that passes fsck because its
// broken parts were removed is not repaired.
func Run(o *Options) (*Result, error) {
	repo, db, err := open(o.Dir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	damage, err := Scan(repo, db)
	if err != nil {
		return nil, err
	}

	res := &Result{}
	if damage.Empty() {
		// Ask fsck even here. The scan finds the damage this package can
		// repair; fsck finds the rest. Saying "nothing to repair" off the
		// narrower check would tell someone their repository is fine when
		// git still refuses to use it.
		res.Clean, err = verify(o.Dir)
		return res, err
	}
	if o.DryRun {
		return plan(damage, res), nil
	}

	q := NewQuarantine(repo.CommonDir, o.Run)
	for _, path := range damage.Derived {
		if err := q.Take(path, "a rebuildable cache that would not parse"); err != nil {
			return nil, err
		}
		res.Derived = append(res.Derived, path)
	}

	// Repair goes round until it stops making progress.
	//
	// A pass can only see the damage it can reach. A missing tree hides
	// everything under it, so the objects below only become visible once the
	// tree itself is back. One pass therefore repairs one layer, and the loop
	// is what reaches the bottom. It ends when a whole pass recovers nothing,
	// which means what is left has no source.
	sources := NewSources(repo, db)
	defer sources.Close()

	// done is every object this run has already put back.
	//
	// It is what makes the loop terminate. An object can still read as damaged
	// after it has been recovered: a corrupt entry in a pack shadows the good
	// loose copy, because the database answers from the pack. Without this the
	// run recovers that object on every pass, forever. Recovering nothing NEW
	// is the real signal that a pass made no progress.
	done := set.New[string]()

	for pass := 0; ; pass++ {
		if pass > 0 {
			db.Close()
			repo, db, err = open(o.Dir)
			if err != nil {
				return nil, err
			}
			damage, err = Scan(repo, db)
			if err != nil {
				return nil, err
			}
			sources.Close()
			sources = NewSources(repo, db)
		}
		if len(damage.Objects) == 0 {
			res.Unrecovered = nil
			break
		}
		recovered := 0
		var stuck []BadObject
		for _, bad := range damage.Objects {
			if done.Contains(bad.OID.String()) {
				// Already put back, and still reading as damaged. Something
				// other than the object itself is wrong -- a pack that will
				// not decode is the usual one -- and recovering it again
				// would not change that.
				stuck = append(stuck, bad)
				continue
			}
			// Read the replacement before touching anything. A source that
			// has nothing leaves the repository exactly as it was.
			found, err := sources.Find(bad)
			if err != nil {
				stuck = append(stuck, bad)
				continue
			}
			// Then displace the corrupt file, and only then write. The
			// replacement lands on the same path, so writing first would
			// overwrite the evidence and leave the quarantine holding the
			// repaired object. If the write fails after this, the manifest
			// still names the original and --undo brings it back.
			for _, path := range bad.Files {
				if err := q.Take(path, "a corrupt object file, replaced"); err != nil {
					return nil, err
				}
			}
			rec, err := sources.Write(bad, found, repo.ObjectsDir)
			if err != nil {
				return nil, fmt.Errorf("writing the recovered %s: %w", bad.OID, err)
			}
			done.Add(bad.OID.String())
			res.Objects = append(res.Objects, rec)
			recovered++
		}
		res.Unrecovered = stuck
		res.RemoteError = sources.RemoteError()
		if recovered == 0 {
			break
		}
	}

	for _, bad := range damage.Refs {
		if !bad.Malformed {
			continue
		}
		fixed, err := repairRef(repo, db, q, bad)
		if err != nil {
			// A ref with no usable reflog cannot be restored without inventing
			// a value, so it is left exactly as it is and reported.
			continue
		}
		res.Refs = append(res.Refs, fixed)
	}

	if len(q.Files()) > 0 {
		res.Quarantine = q.Dir()
	}

	res.Clean, err = verify(o.Dir)
	if err != nil {
		return res, err
	}
	return res, nil
}

// open reads the repository and its object database together, which every pass
// needs to redo: the database remembers the names it could not read, so a pass
// that reuses it cannot see an object the previous pass put back.
func open(dir string) (*gitrepo.Repo, *odb.DB, error) {
	repo, err := gitrepo.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	db, err := odb.Open(repo.ObjectsDir, repo.DisplayObjectsDir, repo.Algo, false)
	if err != nil {
		return nil, nil, err
	}
	return repo, db, nil
}

// plan fills in what a dry run would have done, without doing it.
func plan(damage *Damage, res *Result) *Result {
	res.Derived = append(res.Derived, damage.Derived...)
	res.Unrecovered = append(res.Unrecovered, damage.Objects...)
	sort.Slice(res.Unrecovered, func(i, j int) bool {
		return res.Unrecovered[i].OID.Compare(res.Unrecovered[j].OID) < 0
	})
	return res
}

// verify reports whether the repaired repository is whole.
//
// It runs the whole fsck rather than this package's scan. The scan looks for
// the damage this package knows how to repair, which is a smaller question:
// a corrupt pack, an index that will not parse, or a malformed packed-refs
// would all pass it. Answering "the repository is whole" from the narrower
// check would be a claim the run has not earned, so the broader one decides.
//
// fsck exits non-zero for damage and zero for a repository that merely holds
// dangling or unreachable objects, which is the same line this package draws.
func verify(dir string) (bool, error) {
	o := fsckcmd.DefaultOptions()
	o.Dir = dir
	o.Stdout = io.Discard
	o.Stderr = io.Discard
	// Reflogs are roots here, as they are for git, so an object only the
	// reflog still names does not read as damage.
	return fsckcmd.Run(o) == 0, nil
}

// Report writes what a run did, for a person to read.
func (r *Result) Report(w io.Writer, dryRun bool) {
	verb := "recovered"
	if dryRun {
		verb = "would recover"
	}

	for _, path := range r.Derived {
		if dryRun {
			fmt.Fprintf(w, "would rebuild: %s\n", path)
		} else {
			fmt.Fprintf(w, "rebuilt: %s\n", path)
		}
	}
	for _, rec := range r.Objects {
		fmt.Fprintf(w, "%s: %s %s, from %s\n", verb, rec.Type.Name(), rec.OID, rec.Source)
	}
	for _, ref := range r.Refs {
		fmt.Fprintf(w, "restored: %s -> %s, from %s\n", ref.Name, ref.OID, ref.From)
	}
	if r.Quarantine != "" {
		fmt.Fprintf(w, "\nDisplaced files are in %s.\nNothing was deleted; `git fix --undo` puts them back.\n", r.Quarantine)
	}
	if len(r.Unrecovered) > 0 {
		fmt.Fprintf(w, "\n%d object(s) could not be recovered. Nothing about them was deleted.\n", len(r.Unrecovered))
		for _, b := range r.Unrecovered {
			fmt.Fprintf(w, "  %s\n", b.Describe())
		}
		if r.RemoteError != nil {
			// The remote might well have these. Saying so is the difference
			// between a repository to restore and one to grieve over.
			fmt.Fprintf(w, "\nThe remote was NOT consulted, because reaching it failed:\n  %s\n"+
				"Fix that and run this again before believing anything is lost.\n", r.RemoteError)
			return
		}
		fmt.Fprint(w, "\nThe repository still needs these. A remote, another clone, or a\n"+
			"backup that has them is the only way back.\n")
	}
}
