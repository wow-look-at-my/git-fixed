# git-fixed

Tools for repositories git has broken, written in Go. `git-fsck` finds the damage on every core; `git-fix` undoes it without losing anything.

## git-fix

Repairs a damaged repository. It recovers every object it can, from the repository itself before the network, and it never deletes: a file it has to
displace goes to a quarantine directory that `--undo` empties back.

```
$ git-fix              # repair this repository
$ git-fix --dry-run    # say what is wrong, change nothing
$ git-fix --undo       # put the last run's displaced files back
```

An object no source has is reported, and the run fails. It is not amputated, no branch is wound back to route around it, and no history is rewritten.
Dangling and unreachable objects are left alone: those are ordinary, and pruning them is how repositories lose work in the first place.

Recovery sources, cheapest first: another copy already in the repository, the worktree file the index names, a tree rebuilt from the index, then a
remote. Every source ends at the same check -- content that does not hash to the name being recovered is refused -- so a recovery is the original
object or it does not happen. See `docs/repair.md`.

## git-fsck

Same options, same output, same exit status as `git fsck`, and about 1.5x faster than git 2.55.0 on a repository of 229,960 objects.

```
$ git-fsck
$ git-fsck --strict --dangling
$ git-fsck --connectivity-only
```

Run it in a repository, exactly as you would run `git fsck`. It takes every option git's does, including the `--no-` forms and any unambiguous
abbreviation. `GIT_FIXED_THREADS=N` pins the worker count; the default is one per core.

### Build

```
go-toolchain          # builds, vets, tests, and writes build/git-fsck
```

### What it checks

Everything git checks: loose objects and packs, tree entry names and modes, commit and tag headers, `.gitmodules` and `.gitattributes` content, refs,
reflogs, every worktree's index, reachability, `.rev` reverse indexes, `.bitmap` files, the commit-graph, and the multi-pack-index. `fsck.<msgid>`
severity settings and `fsck.skipList` are honoured.

The reference database is checked first, the way `git refs verify` does it: the file type, the name, the content and its trailing bytes, symref
targets, and the whole `packed-refs` grammar. `--no-references` turns that phase off. See [docs/ref-consistency.md](docs/ref-consistency.md).

One check goes further than git's. A tree entry whose name would reach `.git` on ext4 with casefolding, or on ZFS with normalization, is reported here
and not by git, which only knows HFS+ and NTFS. It is on by default and there is no way to turn it off. See
[docs/alias-detection.md](docs/alias-detection.md).

### Is it really the same?

77 differential tests build a deliberately broken repository, run the system `git fsck` and this one over it, and require the same lines and the same
exit status. Two of them corrupt a repository one byte at a time: 328 loose objects in one repository, and a packfile at twelve points.
`scripts/bench.sh` refuses to report a time unless the two agreed first.

That includes the line git's decompressor prints for itself, such as `inflate: data stream error (invalid block type)`. Go's decompressor reports
every one of those cases as the same error, so the reason is worked out separately, by an inflate that runs only after a read has failed. See
[docs/zlib-messages.md](docs/zlib-messages.md).

## Documentation

- [docs/architecture.md](docs/architecture.md) -- the phases, where the parallelism is, what is measured
- [docs/alias-detection.md](docs/alias-detection.md) -- names that reach `.git` on four filesystems
- [docs/pack-verification.md](docs/pack-verification.md) -- the delta forest, and decoding each object once
- [docs/output-ordering.md](docs/output-ordering.md) -- why output is sorted, and what "same as git" means
- [docs/commit-graph.md](docs/commit-graph.md) -- commit-graph checks
- [docs/multi-pack-index.md](docs/multi-pack-index.md) -- multi-pack-index checks
- [docs/ref-consistency.md](docs/ref-consistency.md) -- the ref database check
- [docs/zlib-messages.md](docs/zlib-messages.md) -- reproducing zlib's own complaint
