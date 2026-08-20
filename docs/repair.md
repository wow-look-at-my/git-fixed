# Repair

`git fsck` finds damage. `git fix` undoes it. This document is the contract that repair works under.

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
  each file came from. `git fix --undo <run>` puts them all back. A repair that turns out to be wrong costs one command, not a repository.

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

1. Scan. Read every ref, every object, and the index. Nothing is written in this phase.
2. Classify each fault into one of the six kinds above.
3. Recover objects, cheapest source first.
4. Apply. Derived files are quarantined, recovered objects are written, repaired refs are updated, and every step is appended to the run manifest.
5. Verify. Re-run the whole scan against the repaired repository. A run that does not come back clean says so.
6. Report what was recovered, from where, and what could not be.

`--dry-run` stops after step 3 and prints the plan, including which source would answer for each object.

## What it does not repair yet

Naming these matters as much as the list above, because a run that finds nothing must not be read as a clean bill of health. So the verification step
runs the whole `fsck`, not this package's scan, and a repository `fsck` still refuses is reported that way even when the repair had nothing to do.

- **A corrupt packfile.** Objects it holds are recovered as loose copies where a source has them, but the pack itself stays broken and `fsck` keeps
  complaining. Worse, a corrupt pack entry SHADOWS the good loose copy, because the object database answers from the pack -- so those objects read as
  damaged however many times they are put back. That is why a run tracks what it has already recovered: without it, the repair loops forever on the
  same object. The fix is to rewrite the pack from what can be read, and it is not written.
- **A `.git/index` that will not parse.** The index is not derived: it holds staged content and stat information that exists nowhere else, so it is
  not safe to displace and rebuild. Reported, not touched.
- **A malformed `packed-refs`.** The loose refs and the reflogs together hold enough to rebuild it, and that is not written either.

## What the tool never does

- Delete anything. Removal means quarantine.
- Run `gc`, `prune`, `repack`, or `reflog expire`.
- Prune, report, or count an unreachable or dangling object as damage.
- Move a ref backwards without being asked.
- Rewrite a tree or a commit to route around a missing object.
- Look outside the repository for a copy of anything.
