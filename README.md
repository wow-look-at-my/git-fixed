# git-fixed

Drop-in replacements for git's own commands, written in Go, that use every core instead of one. First one: `git fsck`.

## git-fsck

Same options, same output, same exit status as `git fsck`, and about 1.6x faster on a repository of 406,500 objects.

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

### One known difference

For a corrupt loose object, git prints an extra line naming zlib's own complaint, such as `inflate: data stream error (invalid block type)`. Go's
decompressor does not distinguish those cases, so that line is missing except for a bad zlib header, where the wording does match. Every other line,
and the exit status, agree. [docs/architecture.md](docs/architecture.md) says what closing it would take.

### Is it really the same?

64 differential tests build a deliberately broken repository, run the system `git fsck` and this one over it, and require the same lines and the same
exit status. `scripts/bench.sh` refuses to report a time unless the two agreed first.

## Documentation

- [docs/architecture.md](docs/architecture.md) -- the phases, where the parallelism is, what is measured
- [docs/alias-detection.md](docs/alias-detection.md) -- names that reach `.git` on four filesystems
- [docs/pack-verification.md](docs/pack-verification.md) -- the delta forest, and decoding each object once
- [docs/output-ordering.md](docs/output-ordering.md) -- why output is sorted, and what "same as git" means
- [docs/commit-graph.md](docs/commit-graph.md) -- commit-graph checks
- [docs/multi-pack-index.md](docs/multi-pack-index.md) -- multi-pack-index checks
- [docs/ref-consistency.md](docs/ref-consistency.md) -- the ref database check
