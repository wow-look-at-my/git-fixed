# Architecture

`git-fixed --dry-run` is a drop-in replacement for `git fsck`. It reads the same repository, accepts the same options, prints the same lines, and exits
with the same status. What differs is that every expensive phase runs on all cores. Without `--dry-run` the same run goes on to repair what it found.

## What "compatible" means, and how it is measured

Two runs agree when they print the same SET of lines and exit with the same status. The set, not the sequence: git's own order falls out of `readdir`
order and of its internal hash table, so it is not reproducible from one machine to the next. `internal/gittest/fsck.go` compares that way. It runs
the system `git fsck` in the same repository, splits both outputs into non-empty lines, sorts them, and requires equality along with the exit code.

`internal/fsckcmd/differential_test.go`, `repos_test.go`, `refs_test.go` and `corrupt_test.go` hold 54 test functions making that comparison, most
of them table-driven over several repositories each. Each builds a repository
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

## The object table

There is one entry per object the run has heard of, so every field in it is paid for a million times over. The entry is 64 bytes: the name, an
atomic type, atomic flags, and the edge slice. `internal/fsckcmd/objtable.go`.

It is not a Go map. An object name is already a uniform hash, so a table that takes four of its bytes as the hash needs no hash function at all --
with a map, `Lookup` was a quarter of the whole run on a million-object repository. Four properties follow from being written once per tree entry,
which is tens of millions of times:

- **The slots are open-addressed and eight bytes wide**: four bytes of the name, and the entry's index plus one so that zero is empty. At the
  half-full load factor that is sixteen bytes an object for the index, against a Go map's pointer-bearing buckets.
- **Entries come from 4096-entry slabs**, so a million objects cost a few hundred allocations rather than a million, and each entry has a dense
  index that an edge can name without a pointer.
- **The table is sized from the pack indexes before the first write.** A shard that starts at eight slots and grows to eight thousand rehashes ten
  times, which was five percent of a million-object run spent reaching a size that was known in advance. `newObjTable(expect)`.
- **Nothing in a slot or an edge is a pointer**, so neither the slot table nor the edges cost the collector anything on a cycle.

## The edge cache

git parses each object once and keeps the parsed object, so its connectivity walk re-reads nothing. Keeping whole parsed objects for a repository of
half a million objects is expensive, so this implementation keeps only what the walk actually needs: for each object, the list of objects it points
at, already resolved. That is `objEntry.edges`, and one edge is a single `uint64` -- the target's index into the slabs in the low 32 bits, then the
bit that says it resolved, the bit that marks a tag's target, and four bits for the type the reference implies.

Packing it into one word rather than holding a pointer and a type is not about the eight bytes saved. There is one edge per tree entry, so a pointer
here puts tens of millions of words under the collector on every cycle, for a structure nothing ever frees until the run ends.

Two cases fall outside it and re-read the object. `--connectivity-only` never ran an object pass, so there is nothing to cache. `--name-objects`
builds each object's name from the path the walk took to reach it, which a recorded edge cannot carry.

## The delta base cache

A read of one packed object has to reconstruct every object under it in the delta chain. Without a cache, an object at depth ten costs ten inflations,
and its neighbour costs ten more even though nine of them were the same objects. `internal/odb/cache.go` is an LRU of reconstructed bases, sized to
git's own `core.deltaBaseCacheLimit` default of 96 MiB. Only a base goes in it, so the buffer a caller of `DB.Read` gets back is always its own.

This is what the object pass avoids entirely by walking the delta forest instead, and what `--connectivity-only` depends on, since that mode reads
objects in reachability order rather than pack order.

## Measured

Against git 2.55.0, on four cores, so read every ratio against four. `scripts/bench.sh` produces the time and refuses to print one unless the two
implementations agreed on the output first. Each figure is the best of several runs, on a warm page cache, and memory is peak RSS.

The main repository is the synthetic 229,960 objects `scripts/make-bench-repo.sh` builds. Two others are here because they are the shapes that break
a tool rather than the shape that is common: 988,000 small objects, and 72 objects that are 128 MiB blobs in one delta chain.

| repository        | git             | git-fixed       | before this work |
|-------------------|-----------------|-----------------|------------------|
| 229,960 objects   | 1.10s / 114 MB  | 0.37s / 97 MB   | 0.44s / 179 MB   |
| 988,000 objects   | 6.02s / 332 MB  | 2.07s / 407 MB  | 2.71s / 772 MB   |
| 72 x 128 MiB, one chain | 13.03s / 403 MB | 4.55s / 533 MB | 9.99s / 3,096 MB |

The last row is what the whole memory bound is for. A delta chain is decoded by keeping every parent alive down to the leaf, so the cost was workers
times chain depth times object size, and nothing in the repository's own size predicted it. `docs/pack-verification.md`.

One worker over the 229,960 costs 0.92s against four workers' 0.37s, so about three quarters of the run is parallel.

### The narrow modes cost more than git, and it is the repair scan

| run over the 229,960       | git            | git-fixed      |
|----------------------------|----------------|----------------|
| `--dry-run`                | 1.10s / 114 MB | 0.37s / 97 MB  |
| `--dry-run --connectivity-only` | 0.33s / 81 MB  | 2.62s / 246 MB |
| `--dry-run --no-full`      | 0.02s / 13 MB  | 2.28s / 249 MB |
| `--dry-run --strict`       |                | 2.56s / 240 MB |

`--strict` only changes how findings are graded: it adds no pass, and it reads nothing extra. It costs the same 2.5s as the two modes that read
LESS, which is what says where the time goes. It is not the fsck. It is the repair scan underneath, and it is there in every row but the first.

That scan verifies every pack and walks everything the references reach. `ScanTrustingFsck` skips both when the run above it was a full default fsck
that came back clean, because such a run has already read every object and reported any that would not decode. A narrower fsck has not looked --
`--connectivity-only` reads no object and `--no-full` skips the packs -- so the scan has to look itself, and the 2.2s is work git never does at all.

The narrow modes therefore pay the full damage scan to save a fraction of a full fsck, which is a bad trade for anyone who reaches for them to go
faster. Making it a good one means letting the fsck hand its object table to the scan instead of the scan re-reading everything, which is a real
change to the boundary between the two halves and is not attempted here.

## What is still serial

Four workers give 2.49x of one, not 4x, so about four fifths of the run is parallel. The rest runs on the main goroutine while the workers wait, and
it is what a machine with many cores hits first. A profile of the 229,960 charges the main goroutine 150ms of a 449ms run, and the largest piece of
that is `checkPackRevIndexes` at 70ms, which verifies each `.rev` file one pack at a time.

Seven things that used to be serial, or that scaled badly, are fixed. Each was found in a profile, not by reading the code:

- **The object table used to be 256 shards.** A shard is taken once per tree entry, millions of times, so on ninety six cores a third of those takes
  would collide. It is 64 shards per core now, and each shard's lock is padded off its neighbour's cache line.
- **`objTable.All()` used to build a slice of every object and sort it by name.** Nothing needed either half -- the reporter sorts the whole report
  by `sortKey` before printing -- and it was a single-threaded sort of the entire repository on the critical path. Indices are dense now, so a phase
  that visits every object counts up to `Len` and calls `At`, and there is no slice to build.
- **The connectivity walk used to take the shared stack once per object.** With a worker per core, that one lock was the ceiling. A worker now claims
  `walkBatch` objects at a time and hands a surplus back, which divides the traffic by the batch size.
- **`buildLayout` used to sort `packEntry` itself, and `packEntry` held a string.** Every swap was a write barrier and moved 48 bytes. It now sorts a
  16-byte pointer-free pair, and the entry's one string moved to `packLayout.headerErrs`, which took the phase from 230ms of profiled CPU to 100ms.
  This runs before any worker starts, so all of it was on the critical path.
- **The object table used to be a Go map.** `Lookup` was a quarter of the whole run on a million-object repository, hashing names that are already
  hashes. It is open-addressed over 8-byte slots now, keyed on four bytes of the name itself.
- **Each shard used to start at eight slots and grow into its size.** A shard that ends at eight thousand slots rehashes its contents ten times over,
  and that was five percent of a million-object run spent reaching a size the pack indexes already knew. The table is sized before the first write.
- **The pack's own checksum used to run in front of the object walk.** It is one thread reading every byte of the pack, so on a multi-gigabyte pack
  that is a minute of one core with the rest of the machine waiting. It answers a question the walk does not ask, so it now runs beside it and the
  pack costs whichever of the two is slower.

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
| `internal/zlibmsg`  | the decompressor's own complaint about a corrupt object, which `compress/flate` does not distinguish |
| `internal/progress` | the phase meters, drawn the way `progress.c` draws them: `docs/progress.md`                  |
| `internal/repair`   | the damage scan, the recovery ladder, the quarantine: `docs/repair.md`                       |
| `internal/gittest`  | test repositories, including deliberately broken ones, and the comparison against real git  |

`internal/parseopt` exists instead of cobra because a drop-in replacement has to accept exactly what git accepts and refuse exactly what git refuses.
git abbreviates any unambiguous long option, spells the negative form `--no-x`, and prints a usage block cobra cannot reproduce. The org convention is
cobra for a CLI whose shape we choose; this CLI's shape is already fixed by the tool it replaces.
