# git-fixed

One tool for repositories git has broken, in Go, using every core. `git-fixed` runs a full fsck and then repairs what it found; `--dry-run` stops
after the fsck. There is one binary and there must stay one. More commands are planned, so keep anything general out of `internal/fsckcmd`.

- **`--dry-run` is the drop-in for `git fsck`**, so its output and its exit status are git's whenever there is nothing to repair. A line printed
  there that git does not print is a bug, not a nicety.
- **A run must cost what the fsck costs.** The fsck hands the scan the packs it read end to end, the objects it could not produce, and whether that
  list is the whole of it -- so the scan re-reads neither. A status bit answers none of those: `ErrorObject` is a corrupt file and also a commit
  with no author. `repair.Verdict`, `docs/repair.md`.
- **A pass hands the next pass what it learned.** One pass repairs one layer, so a chain of damage costs several. Each later pass carries the packs
  the one before it read, checked by size and modification time, and starts its walk under the objects that pass put back rather than at the
  references. Four passes used to mean four full pack reads and four full walks. `descend`, `trustUnchanged`, `docs/repair.md`.
- **A remote is asked once, for the names only it has.** One scratch repository serves the run, `Prime` asks for what the three local rungs could not
  answer, and each name is asked once. A commit asked for by name brings its whole ancestry, so the ask is bounded by `--depth=1`, by a filter the
  scratch repository is a promisor to accept, and by a ref to negotiate from. One missing commit cost 480 objects and now costs 2. `docs/repair.md`.
- **A phase that takes time draws a meter, and the meter says what the run costs.** git shows one on five phases of its fsck and this shows one on
  the same five, plus two on the repair scan, which git has nothing to copy for. Every line carries the clock and the memory high-water mark,
  because a run the kernel kills for memory never reaches the line that would have said so. `internal/progress`, `internal/memwatch`,
  `docs/progress.md`, `docs/memory.md`.
- **Judge the fsck options before fsck runs.** `fsckcmd.Run` resolves some of them into the struct it was given -- with no object named the index
  becomes a head -- so `sameVerdict` asked afterwards answers about a command line nobody typed. `cmd/git-fixed/fsck.go`.

## Repair's one rule: no repair may lose data

Not "no more than was already lost". The repository worked before it broke and must work afterwards. This governs every choice in `internal/repair`:

- **Nothing is deleted, only quarantined.** A displaced file moves to `.git/git-fixed/quarantine/<run>/` with a manifest, and `--undo` restores it.
  No path in that package may call `os.Remove` on a repository file.
- **An object no source has is reported, and the run fails.** Never amputate it, never wind a ref back to route around it, never rewrite a tree or a
  commit. A repository that passes fsck because its broken parts were removed is not repaired.
- **Dangling and unreachable are not damage.** Never pruned, never counted. Never run `gc`, `prune`, `repack`, or `reflog expire` -- those are what
  break these repositories.
- **Every recovery is verified by hash.** `odb.WriteLoose` refuses content that does not hash to the name being recovered, which is what makes a
  recovery provably the original. Depth, and the six damage kinds: `docs/repair.md`.
- **A container is emptied before it is displaced.** A corrupt pack goes to quarantine only after every object it still yields is a loose object; a
  pack that yields none is reported and left alone. The index and `packed-refs` are salvaged line by line, never rebuilt from scratch.
  `internal/repair/packs.go`, `index.go`, `packedrefs.go`.

## The contract

- **Compatible means the same SET of lines and the same exit status.** git's own order is not reproducible, so the tests sort both outputs and
  compare. Never relax this to "close enough" -- see `docs/output-ordering.md`.
- **Every behaviour comes from git 2.55.0's source, not from guessing.** When a check's wording or ordering is in question, build a repository that
  triggers it and run the real `git fsck`. Several bugs here were found exactly that way.
- **The differential tests need git >= `gittest.MinGit`, and fail rather than skip below it.** An older git rewords messages, so a run against one
  compares this implementation against a different specification. CI installs git from `ppa:git-core/ppa` for the same reason.
- **A new check needs a differential test in the same change.** `internal/fsckcmd/differential_test.go`, `repos_test.go`, `refs_test.go` and
  `corrupt_test.go` hold 54 test functions, most of them table-driven over several repositories each. `internal/gittest` writes the broken
  repositories git's porcelain refuses to produce, including packs it will not build.
- **`go-toolchain`, bare, is the build.** It gates coverage at 80%. Never run `go build` or `go test` directly, and never pipe its output.
- **A test that writes over a file git made must chmod it first.** git writes a packfile and a loose object read-only, and the agent sandbox runs as
  root, where the mode is ignored. Such a test passes here and fails on CI for everyone else. `overwrite` in `repair_gaps_test.go` is the helper.

## Layout

- `cmd/git-fixed` -- the one binary: option table, the two modes, exit status. `internal/parseopt` implements git's parse-options, not cobra: the
  CLI's shape is fixed by the tool it replaces. See `docs/architecture.md`.
- `internal/gitobj` -- object names and types. `internal/gitrepo` -- config, refs, reflogs, index, worktrees.
- `internal/odb` -- loose objects, packs, alternates, delta decoding, pack verification, the delta base cache.
- `internal/fsck` -- the checks from `fsck.c`: trees, commits, tags, blobs, the message-id severity table.
- `internal/gitpath` -- whether a tree entry name reaches `.git` on some filesystem.
- `internal/progress` -- git's own meter: the same shape, the same 1% and 1s thresholds, plus an elapsed clock. `docs/progress.md`.
- `internal/memwatch` -- the memory and swap high-water marks the meters and the closing line carry. `docs/memory.md`.
- `internal/fsckcmd` -- the ref-consistency pass, the six object phases, the object table, the connectivity walk, the sorted reporter.
- `internal/repair` -- the damage scan, the walk, the recovery ladder, the quarantine, the refusal to amputate. `docs/repair.md`.
- `internal/gittest` -- test repositories and the comparison against the real `git fsck`.

## Performance invariants

Breaking one of these costs about half the run. Each is a mistake that was made here and measured.

- **The per-object callback runs unlocked.** `VerifyOpts.Object` is the whole fsck check, so a lock around it serializes everything.
  `docs/pack-verification.md`.
- **The object pass records edges; the connectivity walk reuses them.** Re-reading every object for the walk doubles the work. `objEntry.edges`.
- **A packed read goes through the delta base cache.** Without it an object at chain depth ten costs ten inflations. `internal/odb/cache.go`.
- **A tree is decoded once**, into a pooled slice, with entry names as `[]byte` views into the decode buffer. Copying them to strings was the single
  largest allocation source measured.
- **Nothing with one instance per object holds a pointer.** `packEntry` (24 bytes), `objEntry` (48, its edges named by index into an arena), the
  edges themselves (4) and the object table's slots are all pointer-free, so tens of millions of them cost the collector nothing per cycle.
  `TestAnObjectEntryHoldsNoPointer`, `TestAPackEntryStaysSmall`, `internal/fsckcmd/edgearena.go`.
- **A field on one of those is a field per object, so everything derivable is derived.** An entry ends where the next starts, a name is the hash
  without the length its algorithm holds, a resolved edge's type is its target's. `docs/architecture.md`, `docs/pack-verification.md`.
- **A pass hands the pack's pages back as it reads them.** Every byte of a pack is read, so a run that does not is resident for the whole of every
  pack -- which on these repositories is the machine. `releaseEvery`, `odb.Pack.Release`, `docs/memory.md`.
- **The object table is not a map, and it is sized before the first write.** Object names are already uniform, so four of their bytes are the hash;
  the size comes from the pack indexes, because growing a shard rehashes it. `docs/architecture.md` has the seven that were measured and fixed.
- **The delta walk holds one buffer per chain level, so it is bounded.** Without the budget the cost is workers times depth times object size, which
  reached 3 GB on a pack of 72 large blobs. `odb.DefaultChainBudget`, `docs/pack-verification.md`.
- **The heap is capped at three quarters of the machine, and the collector's target is halved.** A repository larger than the machine costs time,
  not the run: the limit is soft, so no check is skipped to stay under it. `GOGC=50` because a table that holds no pointer is cheap to collect and
  expensive to double. `GOMEMLIMIT`, `GOGC` and go-toolchain's cgroup guard all win. `cmd/git-fixed/memlimit.go`, `docs/architecture.md`.
- `scripts/bench.sh <repo>` measures against the system git and refuses to print a time unless the output matched.

## Deliberate divergences

- **ext4 and ZFS `.git` aliases are reported; git checks only HFS+ and NTFS.** On by default, no opt-out. `docs/alias-detection.md`.
- **A size in a header is refused when no stream on disk could produce it.** git reserves it and dies naming nothing; this reports the file and
  carries on. `docs/allocation-bounds.md`.
- **An unreadable index is a finding, not the end of the run.** git dies with 128 and skips four later phases that never open the index; this
  prints git's message, checks everything else, and sets bit 256. `docs/exit-status.md`.

One known gap. git's `unpack_entry()` falls back to the wider database for a delta whose base its pack does not hold, and can produce the object
after all; this reports the base it could not find and stops. Both print the same two error lines, so only a repository where the fallback would
have SUCCEEDED sees a difference. `internal/odb.materializeRoot`.

## Docs

- `docs/repair.md` -- the six damage kinds, the recovery ladder, why nothing is ever deleted
- `docs/architecture.md` -- phases, parallelism, caches, benchmarks, package layout
- `docs/alias-detection.md` -- the four filesystems and the four rules
- `docs/pack-verification.md` -- delta forest, worker distribution, big blobs
- `docs/output-ordering.md` -- the sort key and why output is deterministic
- `docs/commit-graph.md` -- commit-graph checks, chains, generation numbers
- `docs/multi-pack-index.md` -- multi-pack-index checks and its three failure vocabularies
- `docs/ref-consistency.md` -- the ref database check, its 16 message ids, packed-refs grammar
- `docs/zlib-messages.md` -- reproducing zlib's own complaint about a corrupt object
- `docs/allocation-bounds.md` -- the sizes a header may claim, and what happens to one no file could hold
- `docs/progress.md` -- the meters, their thresholds, the clock, and which phases draw one
- `docs/exit-status.md` -- the status bits, where git dies and this does not, and where 128 stays
- `docs/memory.md` -- the resident, anonymous and swap marks, where they are printed, and handing a pack's pages back as it is read
