# git-fixed

Tools for repositories git has broken, in Go, using every core. `git fsck` finds damage; `git fix` repairs it. More commands are planned, so keep
anything general out of `internal/fsckcmd`.

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
  pack whose index will not map is reported and left alone. The index and `packed-refs` are salvaged line by line, never rebuilt from scratch.
  `internal/repair/packs.go`, `index.go`, `packedrefs.go`.

## The contract

- **Compatible means the same SET of lines and the same exit status.** git's own order is not reproducible, so the tests sort both outputs and
  compare. Never relax this to "close enough" -- see `docs/output-ordering.md`.
- **Every behaviour comes from git 2.55.0's source, not from guessing.** When a check's wording or ordering is in question, build a repository that
  triggers it and run the real `git fsck`. Several bugs here were found exactly that way.
- **The differential tests need git >= `gittest.MinGit`, and fail rather than skip below it.** An older git rewords messages, so a run against one
  compares this implementation against a different specification. CI installs git from `ppa:git-core/ppa` for the same reason.
- **A new check needs a differential test in the same change.** `internal/fsckcmd/differential_test.go`, `repos_test.go`, `refs_test.go` and
  `corrupt_test.go` hold 50 test functions, most of them table-driven over several repositories each. `internal/gittest` writes the broken
  repositories git's porcelain refuses to produce.
- **`go-toolchain`, bare, is the build.** It gates coverage at 80%. Never run `go build` or `go test` directly, and never pipe its output.

## Layout

- `cmd/git-fsck` -- option table and exit status. `internal/parseopt` implements git's parse-options, not cobra: the CLI's shape is fixed by the tool
  it replaces. See `docs/architecture.md`.
- `internal/gitobj` -- object names and types. `internal/gitrepo` -- config, refs, reflogs, index, worktrees.
- `internal/odb` -- loose objects, packs, alternates, delta decoding, pack verification, the delta base cache.
- `internal/fsck` -- the checks from `fsck.c`: trees, commits, tags, blobs, the message-id severity table.
- `internal/gitpath` -- whether a tree entry name reaches `.git` on some filesystem.
- `internal/fsckcmd` -- the ref-consistency pass, the six object phases, the object table, the connectivity walk, the sorted reporter.
- `cmd/git-fix` and `internal/repair` -- the damage scan, the recovery ladder, the quarantine, the refusal to amputate. `docs/repair.md`.
- `internal/gittest` -- test repositories and the comparison against the real `git fsck`.

## Performance invariants

Breaking one of these costs about half the run. Each is a mistake that was made here and measured.

- **The per-object callback runs unlocked.** `VerifyOpts.Object` is the whole fsck check, so a lock around it serializes everything.
  `docs/pack-verification.md`.
- **The object pass records edges; the connectivity walk reuses them.** Re-reading every object for the walk doubles the work. `objEntry.edges`.
- **A packed read goes through the delta base cache.** Without it an object at chain depth ten costs ten inflations. `internal/odb/cache.go`.
- **A tree is decoded once**, into a pooled slice, with entry names as `[]byte` views into the decode buffer. Copying them to strings was the single
  largest allocation source measured.
- **`packEntry` holds no pointer, and the object table has 64 shards per core.** There is one of each per object, so a pointer or a shard collision
  costs the whole repository. `docs/architecture.md` has the four that were measured and fixed.
- `scripts/bench.sh <repo>` measures against the system git and refuses to print a time unless the output matched.

## Deliberate divergences

- **ext4 and ZFS `.git` aliases are reported; git checks only HFS+ and NTFS.** On by default, no opt-out. `docs/alias-detection.md`.

There are no known gaps otherwise. The one that used to be here -- zlib's own complaint about a corrupt object, which Go's decompressor does not
distinguish -- is now reproduced by `internal/zlibmsg`. `docs/zlib-messages.md`.

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
