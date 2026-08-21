# git-fixed

One tool for repositories git has broken, written in Go. It finds the damage on every core, the way `git fsck` does, and then repairs it without
losing anything.

```
$ git-fixed              # say what is wrong, then repair it
$ git-fixed --dry-run    # say what is wrong, change nothing
$ git-fixed --undo       # put the last run's displaced files back
```

Run it in a repository, or point it at one with `-C <directory>`. `GIT_FIXED_THREADS=N` pins the worker count; the default is one per core.

## Repairing

It recovers every object it can, from the repository itself before the network, and it never deletes: a file it has to displace goes to a quarantine
directory that `--undo` empties back.

An object no source has is reported, and the run fails. It is not amputated, no branch is wound back to route around it, and no history is rewritten.
Dangling and unreachable objects are left alone: those are ordinary, and pruning them is how repositories lose work in the first place.

Recovery sources, cheapest first: another copy already in the repository, the worktree file the index names, a tree rebuilt from the index, then a
remote. Every source ends at the same check -- content that does not hash to the name being recovered is refused -- so a recovery is the original
object or it does not happen.

A packfile, a `.git/index` and a `packed-refs` each hold many things at once, so each is emptied before it is displaced. A corrupt pack has every
object it still yields written back as loose objects first, because a corrupt entry shadows the good copy underneath it. An index is read entry by
entry and keeps everything staged that still parses. `packed-refs` is rewritten from the lines that read, and a line that does not read is looked up
in the reflog and then reported rather than dropped. See `docs/repair.md`.

## Finding the damage

The first half of a run is a full fsck: the same options, the same output and the same exit status as `git fsck`, with the work spread across every
core instead of one. `--dry-run` stops there, so it is the drop-in -- on a healthy repository its output and its exit status are git's, to the
character. On a damaged one it adds what it would repair, below git's findings.

`--dry-run` promises to repair nothing, not to write nothing: `--lost-found` is git's own option and saving dangling objects is the whole of what it
does, so it still writes them.

**Read the speedup with the core count next to it.** On a four-core machine, over 229,960 objects, it is 1.93x git 2.55.0 -- and four cores is all the
speedup four cores can buy. Reproduce it yourself: `scripts/make-bench-repo.sh <dir>` builds the repository and `scripts/bench.sh <dir>` times both
tools, refusing to print a number unless their output matched.

| workers | seconds | vs git |
|---|---|---|
| 1 | 1.305 | 0.97x |
| 2 | 0.948 | 1.33x |
| 4 | 0.655 | 1.93x |

The honest limit is in that table: four workers give 1.99x of one, not 4x, so about two thirds of the run is parallel today. `docs/architecture.md`
says what the rest is and what has already been taken off it.

Repairing costs nothing on a healthy repository. The scan skips the two passes the fsck above it has already made, so a run that finds nothing takes
what the fsck alone takes.

### What it checks

Everything git checks: loose objects and packs, tree entry names and modes, commit and tag headers, `.gitmodules` and `.gitattributes` content, refs,
reflogs, every worktree's index, reachability, `.rev` reverse indexes, `.bitmap` files, the commit-graph, and the multi-pack-index. `fsck.<msgid>`
severity settings and `fsck.skipList` are honoured. It takes every option `git fsck` does, including the `--no-` forms and any unambiguous
abbreviation:

```
$ git-fixed --dry-run --strict --dangling
$ git-fixed --dry-run --connectivity-only
```

The reference database is checked first, the way `git refs verify` does it: the file type, the name, the content and its trailing bytes, symref
targets, and the whole `packed-refs` grammar. `--no-references` turns that phase off. See [docs/ref-consistency.md](docs/ref-consistency.md).

One check goes further than git's. A tree entry whose name would reach `.git` on ext4 with casefolding, or on ZFS with normalization, is reported here
and not by git, which only knows HFS+ and NTFS. It is on by default and there is no way to turn it off. See
[docs/alias-detection.md](docs/alias-detection.md).

### Is it really the same?

52 differential test functions, most of them table-driven over several repositories each, build a deliberately broken repository, run the system
`git fsck` and this one over it, and require the same lines and the same exit status. Two of them corrupt a repository one byte at a time: 328 loose
objects in one repository, and a packfile at twelve points. `scripts/bench.sh` refuses to report a time unless the two agreed first.

That includes the line git's decompressor prints for itself, such as `inflate: data stream error (invalid block type)`. Go's decompressor reports
every one of those cases as the same error, so the reason is worked out separately, by an inflate that runs only after a read has failed. See
[docs/zlib-messages.md](docs/zlib-messages.md).

## Build

```
go-toolchain          # builds, vets, tests, and writes build/git-fixed
```

## Documentation

- [docs/repair.md](docs/repair.md) -- the damage kinds, the recovery ladder, why nothing is ever deleted
- [docs/architecture.md](docs/architecture.md) -- the phases, where the parallelism is, what is measured
- [docs/alias-detection.md](docs/alias-detection.md) -- names that reach `.git` on four filesystems
- [docs/pack-verification.md](docs/pack-verification.md) -- the delta forest, and decoding each object once
- [docs/output-ordering.md](docs/output-ordering.md) -- why output is sorted, and what "same as git" means
- [docs/commit-graph.md](docs/commit-graph.md) -- commit-graph checks
- [docs/multi-pack-index.md](docs/multi-pack-index.md) -- multi-pack-index checks
- [docs/ref-consistency.md](docs/ref-consistency.md) -- the ref database check
- [docs/zlib-messages.md](docs/zlib-messages.md) -- reproducing zlib's own complaint
- [docs/memory.md](docs/memory.md) -- the memory and swap high-water marks, and why resident is mostly packfile
