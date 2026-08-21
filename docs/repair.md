# Repair

`git-fixed` finds damage and undoes it in one run. This document is the contract that repair works under.

## The rule everything else follows from

**No repair may lose data. Ever.**

Not "no repair may lose data that was reachable". Not "no repair may lose more data than was already lost". The repository worked for its owner
before it broke, and it must work, fully, afterwards. An object that cannot be recovered is a failure to report, never a thing to delete so that the
report comes back clean.

This rule is not a preference. It is the reason the tool exists. Git's own maintenance is what breaks these repositories: a `gc` deleted tens of
thousands of referenced objects out of a repository that was working, and recovering it by hand took an afternoon. A repair tool that reaches for
`git gc`, `git prune`, or `git reflog expire` to tidy up after itself is the same accident wearing a different name. This tool runs none of them.

Two properties make lossless repair possible rather than aspirational:

- **An object's name is the hash of its content.** So a candidate recovered from anywhere is either bit-for-bit the original object or it is
  rejected. There is no "close enough" and no judgement call. Every recovery in this document ends with the same check.
- **Nothing is deleted, only moved.** Every removal goes to `.git/git-fixed/quarantine/<run>/`, keeping its path, with a manifest that says where
  each file came from. `git-fixed --undo <run>` puts them all back. A repair that turns out to be wrong costs one command, not a repository.

  An undo restores over what the repair wrote, because most of what a run displaces it also replaces -- a whole index over a broken one, a valid
  `packed-refs` over a malformed one. Nothing is overwritten even so: whatever is in the way moves into that run's own `replaced/` directory first,
  keeping its path. So an undo deletes no more than a repair does, and the two states sit side by side afterwards.

## What counts as damage

Six kinds, and the sixth is the one git users get wrong most often.

### 1. A derived file

The commit-graph, the multi-pack-index, `*.rev`, `*.bitmap`, and `objects/info/packs` are caches. Every byte in them is recomputable from the objects
they describe. A corrupt one is not damage to the repository, it is damage to an index of the repository, and git rebuilds each on demand.

Quarantine it. Nothing else is needed, and nothing is lost even in principle.

`.git/index` is NOT in this set, however much its name suggests otherwise. It carries staged content and stat information that exists nowhere else.
It is treated as real data.

### 2. An object a remote has

The remote's copy is the same object, because the name is the hash. Fetch it and write it in.

The mechanics are less obvious than they look: fetching into the damaged repository does not work. Fetch negotiation is driven by what the repository
claims to have, and a repository with a corrupt object claims to have it -- the file is right there. So the fetch completes, sends nothing, and
changes nothing. Recovery goes through a scratch repository instead: fetch there, copy the objects across, verify each one hashes to the name it was
fetched for.

### 3. An object this repository still has, somewhere

More often than people expect, the content has not left the disk:

- **A second copy.** The same object is frequently both loose and packed, or lives in an alternate object store. If the loose copy is corrupt and the
  packed copy is fine, the packed copy already IS the object. Quarantine the corrupt file and the repository is whole.
- **The worktree.** A missing blob whose path the index still names can be read straight back out of the checked-out file. Hash it; if it matches,
  it is the object.
- **The index.** The index records a path, a mode, and an object name for every tracked file. That is exactly the information a tree object
  serializes. A missing tree can therefore be rebuilt entry by entry, and the rebuild is verified the only way that counts: it must hash to the name
  of the tree that went missing.

None of this involves searching the disk for other clones. There are none, and looking for them is how a repair tool ends up writing a stranger's
objects into your repository.

### 4. An object nothing has

The tool stops. It prints every object it could not recover, which refs and paths need each one, and exits non-zero.

It does not amputate. It does not move a branch back to the last commit that still resolves, it does not drop the broken entry from a tree, and it
does not rewrite history to route around the hole. Each of those produces a repository that passes `fsck`, and each of them is the tool destroying
the thing it was asked to save. "It was already broken, so deleting it cannot make it worse" is false: before, the owner had a damaged repository and
a list of what was missing; after, they have a clean repository and no idea what they lost.

Rewinding a ref is available, but only when the owner asks for it by name, and only after the report has said exactly what it costs.

### 5. A broken ref

Two different faults share this name, and they need different answers.

- **The ref file is malformed** -- empty, truncated, garbage, a symref pointing at nothing -- **but the object it meant to name is fine.** The
  reflog for that ref records every value it has held. Take the newest one whose object is present and complete. This restores the ref to the value
  it had, so nothing is lost, and it is applied automatically.
- **The ref file is fine and names an object that is missing.** This is case 4 wearing a ref's clothes. Recover the object. If it cannot be
  recovered, report it. Do not quietly wind the ref back to where it still resolves.

`packed-refs` and `HEAD` are repaired the same way, from loose refs and from the reflog respectively.

### 6. Dangling and unreachable objects, which are not damage at all

`git fsck` reports these by default and they look alarming. They are not:

- **dangling** means an object nothing points at any more. An amended commit, a dropped stash, the old side of a rebase.
- **unreachable** means the same thing said from the other direction.

Both are ordinary. Every one of them is content the owner made, sitting safely on disk, and every one of them is exactly what `git reflog` and
`git fsck --lost-found` exist to recover from. They are reported for information and never touched. The tool will not prune them, will not count them
as work to do, and will not report a repository containing them as unhealthy.

This is the item worth stating loudest, because the standard advice for a noisy `fsck` is `git gc --prune=now`, and that command's entire job is to
destroy the objects in this category.

## The recovery ladder

Given one object name to recover, the sources are tried in this order:

1. A good copy elsewhere in this repository (packed, loose, or in an alternate).
2. The worktree file the index names for that object.
3. A rebuild from the index, for a tree.
4. A remote.
5. Nothing -- report and fail.

Local sources come before the remote because they cost nothing and cannot fail halfway. Which source answers first never changes the result: the hash
check at the end means every source produces the identical bytes or none at all.

## What a run does, in order

0. Diagnose. `git-fixed` runs a full fsck first and prints git's own findings, so a person sees what was wrong before anything moves.
1. Scan. Read every ref, every pack, every object, and the index. Nothing is written in this phase.

   Most of that work has just been done, and the fsck in step 0 hands over what it found so this does not do it again. See "What the scan takes
   from the fsck" below. Everything else still runs, because git does not look at all of it -- it never verifies `objects/info/packs`, so a stale
   one leaves fsck happy and is still a file to put right. Only the first scan of a run may take anything on trust; every later one follows a change
   this run made, which nobody has checked.
2. Classify each fault into one of the six kinds above.
3. Quarantine the derived caches, which need nothing else.
4. Empty out and displace any packfile that will not verify, then scan again. This comes first among the repairs because a corrupt pack entry hides
   the objects underneath it from every step below.
5. Rewrite `packed-refs` if git's reader refuses it, then scan again. Second for the same reason: a reference nobody can read leads nowhere, so the
   objects it needs read as unreferenced.
6. Recover objects, cheapest source first, going round until a whole pass recovers nothing new.
7. Restore malformed ref files from their reflogs.
8. Rebuild the index, last, because it falls back to the commit `HEAD` names and wants the refs and objects already back.
9. Verify. Run the whole `fsck` against the repaired repository, not this package's narrower scan. A run that does not come back clean says so.
10. Report what was recovered, from where, and what could not be.

Every step appends to the run manifest as it goes, so an interrupted run is still undoable.

`--dry-run` stops after step 2 and prints the plan, including which source would answer for each object. It promises to repair nothing rather than to
write nothing: `--lost-found` is git's own option and saving dangling objects is all it does, so under a dry run it still saves them.

## What the scan takes from the fsck

The scan's two long passes are the ones the fsck has just made: verifying every packfile, and reading every object a reference leads to. Over
229,960 objects they take a scan from 0.7s to 3.2s while finding nothing. Over a hundred million they are twenty minutes and forty-eight.

The fsck hands back three things, and each answers something its exit status cannot.

- **The packs it read end to end**, by path (`Options.PackVerified`). The status word is per run: one corrupt loose object sets `ErrorObject`, which
  says nothing whatever about the packs, and acting on the bit read every pack in the repository a second time to find that out. A pack on this list
  is not read again, and neither is any blob in it -- a pack that verified has decoded every object in it and required each one to hash to the name
  its index gives it, which is the whole question a walk asks of a blob.
- **The objects it could not produce** (`Options.ObjectsDamaged`), with whether the connectivity walk reached each one. A bit does not say what it
  is about: `ErrorObject` is what a loose file that will not decode sets and also what a commit with no author sets, and `ErrorReachable` is what a
  missing object sets and also what a reflog entry naming a pruned object sets. One of each pair is the reason this tool exists and the other is not
  damage at all. An empty list means the walk has nothing to find and does not run.
- **Whether that list is the whole of it.** A fault that no single object name describes -- a link refused on the type it implies, a reflog entry
  naming an object that is gone -- marks the list partial, and a partial list is not counted against.

With a whole list, the walk is an errand rather than a search: it goes and finds the route to each damaged object and stops at the last one. It
reports the first route to each, where a search reports every route. That is the price of not reading a hundred million objects to list the rest.

A run that stops part way hands back nothing. It checked part of the repository, so none of it may be taken on trust.

What it is worth, over 1,241,680 objects on four cores, against the same tool before the fsck handed anything over:

| the repository                  | before        | after         |
|---------------------------------|---------------|---------------|
| whole                           | 3.47s / 544 MB | 3.14s / 500 MB |
| one corrupt loose object nothing points at | 7.96s / 553 MB | 3.30s / 501 MB |
| a corrupt blob at the tip       | 6.83s / 555 MB | 3.09s / 497 MB |

The middle row is the shape that cost the most and deserved the least: one stray file in the object directory sent the scan back through every pack
and then over every object a reference reaches. It now costs what a whole repository costs, because that is all there was to do.

The last row is the errand. The walk read 14 objects of 1,241,684 and stopped, and still reported the route:
`refs/heads/master -> <commit>:tip.txt`.

`Verdict` in `internal/repair/repair.go` holds all of it, and `sameVerdict` in `cmd/git-fixed/fsck.go` decides whether the fsck that produced it
asked the question this asks. A narrower fsck -- `--strict`, `--connectivity-only`, a named object -- answers something else, and its verdict is not
used at all.

## What one pass takes from the pass before it

A repair goes round more than once. A missing tree hides everything under it, so a scan sees only the top of a chain, one pass repairs one layer,
and the layer below only becomes visible once that pass is done. The loop is what reaches the bottom.

Every pass after the first used to begin with a full scan, which handed over nothing at all. On a repository of 104 million objects that is fifteen
to twenty minutes of packs and five minutes of walking, per layer, and a run seen doing four passes spent over an hour of it. Both are now carried
across the passes, for the same reason and by the same rule: hand over the fact, and check it rather than assume it.

- **The packs.** A pass writes loose objects and moves whole packs to quarantine. It never writes into a pack, so a pack still standing is still the
  pack that was read. The scan hands the next scan of the same run its `Damage.Verified`: each pack's path, size and modification time. `rescan`
  compares all three before trusting any of it, so a pack that grew, that was rewritten to the same size, or that is no longer there gets read.
  `trustUnchanged`, `internal/repair/scan.go`.
- **The route back to the damage.** A later pass does not scan at all. `descend` starts the walk under the objects the pass before it put back,
  because that is where the next layer is, and everything else reachable was approved by the walk that already ran. It reads nothing a full scan
  would not have had to read: the earlier walk stopped at the object that was missing, so nothing below it has been looked at yet.
  `internal/repair/walk.go`.

Two things do still scan the whole repository again, and must. Displacing a corrupt pack or rewriting `packed-refs` changes what the repository can
produce and what its references reach, globally -- a line git's reader refuses hides every reference below it -- so each is followed by a `rescan`.
And the run ends with a full `git fsck` of its own, which is the proof that the repair worked and is not something to take on trust from anybody.

An object no source had is carried by hand from pass to pass (`stillBad`), because nothing re-reports it now. Dropping it would leave a failed run
calling itself `Ok`.

## The three container files

An object, a ref file and a cache each hold one thing. A packfile, an index and `packed-refs` each hold many, and that changes what repairing them
means: the fault is in the container, and everything inside it has to survive being taken out of it.

### A corrupt packfile

Every object the pack still yields is written back as a loose object FIRST, and only then does the pack go to quarantine, together with its `.idx`,
`.rev`, `.bitmap`, `.keep`, `.promisor` and `.mtimes`. The order is the whole repair, for two reasons that pull in opposite directions:

- A corrupt entry in a pack **shadows** every loose copy of the object it holds, because the database answers from packs first. So the object keeps
  reading as damaged however many times the recovery ladder puts it back, and nothing below reaches it while the pack is there. That is why a run
  tracks what it has already recovered: without it, the repair loops on the same object forever.
- But removing a pack removes every object in it. So each one has to be a loose object before the pack moves.

Extraction goes through the same check as every other recovery: `Verify` hands over an object only once it has decoded and hashed to its recorded
name. An object that already has a readable loose copy is left alone; a loose file that will NOT read back is quarantined first, because leaving it
would shadow the copy about to be written.

A pack that yields nothing at all is never displaced. Two faults reach that state: an index that will not map, and a pack header that stops the read
before the first entry -- and in the second case every object is still in the file, byte for byte. Moving such a pack would take all of them out of a
repository that has no other copy, and it would buy nothing, because there is no loose copy for it to stop shadowing. It stays exactly where it is,
and the run reports it and fails.

The decision is made from what the extraction actually produced, not from which fault was diagnosed. That distinction is the repair: judging it from
the fault meant a pack with four bad bytes in its signature was displaced after yielding zero objects, and the run only ended well because a remote
happened to have the history.

The cost is disk: a repository whose one pack has a single bad byte comes out with every object loose. That is the price of the objects surviving,
and `git repack` puts them back together whenever the owner wants.

### A `.git/index` that will not parse

The index is not a derived file, whatever its name suggests. It records which paths are staged, at which mode, holding which content, and rebuilding
it from `HEAD` alone would silently unstage everything.

So the damaged file is read as far as it goes, entry by entry, and every one that parses is kept -- the checksum is deliberately not consulted,
because a file with a wrong checksum still holds real entries. `HEAD`'s tree then supplies only the paths the salvage did not reach. The original
goes to quarantine whole, and a rewritten index is version 2, which every git since 1.5 reads.

A salvaged entry keeps the 40 bytes of stat data git recorded for it, so git does not have to read every file in the worktree again to find out
nothing changed. An entry that came from `HEAD` has none, so git re-reads that one file once.

The rewritten file carries entries and no extensions, so the cache-tree and the untracked cache do not survive it. Both are caches: git writes a new
cache-tree on the next commit and a new untracked cache on the next `git status`, and neither records anything the entries do not. The old file is in
quarantine either way, with its extensions in it, byte for byte.

When the old file claimed more entries than it yielded, the report says how many paths are gone -- and says where their content still is. It is not
lost: `git add` writes a blob before the index records it, so staged content is in the object database, unreferenced, and `git fsck --lost-found`
writes it out.

### A malformed `packed-refs`

git's reader stops at the first line it refuses, so one bad line hides every reference below it. Those references are not gone, they are unreadable,
and rewriting the file in valid grammar brings them all back: the trait header, every reference sorted by name, and a recomputed peel line under each
tag.

A line that will not read is the part that needs care, because a dropped line may have been a branch. Each one has its reference name looked up in the
loose refs and then in the reflog, and anything that answers is restored. Anything still unaccounted for is listed in the report and keeps the run
from claiming success -- even when `fsck` comes back clean, which it will, because a reference nobody can see is not damage git recognises. This is
the one place where a repaired repository still reports a failure, and it is deliberate: a rewrite that quietly drops a line the owner cannot see is
exactly how a branch disappears without anybody noticing.

## What the tool never does

- Delete anything. Removal means quarantine.
- Run `gc`, `prune`, `repack`, or `reflog expire`.
- Prune, report, or count an unreachable or dangling object as damage.
- Move a ref backwards without being asked.
- Rewrite a tree or a commit to route around a missing object.
- Look outside the repository for a copy of anything.
