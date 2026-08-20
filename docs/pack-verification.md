# Pack verification

`Pack.Verify` is git's `verify_pack()`. It answers four questions about one pack, and hands every object it decodes to the caller so the fsck checks
can run on it without a second pass.

1. Does the index's own trailing checksum match?
2. Does the pack's trailing checksum match its contents, and does the index record that same checksum?
3. Does each entry's compressed byte range still match the CRC the index recorded for it?
4. Does each object decode, and does it hash to the name the index gives it?

One group of failures stops the pack, because nothing below them can be read: a file too short to be a pack, a missing `PACK` signature, an
unsupported version, an object count that disagrees with the index, or a trailing hash the index does not record. Every other failure, the two
checksum comparisons included, is reported and the check carries on.

## The delta forest

A pack stores most objects as a delta against another object. Reading one object naively means reconstructing every object beneath it, so a chain of
ten costs ten inflations -- and the object next to it in the chain pays for nine of the same ones again.

`buildLayout` reads every entry header once and turns the pack into a forest. An entry that is not a delta is a root. An offset-delta's parent is the
entry at the recorded offset, found by binary search over the offset-sorted entries. A ref-delta's parent is looked up in this pack's index, and
nowhere else, which is what git's `get_delta_base()` does -- "the base entry _must_ be in the same pack". A base that is not there is an entry
nothing can produce, and it is reported rather than walked. Children are stored as one flat `childList` with a `childStart` index, so the forest costs
two int32 slices rather than a slice header per node.

Each worker then takes one root and walks its subtree depth-first, keeping one buffer per level of the chain on a stack. A child is built by applying
its delta to the parent's buffer, which is already in memory. Every object in the pack is inflated exactly once, whatever its depth.

The forest is also why this path never touches the delta base cache: it does not need one. See `docs/architecture.md` for the cache that the ordinary
read path, and `--connectivity-only`, do depend on.

## What the chain costs, and the budget that bounds it

A stack of one buffer per level is what makes each object cost one inflation, and it is also what makes the walk's memory depend on something the
repository's own size does not predict. The cost is workers, times the depth of a chain, times the size of the objects in it. A pack of 72 objects
that are 128 MiB blobs in one chain reached 3,096 MB on four cores; the repository is 18 MB of pack. A repository whose packs hold large files
rewritten many times -- which is a common thing to have and the reason a repository ends up here -- is exactly that shape.

`walker.budget` is one atomic counter over all the workers, `DefaultChainBudget` of 256 MiB, and `take` reserves against it before a buffer is
stacked. Two properties keep it a bound rather than a deadlock:

- **Depth zero is always allowed.** A root, and the first level under it, are taken whatever the counter says. Every worker can therefore always make
  progress, and a single object larger than the whole budget still decodes.
- **A child that cannot be afforded is not waited for.** It goes on a `deferred` list and the walk carries on. Afterwards each deferred entry is
  rebuilt by `rebuild`, which walks up `packLayout.parents` to the root and applies the chain downwards, holding two objects at a time rather than
  the chain. That costs the deferred entry its own chain of inflations, and it is why the budget cannot lose a finding: an entry that will not
  decode is refused the same way on both paths.

Descending into a node's LAST child hands the parent's buffer over instead of stacking a second one on top of it. A chain that never branches --
which is what a large file rewritten many times is -- therefore holds one buffer, not one per revision, and the budget is never reached by it at all.

Measured on that 72-object pack: 3,096 MB before, 533 MB after, and the run got faster rather than slower, 9.99s to 4.55s, because the collector had
a fraction of the live heap to walk. `TestAChainBudgetChangesNothingButMemory` runs the same packs at several budgets and requires the same objects
and the same complaints out of each.

## Where the work is spread

Roots are handed out with one atomic counter, so a worker that draws a shallow root comes back for another. CRC checking is a flat scan over the
mapping, independent of the forest, so it runs as its own parallel pass in batches of 512 entries.

`VerifyOpts.Object` is called from every worker at once, with no lock. That callback is the entire per-object fsck check -- decoding the tree,
checking every entry name, resolving links into the object table -- so it is most of the run's work. Serializing it leaves one core doing everything
while the others decode ahead of it and wait; that mistake cost a 2x slowdown before it was found. `VerifyOpts.Emit` does take a lock, which is free
in practice because a pack that emits anything at all is broken and rare.

The bytes handed to `Object` are only valid until it returns. They belong to the walker's chain stack, and the next child overwrites them.

The pack's own checksum, question 2 above, is one thread reading every byte of the pack. On a multi-gigabyte pack that is a minute of one core with
the rest of the machine waiting for it, and it answers a question the object walk does not ask. It runs in its own goroutine beside the walk, so a
pack costs whichever of the two is slower rather than their sum.

## Big blobs

An undeltified blob larger than `core.bigFileThreshold` is hashed by streaming through `StreamHash` instead of being held in memory, which is what git
does. It cannot be a delta base for anything the walk needs, so nothing downstream misses its contents.
