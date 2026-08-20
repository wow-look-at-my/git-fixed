# Architecture

`git-fixed --dry-run` is a drop-in replacement for `git fsck`. It reads the same repository, accepts the same options, prints the same lines, and exits
with the same status. What differs is that every expensive phase runs on all cores. Without `--dry-run` the same run goes on to repair what it found.

## What "compatible" means, and how it is measured

Two runs agree when they print the same SET of lines and exit with the same status. The set, not the sequence: git's own order falls out of `readdir`
order and of its internal hash table, so it is not reproducible from one machine to the next. `internal/gittest/fsck.go` compares that way. It runs
the system `git fsck` in the same repository, splits both outputs into non-empty lines, sorts them, and requires equality along with the exit code.

`internal/fsckcmd/differential_test.go`, `repos_test.go`, `refs_test.go` and `corrupt_test.go` hold 77 such comparisons. Each builds a repository
that is broken in one specific way -- a tree entry named `.git`, a duplicate tree entry, a bad committer line, a corrupt loose object, a pack whose
CRC no longer matches, a commit-graph with a wrong parent, a `packed-refs` line with no newline -- and then requires the two implementations to
agree.
`internal/gittest/repo.go` writes those repositories directly, because git's own porcelain refuses to produce most of them.

`gittest.RequireGit` fails the test when the system git is older than `gittest.MinGit`, rather than skipping it: git rewords its messages between
releases, so an older git compares this implementation against a different specification. CI installs git from `ppa:git-core/ppa` for that reason.

Our own output IS ordered, deterministically. See `docs/output-ordering.md`.

## Phases

`fsckcmd.Run` follows `builtin/fsck.c` step by step. The reporter is flushed between phases, so a phase's lines are all printed before the next phase
starts.

1. **References.** The reference files themselves, before any object is read: file type, name, content, symref targets, and the `packed-refs`
   grammar. git 2.51 folded this in from `git refs verify`. `--no-references` turns it off. See `docs/ref-consistency.md`.
2. **Objects.** Every loose file and every pack in every object directory, checked in parallel. `--connectivity-only` replaces this with a pass that
   only records which objects exist.
3. **Heads.** The refs named on the command line, or every ref, plus reflogs unless `--no-reflogs`.
4. **Index.** Each worktree's index file, when `--cache` is in effect (it is, when no object was named).
5. **Index files.** The `.rev` reverse indexes and the `.bitmap` files.
6. **Connectivity.** The walk out from the roots, then a verdict on every object the run has heard of.
7. **Graphs.** `commit-graph` and `multi-pack-index`.

The exit status is a bitmask, the same one `builtin/fsck.c` returns: object 1, reachable 2, pack 4, refs 8, commit-graph 16, multi-pack-index 32,
rev-index 64, bitmap 128. A condition git calls `die()` for exits 128 instead, after the run prints what it already found.

## Where the parallelism is

- **A pack** is a delta forest. `Pack.Verify` builds that forest once, then hands each root to a worker, which decodes the root and walks down its
  children reusing the parent's buffer. See `docs/pack-verification.md`.
- **Loose objects** are one flat list of files, split across workers.
- **The connectivity walk** draws from one shared stack. A level-at-a-time walk would be simpler, but history is long and narrow: most levels hold one
  commit, so three of four workers would idle and every commit would cost a barrier.
- **The object checks themselves** run inside the worker that decoded the object. This is the whole point: the check is most of the work, so running
  it under a lock would leave one core doing everything while the others decode ahead of it.

## The edge cache

git parses each object once and keeps the parsed object, so its connectivity walk re-reads nothing. Keeping whole parsed objects for a repository of
half a million objects is expensive, so this implementation keeps only what the walk actually needs: for each object, the list of objects it points
at, already resolved to table entries. That is `objEntry.edges`, three words per reference and no strings.

Two cases fall outside it and re-read the object. `--connectivity-only` never ran an object pass, so there is nothing to cache. `--name-objects`
builds each object's name from the path the walk took to reach it, which a recorded edge cannot carry.

## The delta base cache

A read of one packed object has to reconstruct every object under it in the delta chain. Without a cache, an object at depth ten costs ten inflations,
and its neighbour costs ten more even though nine of them were the same objects. `internal/odb/cache.go` is an LRU of reconstructed bases, sized to
git's own `core.deltaBaseCacheLimit` default of 96 MiB. Only a base goes in it, so the buffer a caller of `DB.Read` gets back is always its own.

This is what the object pass avoids entirely by walking the delta forest instead, and what `--connectivity-only` depends on, since that mode reads
objects in reachability order rather than pack order.

## Measured

On a synthetic repository of 229,960 objects, built by `scripts/make-bench-repo.sh`, against git 2.55.0. `scripts/bench.sh` produces these numbers,
and refuses to print a time unless the two implementations agreed on the output first. It times `--dry-run`, which is the comparable command and
also the one that cannot touch the repository being measured.

The repair half is not in these numbers because on a healthy repository it costs nothing. Its scan skips the pack verification and the object walk
when the fsck above it came back clean, which is the difference between 0.65s and 3.2s here. `internal/repair.ScanTrustingFsck` says what it still
checks and why.

Four cores, so read every ratio against four. Each figure is the best of nine runs, because the machine's own noise is wider than several of the
differences below.

| run                        | git   | git-fixed | git-fixed, one worker |
|----------------------------|-------|-----------|-----------------------|
| `fsck`                     | 1.27s | 0.66s     | 1.31s                 |
| `fsck --connectivity-only` | 0.48s | 0.80s     | 1.53s                 |
| `fsck --no-full`           | 0.03s | 0.08s     |                       |

`--connectivity-only` is the one mode that is still slower than git. It has no object pass, so it has no edge cache to walk and pays a full object
read per node; git's advantage there is that its parsed objects are already in memory from the mark pass. `--no-full` is far too short to say
anything.

## What is still serial

Four workers give 1.99x of one, not 4x, so about two thirds of the run is parallel. The rest runs on the main goroutine while the workers wait, and
it is what a machine with many cores hits first. The largest piece left is `checkPackRevIndexes`, which verifies each `.rev` file one pack at a time.

Four things that used to be serial, or that scaled badly, are fixed. Each was found in a profile, not by reading the code:

- **The object table used to be 256 shards.** A shard is taken once per tree entry, millions of times, so on ninety six cores a third of those takes
  would collide. It is 64 shards per core now, and each shard's lock is padded off its neighbour's cache line.
- **`objTable.All()` used to sort every object by name.** Nothing needed it -- the reporter sorts the whole report by `sortKey` before printing -- and
  it was a single-threaded sort of the entire repository on the critical path.
- **The connectivity walk used to take the shared stack once per object.** With a worker per core, that one lock was the ceiling. A worker now claims
  `walkBatch` objects at a time and hands a surplus back, which divides the traffic by the batch size.
- **`buildLayout` used to sort `packEntry` itself, and `packEntry` held a string.** Every swap was a write barrier and moved 48 bytes. It now sorts a
  16-byte pointer-free pair, and the entry's one string moved to `packLayout.headerErrs`, which took the phase from 230ms of profiled CPU to 100ms.
  This runs before any worker starts, so all of it was on the critical path.

## zlib's own messages

git prints its decompressor's complaint before its caller adds one, so a corrupt object produces a line like `error: inflate: data stream error
(invalid block type)`. Go's `compress/flate` reports every one of those cases as one corrupt-input error, so the reason has to be worked out
separately. `internal/zlibmsg` is an inflate that produces nothing but the first fault, and it runs only after a read has already failed. See
`docs/zlib-messages.md`.

## Package layout

| package             | what it holds                                                                             |
|---------------------|-------------------------------------------------------------------------------------------|
| `cmd/git-fixed`     | the one binary: option table, the two modes, defaults, exit status                          |
| `internal/parseopt` | git's parse-options behaviour: `--no-` forms, unique-prefix abbreviation, git's usage text   |
| `internal/gitobj`   | object names and object types                                                               |
| `internal/gitrepo`  | the repository: config, refs, reflogs, index, worktrees                                     |
| `internal/odb`      | the object database: loose files, packs, alternates, delta decoding, pack verification      |
| `internal/fsck`     | the checks themselves, matching `fsck.c`: trees, commits, tags, blobs, the severity table   |
| `internal/gitpath`  | whether a tree entry name can reach `.git` on some filesystem                               |
| `internal/fsckcmd`  | the ref pass, the object phases, the object table, the connectivity walk, the reporter                         |
| `internal/gittest`  | test repositories, including deliberately broken ones, and the comparison against real git  |

`internal/parseopt` exists instead of cobra because a drop-in replacement has to accept exactly what git accepts and refuse exactly what git refuses.
git abbreviates any unambiguous long option, spells the negative form `--no-x`, and prints a usage block cobra cannot reproduce. The org convention is
cobra for a CLI whose shape we choose; this CLI's shape is already fixed by the tool it replaces.
