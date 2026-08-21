# Exit status

`--dry-run` gives back a bitmask of what it found, the same one `builtin/fsck.c` returns:

| bit | meaning |
| --- | --- |
| 1 | an object is corrupt, missing, or will not parse |
| 2 | something a reference reaches is not there |
| 4 | a packfile will not verify |
| 8 | a reference is wrong |
| 16 | the commit-graph is wrong |
| 32 | the multi-pack-index is wrong |
| 64 | a reverse index is wrong |
| 128 | a bitmap is wrong |
| 256 | the index will not parse -- this implementation's own, see below |

Zero means the repository is sound. Without `--dry-run` the status is about the repair instead: zero when the repository ended whole, 1 when something
was left broken.

## Where git dies, and where this does not

git answers a condition it will not continue past by calling `die()`, which exits 128. That number says one thing: git gave up. It says nothing about
the repository, and everything git had not looked at yet stays unlooked-at.

For an unreadable index that is pure loss. `fsck_index()` reads the index and nothing else does: not the reverse-index checks, not the bitmap checks,
not the connectivity walk, not the commit-graph or multi-pack-index checks. All four of those run after it, and git skips all four because a file
none of them opens would not parse. A person whose index is eight bytes long learns that their index is eight bytes long and nothing whatever about
their objects.

So the index is a finding here. The message is git's own, word for word, and it still names the index the way git names it. The run then checks
everything else and the status carries bit 256.

That bit is not in git's vocabulary, and it does not need to be: git has no bit for this because git never reaches the end of a run that hits it.

## Where 128 stays

A condition the rest of the run genuinely depends on still ends it, and still exits 128, because carrying on would produce a report that is worse
than no report. `packed-refs` that will not parse is the clearest case: every later phase starts from the references, so a run that continued past it
would walk from nothing and call the whole repository unreachable. `fsckcmd.Run` names each of them, and each is a `die()` in `builtin/fsck.c` too.

The test for whether a `die()` should stay is not whether git does it. It is whether anything after it reads what failed.

## The differential tests

`internal/gittest/fsck.go` compares this implementation's output and status against the real `git fsck`. An unreadable index is one of the two places
they deliberately disagree, so `TestUnreadableIndex` asserts both sides by hand rather than requiring them to match: git's 128, and this
implementation's 256 with the later phases run. The other is `docs/allocation-bounds.md`.
