# Commit-graph verification

A commit-graph is a cache. `git fsck` runs `git commit-graph verify` over it, which asks one question about every commit it holds: does the cached
record still match the commit in the object database? A stale record makes git give wrong answers about history, quietly, so fsck treats a mismatch as
an error and sets bit 16 (`ErrorCommitGraph`) in the exit status.

`internal/fsckcmd/commitgraph.go` reads the file itself rather than shelling out. It reports the same lines, in the same words.

## The file, and a chain of them

`objects/info/commit-graph` is one layer. `objects/info/commit-graphs/graph-<hash>.graph`, listed by a `commit-graph-chain` file, is a stack of them,
oldest first: each layer holds commits the ones below it do not, and a commit's position number counts through the whole stack. `lexBase` is where one
layer's own positions start, which is how a parent reference resolves into a lower layer.

`loadCommitGraph` validates the header and the chunk table before anything reads through them: signature `CGPH`, version 1, a hash version matching
the repository, a chunk table long enough for the count in the header, and the four chunks that must be present -- `OIDF` (fanout), `OIDL` (the sorted
object names), `CDAT` (one record per commit), and optionally `EDGE` (parents past the second) and `GDA2` (generation numbers v2). The last two checks
compare the commit count in the fanout against the actual chunk sizes, because everything downstream indexes into those chunks.

**A file that fails to load is reported twice.** git reads the graph once itself, then runs `commit-graph verify`, which reads it again and prints the
same complaint. Neither line carries a path, so both read identically. This is not a duplicate to be tidied up: printing it once disagrees with git.

## The checks, in git's order

The order matters, because the later checks index through the tables the earlier ones validate.

1. **Fanout monotonic.** `fanout[i] <= fanout[i+1]` for all 255 pairs. A failure returns immediately -- a broken fanout makes every lookup below it
   meaningless.
2. **File checksum.** The trailing hash over the rest of the file.
3. **Object names sorted, and fanout consistent.** Walk the lookup table in order. Each name must be strictly greater than the one before it, and the
   fanout entry for each leading byte must equal the index where that byte's names start.
4. If any of the above failed, stop. git does, for the same reason as step 1.
5. **Every commit against the object database.** Read the real commit, decode the graph's record, and compare: root tree, parent list (both too-long
   and terminates-early are reported), commit date, and generation number.

## Generation numbers

A commit's generation must be greater than every parent's. The check is `generation < maxParent+1`, with two adjustments that mirror git exactly.

A graph with no `GDA2` chunk stores generation v1, which saturates at `0x3FFFFFFF`; a parent already at that ceiling has its value decremented before
the comparison, so a saturated parent does not force an impossible value on its child.

A graph written before generation numbers existed stores zero everywhere, which is legal. Mixing the two is not. Once a zero generation is seen, the
per-commit generation and date checks stop, and if both a zero and a non-zero generation were seen anywhere in the file, the run reports the two
commits that showed it -- which is git's wording, complete with its `(e.g., commits '%s' and '%s')`.
