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
	// Healthy is what an fsck the caller already ran said, or nil when it ran
	// none. The command runs one to report its findings, so on a repository
	// with nothing wrong the answer is already known, and reading every
	// object a second time to hear it again is half the run for nothing.
	//
	// It stands in only for the question verify asks. A stricter fsck, a
	// narrower one, or one given objects to check answers a different
	// question, so a caller that ran one of those passes nil.
	Healthy *bool

	// ShowProgress draws a meter over the two passes a scan spends its time
	// in. A scan of a broken repository reads every pack and then every
	// object a reference leads to, which is as long as the fsck before it.
	ShowProgress bool

	Stdout io.Writer
	Stderr io.Writer
}

// meters is where a scan started by this run draws its progress.
func (o *Options) meters() Meters {
	return Meters{Stderr: o.Stderr, Show: o.ShowProgress}
}

// Result is what a run did.
type Result struct {
	// Derived are the rebuildable caches that were displaced.
	Derived []string
	// Objects are the objects put back, and where each came from.
	Objects []Recovered
	// Refs are the references restored.
	Refs []RepairedRef
	// Packs are the packfiles that were emptied out and displaced.
	Packs []RescuedPack
	// Index is the index that was rebuilt, when one was.
	Index *RepairedIndex
	// PackedRefs is the packed-refs rewrite, when there was one.
	PackedRefs *RepairedPackedRefs
	// Refused are the faults that could not be repaired without risking the
	// data behind them. Each one is left exactly as it was found.
	Refused []string
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
func (r *Result) Ok() bool {
	return len(r.Unrecovered) == 0 && len(r.Refused) == 0 && r.droppedRefs() == 0 && r.Clean
}

// droppedRefs counts the packed-refs lines that named nothing recoverable.
func (r *Result) droppedRefs() int {
	if r.PackedRefs == nil {
		return 0
	}
	return len(r.PackedRefs.Dropped) - len(r.PackedRefs.Restored)
}

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
		len(r.Refs) == 0 && len(r.Unrecovered) == 0 &&
		len(r.Packs) == 0 && len(r.Refused) == 0 &&
		r.Index == nil && r.PackedRefs == nil
}

// firstScan reads the repository, skipping what the caller's own fsck has
// already covered. Only the first scan of a run may do that: every later one
// follows a change this run made, which nobody has checked.
func (o *Options) firstScan(repo *gitrepo.Repo, db *odb.DB) (*Damage, error) {
	if o.Healthy != nil && *o.Healthy {
		return ScanTrustingFsck(repo, db)
	}
	return Scan(repo, db, o.meters())
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

	damage, err := o.firstScan(repo, db)
	if err != nil {
		return nil, err
	}

	res := &Result{}
	if damage.Empty() {
		// Ask fsck even here. The scan finds the damage this package can
		// repair; fsck finds the rest. Saying "nothing to repair" off the
		// narrower check would tell someone their repository is fine when
		// git still refuses to use it.
		if o.Healthy != nil {
			res.Clean = *o.Healthy
			return res, nil
		}
		res.Clean, err = verify(o.Dir)
		return res, err
	}
	if o.DryRun {
		return plan(repo, db, damage, res), nil
	}

	q := NewQuarantine(repo.CommonDir, o.Run)
	for _, path := range damage.Derived {
		if err := q.Take(path, "a rebuildable cache that would not parse"); err != nil {
			return nil, err
		}
		res.Derived = append(res.Derived, repo.Shown(path))
	}

	// Packs come before objects, and before anything reopens the database.
	// A corrupt entry in a pack shadows every loose copy of the objects it
	// holds, so no amount of recovery below reaches them while the pack is
	// still in place.
	if len(damage.Packs) > 0 {
		for _, bad := range damage.Packs {
			rescued, err := rescuePack(repo, q, bad)
			if err != nil {
				res.Refused = append(res.Refused, err.Error())
				continue
			}
			res.Packs = append(res.Packs, rescued)
		}
		// The database still holds the pack this run just moved away, so
		// every later step works from a fresh one.
		db.Close()
		repo, db, err = open(o.Dir)
		if err != nil {
			return nil, err
		}
		damage, err = Scan(repo, db, o.meters())
		if err != nil {
			return nil, err
		}
	}

	if damage.PackedRefs != nil {
		// packed-refs comes before the refs and the objects below, because
		// git's reader stops at the first line it refuses -- so until this
		// is rewritten, every reference under that line is invisible and the
		// objects they lead to look unreferenced.
		fixed, err := repairPackedRefs(repo, db, q, damage.PackedRefs)
		if err != nil {
			return nil, err
		}
		res.PackedRefs = &fixed
		repo, db, err = reopen(o.Dir, db)
		if err != nil {
			return nil, err
		}
		damage, err = Scan(repo, db, o.meters())
		if err != nil {
			return nil, err
		}
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
			damage, err = Scan(repo, db, o.meters())
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

	// The index last. It falls back to the commit HEAD names for the paths
	// its own file no longer yields, so it wants a repository whose refs and
	// objects are already back.
	if damage.Index != nil {
		repo, db, err = reopen(o.Dir, db)
		if err != nil {
			return nil, err
		}
		fixed, err := repairIndex(repo, db, q, damage.Index)
		if err != nil {
			return nil, err
		}
		res.Index = &fixed
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

// reopen closes one database and opens the repository again.
func reopen(dir string, db *odb.DB) (*gitrepo.Repo, *odb.DB, error) {
	db.Close()
	return open(dir)
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
func plan(repo *gitrepo.Repo, db *odb.DB, damage *Damage, res *Result) *Result {
	for _, path := range damage.Derived {
		res.Derived = append(res.Derived, repo.Shown(path))
	}
	// Every damaged object is put to the recovery ladder, which reads and
	// writes nothing: the remote it may consult is fetched into a scratch
	// repository of its own. Listing them all as unrecoverable instead --
	// which is what this did -- tells someone their objects are gone while
	// the repair that follows would have put most of them back.
	if len(damage.Objects) > 0 {
		src := NewSources(repo, db)
		defer src.Close()
		for _, b := range damage.Objects {
			f, err := src.Find(b)
			if err != nil {
				res.Unrecovered = append(res.Unrecovered, b)
				continue
			}
			res.Objects = append(res.Objects, Recovered{OID: b.OID, Type: f.Type, Source: f.Source})
		}
		res.RemoteError = src.RemoteError()
	}
	for _, bad := range damage.Packs {
		// A dry run does not extract, so it cannot say yet whether the pack
		// will yield anything -- and that is what decides whether it moves.
		res.Packs = append(res.Packs, RescuedPack{Pack: repo.Shown(bad.Pack)})
	}
	if damage.Index != nil {
		res.Index = &RepairedIndex{Path: repo.Shown(damage.Index.Path), Why: damage.Index.Why}
	}
	if damage.PackedRefs != nil {
		res.PackedRefs = &RepairedPackedRefs{Why: damage.PackedRefs.Why}
	}
	sort.Slice(res.Objects, func(i, j int) bool {
		return res.Objects[i].OID.Compare(res.Objects[j].OID) < 0
	})
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
	for _, p := range r.Packs {
		if dryRun {
			fmt.Fprintf(w, "would empty out: %s\n", p.Pack)
			continue
		}
		fmt.Fprintf(w, "emptied out: %s, %d object(s) written back as loose, %d already elsewhere\n",
			p.Pack, p.Extracted, p.Present)
		if p.Lost > 0 {
			fmt.Fprintf(w, "  %d entr(ies) in it would not decode, and are recovered below or reported\n", p.Lost)
		}
	}
	if pr := r.PackedRefs; pr != nil {
		if dryRun {
			fmt.Fprintf(w, "would rewrite: packed-refs (%s)\n", pr.Why)
		} else {
			fmt.Fprintf(w, "rewrote: packed-refs, %d reference(s) kept, %d restored\n", pr.Kept, len(pr.Restored))
			for _, ref := range pr.Restored {
				fmt.Fprintf(w, "  %s -> %s, from %s\n", ref.Name, ref.OID, ref.From)
			}
		}
	}
	if idx := r.Index; idx != nil {
		if dryRun {
			fmt.Fprintf(w, "would rebuild: %s (%s)\n", idx.Path, idx.Why)
		} else {
			fmt.Fprintf(w, "rebuilt: %s, %d entr(ies) salvaged, %d from HEAD\n",
				idx.Path, idx.Salvaged, idx.FromHead)
		}
	}
	for _, rec := range r.Objects {
		fmt.Fprintf(w, "%s: %s %s, from %s\n", verb, rec.Type.Name(), rec.OID, rec.Source)
	}
	for _, ref := range r.Refs {
		fmt.Fprintf(w, "restored: %s -> %s, from %s\n", ref.Name, ref.OID, ref.From)
	}
	if r.Quarantine != "" {
		fmt.Fprintf(w, "\nDisplaced files are in %s.\nNothing was deleted; `git-fixed --undo` puts them back.\n", r.Quarantine)
	}
	r.reportPartialRepairs(w)
	if len(r.Refused) > 0 {
		fmt.Fprintf(w, "\n%d fault(s) were left alone, because repairing them would have risked\n"+
			"the data behind them. Nothing about them was touched:\n", len(r.Refused))
		for _, why := range r.Refused {
			fmt.Fprintf(w, "  %s\n", why)
		}
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
		// Only once there IS a quarantine directory. A dry run displaces
		// nothing, so pointing at one would be pointing at nothing.
		if len(r.Packs) > 0 && r.Quarantine != "" {
			// Their bytes came out of a pack this run displaced, and that pack
			// is in the quarantine directory whole. Ending on "a backup is the
			// only way back" would send someone hunting for a copy of the
			// repository while the damaged original sat one command away.
			fmt.Fprint(w, "\nSome of these were in a packfile this run took out. That pack is in the\n"+
				"quarantine directory above, byte for byte, and `git-fixed --undo` puts it back.\n"+
				"It is the only copy of those bytes, so keep it until they are recovered.\n")
		}
		fmt.Fprint(w, "\nThe repository still needs these. A remote, another clone, or a\n"+
			"backup that has them is the only way back.\n")
	}
}

// reportPartialRepairs says what a repair could not put back.
//
// A rewritten packed-refs and a rebuilt index both leave a repository git will
// use again, which is exactly the state in which a quiet report is dangerous:
// fsck comes back clean and the owner has no way to know that a reference or a
// staged path did not survive. Every one is named here.
func (r *Result) reportPartialRepairs(w io.Writer) {
	if pr := r.PackedRefs; pr != nil && len(pr.Dropped) > len(pr.Restored) {
		fmt.Fprintf(w, "\n%d line(s) of the old packed-refs named no reference this repository\n"+
			"still knows. If one of them was a branch, it is not in the rewritten file:\n", len(pr.Dropped)-len(pr.Restored))
		for _, line := range pr.Dropped {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprint(w, "The original is in the quarantine directory above, byte for byte.\n")
	}
	idx := r.Index
	if idx == nil || idx.StagedWorkLost() == 0 {
		return
	}
	fmt.Fprintf(w, "\nThe old index claimed %d entries and yielded %d. %d staged path(s) are\n"+
		"not in the rebuilt index.\n", idx.Claimed, idx.Salvaged, idx.StagedWorkLost())
	fmt.Fprint(w, "Their CONTENT is not lost: git add writes a blob before the index records\n"+
		"it, so it is in the object database, unreferenced. `git fsck --lost-found`\n"+
		"writes those out. The original index is in the quarantine directory above.\n")
}

// ReportPlanTotals closes a --dry-run by accounting for everything the scan
// found: what a repair would put right, and what it would leave.
//
// The lines above name each one, and a person reading a long list of them
// needs to know whether the list adds up. A plan that ends without saying so
// leaves the one question it was run to answer -- can this be repaired --
// for the reader to work out by counting.
func (r *Result) ReportPlanTotals(w io.Writer) {
	would := len(r.Derived) + len(r.Objects) + len(r.Refs) + len(r.Packs)
	if r.Index != nil {
		would++
	}
	if r.PackedRefs != nil {
		would++
	}
	fmt.Fprintf(w, "\n%d fault(s) would be repaired, %d would not.\n", would, len(r.Unrecovered))
	if len(r.Packs) > 0 {
		// The pack lines above say "would empty out" and cannot say how much
		// comes out, because that needs the extraction itself. Saying which
		// number is a plan and which is a measurement is the difference
		// between a report and a guess.
		fmt.Fprint(w, "A packfile's own count is not in those totals: how many objects it still\n"+
			"yields is only known once they are written out, which a --dry-run does not do.\n")
	}
	if len(r.Unrecovered) == 0 && len(r.Packs) == 0 {
		fmt.Fprint(w, "Run without --dry-run to make these repairs.\n")
	}
}
