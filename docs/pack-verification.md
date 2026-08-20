# Pack verification

`Pack.Verify` is git's `verify_pack()`. It answers four questions about one pack, and hands every object it decodes to the caller so the fsck checks can
run on it without a second pass.

1. Does the index's own trailing checksum match?
2. Does the pack's trailing checksum match its contents, and does the index record that same checksum?
3. Does each entry's compressed byte range still match the CRC the index recorded for it?
4. Does each object decode, and does it hash to the name the index gives it?

A failure at question 2 stops the run for that pack: nothing below it can be trusted. The others are reported and the check continues.

## The delta forest

A pack stores most objects as a delta against another object. Reading one object naively means reconstructing every object beneath it, so a chain of ten
costs ten inflations -- and the object next to it in the chain pays for nine of the same ones again.

`buildLayout` reads every entry header once and turns the pack into a forest. An entry that is not a delta is a root. An offset-delta's parent is the
entry at the recorded offset, found by binary search over the offset-sorted entries. A ref-delta's parent is looked up in the index; when the base lives
in a different pack, the entry becomes a root and the worker resolves it through the whole database. Children are stored as one flat `childList` with a
`childStart` index, so the forest costs two int32 slices rather than a slice header per node.

Each worker then takes one root and walks its subtree depth-first, keeping one buffer per level of the chain on a stack. A child is built by applying
its delta to the parent's buffer, which is already in memory. Every object in the pack is inflated exactly once, whatever its depth.

The forest is also why this path never touches the delta base cache: it does not need one. See `docs/architecture.md` for the cache that the ordinary
read path, and `--connectivity-only`, do depend on.

## Where the work is spread

Roots are handed out with one atomic counter, so a worker that draws a shallow root comes back for another. CRC checking is a flat scan over the
mapping, independent of the forest, so it runs as its own parallel pass in batches of 512 entries.

`VerifyOpts.Object` is called from every worker at once, with no lock. That callback is the entire per-object fsck check -- decoding the tree, checking
every entry name, resolving links into the object table -- so it is most of the run's work. Serializing it leaves one core doing everything while the
others decode ahead of it and wait; that mistake cost a 2x slowdown before it was found. `VerifyOpts.Emit` does take a lock, which is free in practice
because a pack that emits anything at all is broken and rare.

The bytes handed to `Object` are only valid until it returns. They belong to the walker's chain stack, and the next child overwrites them.

## Big blobs

An undeltified blob larger than `core.bigFileThreshold` is hashed by streaming through `StreamHash` instead of being held in memory, which is what git
does. It cannot be a delta base for anything the walk needs, so nothing downstream misses its contents.
