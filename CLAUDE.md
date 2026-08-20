# git-fixed

Drop-in replacements for git commands, in Go, that use every core. Only `git fsck` exists so far. More commands are planned, so keep anything general
out of `internal/fsckcmd`.

## The contract

- **Compatible means the same SET of lines and the same exit status.** git's own order is not reproducible, so the tests sort both outputs and
  compare. Never relax this to "close enough" -- see `docs/output-ordering.md`.
- **Every behaviour comes from git 2.43.0's source, not from guessing.** When a check's wording or ordering is in question, build a repository that
  triggers it and run the real `git fsck`. Several bugs here were found exactly that way.
- **A new check needs a differential test in the same change.** `internal/fsckcmd/differential_test.go` and `repos_test.go` hold 41 of them;
  `internal/gittest` writes the broken repositories git's porcelain refuses to produce.
- **`go-toolchain`, bare, is the build.** It gates coverage at 80%. Never run `go build` or `go test` directly, and never pipe its output.

## Layout

- `cmd/git-fsck` -- option table and exit status. `internal/parseopt` implements git's parse-options, not cobra: the CLI's shape is fixed by the tool
  it replaces. See `docs/architecture.md`.
- `internal/gitobj` -- object names and types. `internal/gitrepo` -- config, refs, reflogs, index, worktrees.
- `internal/odb` -- loose objects, packs, alternates, delta decoding, pack verification, the delta base cache.
- `internal/fsck` -- the checks from `fsck.c`: trees, commits, tags, blobs, the message-id severity table.
- `internal/gitpath` -- whether a tree entry name reaches `.git` on some filesystem.
- `internal/fsckcmd` -- the six phases, the object table, the connectivity walk, the sorted reporter.
- `internal/gittest` -- test repositories and the comparison against the real `git fsck`.

## Performance invariants

Breaking one of these costs about half the run. Each is a mistake that was made here and measured.

- **The per-object callback runs unlocked.** `VerifyOpts.Object` is the whole fsck check, so a lock around it serializes everything.
  `docs/pack-verification.md`.
- **The object pass records edges; the connectivity walk reuses them.** Re-reading every object for the walk doubles the work. `objEntry.edges`.
- **A packed read goes through the delta base cache.** Without it an object at chain depth ten costs ten inflations. `internal/odb/cache.go`.
- **A tree is decoded once**, into a pooled slice, with entry names as `[]byte` views into the decode buffer. Copying them to strings was the single
  largest allocation source measured.
- `scripts/bench.sh <repo>` measures against the system git and refuses to print a time unless the output matched.

## Deliberate divergences

- **ext4 and ZFS `.git` aliases are reported; git checks only HFS+ and NTFS.** On by default, no opt-out. `docs/alias-detection.md`.
- **zlib's per-error detail line is missing** for a corrupt loose object other than a bad header, because Go's decompressor collapses those cases.
  This is the one known gap, and `docs/architecture.md` says what closing it takes.

## Docs

- `docs/architecture.md` -- phases, parallelism, caches, benchmarks, package layout
- `docs/alias-detection.md` -- the four filesystems and the four rules
- `docs/pack-verification.md` -- delta forest, worker distribution, big blobs
- `docs/output-ordering.md` -- the sort key and why output is deterministic
- `docs/commit-graph.md` -- commit-graph checks, chains, generation numbers
- `docs/multi-pack-index.md` -- multi-pack-index checks and its three failure vocabularies
