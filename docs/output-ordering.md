# Output ordering

git's fsck prints as it goes, from one thread, so its output order is whatever order it happened to visit things in. That order is not reproducible:
it falls out of `readdir` order for loose objects, of pack order for packed ones, and of the layout of git's internal object hash for the connectivity
report. Two runs of git on two machines over the same repository can print the same lines in a different sequence.

Doing the work on every core would make that worse: the sequence would then also depend on which worker finished first. So this implementation does
not print as it goes. Every line is queued with a key that says where it came from, and each phase is sorted and printed before the next phase starts.

## The key

```go
type sortKey struct {
	phase int
	group int   // object directory, or pack number
	pos   int64 // offset in a pack
	oid   gitobj.OID
	seq   int64 // keeps messages about one object in the order they were made
}
```

Comparison runs down that list. `phase` puts the six phases in the order `builtin/fsck.c` produces them. `group` separates object directories, and
packs within a directory. `pos` is a pack offset, so a pack's complaints come out in the order the pack stores its objects. `oid` orders everything
else, which makes the loose-object output independent of `readdir`. `seq` is assigned when the line is queued, and it keeps several lines about one
object in the order the check made them -- the reason a line is there is often the line above it.

The result is one fixed order for a given repository, whatever the worker count. `TestWorkerCountsAgree` requires exactly that: it runs the same
repository at several worker counts and requires byte-identical output.

## Why comparing against git still works

The differential tests sort both outputs before comparing them, so the comparison is over the SET of lines and the exit status. That is the strongest
claim that can honestly be made against a tool whose own order is not stable. See `docs/architecture.md`.

## Verbose output is not queued

`--verbose` prints one line per object. Holding those would cost memory in proportion to the repository, for lines nobody diffs, so they are written
straight out as they are produced. Their order under several workers is therefore not fixed. git's own verbose order is not fixed either.
