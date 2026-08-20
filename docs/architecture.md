# Architecture

`git-fsck` here is a drop-in replacement for `git fsck`. It reads the same repository, accepts the same options, prints the same lines, and exits with
the same status. What differs is that every expensive phase runs on all cores.

## What "compatible" means, and how it is measured

Two runs agree when they print the same SET of lines and exit with the same status. The set, not the sequence: git's own order falls out of `readdir`
order and of its internal hash table, so it is not reproducible from one machine to the next. `internal/gittest/fsck.go` compares that way. It runs the
system `git fsck` in the same repository, splits both outputs into non-empty lines, sorts them, and requires equality along with the exit code.

`internal/fsckcmd/differential_test.go` and `repos_test.go` hold 41 such comparisons. Each builds a repository that is broken in one specific way -- a
tree entry named `.git`, a duplicate tree entry, a bad committer line, a corrupt loose object, a pack whose CRC no longer matches, a commit-graph with
a wrong parent -- and then requires the two implementations to agree. `internal/gittest/repo.go` writes those repositories directly, because git's own
porcelain refuses to produce most of them.

Our own output IS ordered, deterministically. See `docs/output-ordering.md`.

## Phases

`fsckcmd.Run` follows `builtin/fsck.c` step by step. The reporter is flushed between phases, so a phase's lines are all printed before the next phase
starts.

1. **Objects.** Every loose file and every pack in every object directory, checked in parallel. `--connectivity-only` replaces this with a pass that
   only records which objects exist.
2. **Heads.** The refs named on the command line, or every ref, plus reflogs unless `--no-reflogs`.
3. **Index.** Each worktree's index file, when `--cache` is in effect (it is, when no object was named).
4. **Index files.** The `.rev` reverse indexes and the `.bitmap` files.
5. **Connectivity.** The walk out from the roots, then a verdict on every object the run has heard of.
6. **Graphs.** `commit-graph` and `multi-pack-index`.

The exit status is a bitmask, the same one `builtin/fsck.c` returns: object 1, reachable 2, pack 4, refs 8, commit-graph 16, multi-pack-index 32,
rev-index 64, bitmap 128. A condition git calls `die()` for exits 128 instead, after the run prints what it already found.

## Where the parallelism is

- **A pack** is a delta forest. `Pack.Verify` builds that forest once, then hands each root to a worker, which decodes the root and walks down its
  children reusing the parent's buffer. See `docs/pack-verification.md`.
- **Loose objects** are one flat list of files, split across workers.
- **The connectivity walk** draws from one shared stack. A level-at-a-time walk would be simpler, but history is long and narrow: most levels hold one
  commit, so three of four workers would idle and every commit would cost a barrier.
- **The object checks themselves** run inside the worker that decoded the object. This is the whole point: the check is most of the work, so running it
  under a lock would leave one core doing everything while the others decode ahead of it.

## The edge cache

git parses each object once and keeps the parsed object, so its connectivity walk re-reads nothing. Keeping whole parsed objects for a repository of
half a million objects is expensive, so this implementation keeps only what the walk actually needs: for each object, the list of objects it points at,
already resolved to table entries. That is `objEntry.edges`, three words per reference and no strings.

Two cases fall outside it and re-read the object. `--connectivity-only` never ran an object pass, so there is nothing to cache. `--name-objects` builds
each object's name from the path the walk took to reach it, which a recorded edge cannot carry.

## The delta base cache

A read of one packed object has to reconstruct every object under it in the delta chain. Without a cache, an object at depth ten costs ten inflations,
and its neighbour costs ten more even though nine of them were the same objects. `internal/odb/cache.go` is an LRU of reconstructed bases, sized to
git's own `core.deltaBaseCacheLimit` default of 96 MiB. Only a base goes in it, so the buffer a caller of `DB.Read` gets back is always its own.

This is what the object pass avoids entirely by walking the delta forest instead, and what `--connectivity-only` depends on, since that mode reads
objects in reachability order rather than pack order.

## Measured

On a synthetic repository of 406,500 objects, against git 2.43.0. `scripts/bench.sh` produces these numbers, and refuses to print a time unless the two
implementations agreed on the output first.

| run                        | git   | git-fixed | git-fixed, one worker |
|----------------------------|-------|-----------|-----------------------|
| `fsck`                     | 2.43s | 1.52s     | 2.84s                 |
| `fsck --connectivity-only` | 0.89s | 1.66s     | 3.39s                 |
| `fsck --no-full`           | 0.03s | 0.13s     |                       |

`--connectivity-only` is the one mode that is still slower than git. It has no object pass, so it has no edge cache to walk and pays a full object read
per node; git's advantage there is that its parsed objects are already in memory from the mark pass. `--no-full` is far too short to say anything.

## Known divergence: zlib error detail

git's decompressor prints zlib's own complaint before its caller adds one, so a corrupt loose object can produce a line like
`error: inflate: data stream error (invalid block type)`. Go's `compress/flate` collapses every one of those cases into a single corrupt-input error
with a byte offset, so that line is missing for every failure except a bad zlib header, whose wording we do match. Every other line, and the exit
status, still agree.

Closing it means writing a DEFLATE decoder that carries zlib's error taxonomy -- roughly fifteen distinct messages -- and keeping it as fast as the
standard library's. Guessing at the message from the offset would print the wrong reason, which is worse than printing none.

## Package layout

| package             | what it holds                                                                             |
|---------------------|-------------------------------------------------------------------------------------------|
| `cmd/git-fsck`      | the command: option table, defaults, exit status                                            |
| `internal/parseopt` | git's parse-options behaviour: `--no-` forms, unique-prefix abbreviation, git's usage text   |
| `internal/gitobj`   | object names and object types                                                               |
| `internal/gitrepo`  | the repository: config, refs, reflogs, index, worktrees                                     |
| `internal/odb`      | the object database: loose files, packs, alternates, delta decoding, pack verification      |
| `internal/fsck`     | the checks themselves, matching `fsck.c`: trees, commits, tags, blobs, the severity table   |
| `internal/gitpath`  | whether a tree entry name can reach `.git` on some filesystem                               |
| `internal/fsckcmd`  | the phases, the object table, the connectivity walk, the reporter                           |
| `internal/gittest`  | test repositories, including deliberately broken ones, and the comparison against real git  |

`internal/parseopt` exists instead of cobra because a drop-in replacement has to accept exactly what git accepts and refuse exactly what git refuses.
git abbreviates any unambiguous long option, spells the negative form `--no-x`, and prints a usage block cobra cannot reproduce. The org convention is
cobra for a CLI whose shape we choose; this CLI's shape is already fixed by the tool it replaces.
