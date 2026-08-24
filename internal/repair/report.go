package repair

// What a run did, written out for a person to read.

import (
	"fmt"
	"io"
)

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
			// Their bytes came out of a pack this run displaced, and that pack is in the quarantine directory whole.
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
		// The pack lines above say "would empty out" and cannot say how much comes out.
		fmt.Fprint(w, "A packfile's own count is not in those totals: how many objects it still\n"+
			"yields is only known once they are written out, which a --dry-run does not do.\n")
	}
	if len(r.Unrecovered) == 0 && len(r.Packs) == 0 {
		fmt.Fprint(w, "Run without --dry-run to make these repairs.\n")
	}
}
