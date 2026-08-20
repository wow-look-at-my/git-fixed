package repair

// The run: scan, recover, apply, verify.

import (
	"fmt"
	"io"
	"sort"

	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
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
func (r *Result) Nothing() bool {
	return r.Clean && len(r.Objects) == 0 && len(r.Derived) == 0 &&
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
		res.Clean = true
		return res, nil
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

// verify re-reads the repository from scratch and reports whether it is whole.
//
// It opens the repository again rather than reusing the database from the
// repair, because that one has cached which names it could not read and would
// answer from that cache.
func verify(dir string) (bool, error) {
	repo, db, err := open(dir)
	if err != nil {
		return false, err
	}
	defer db.Close()
	damage, err := Scan(repo, db)
	if err != nil {
		return false, err
	}
	return damage.Empty(), nil
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
