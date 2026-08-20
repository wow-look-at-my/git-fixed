# What a size in a header is allowed to ask for

Every object git stores says how big it is before it says what it is. A loose object begins `blob 1234\0`. A pack entry begins with a varint size. A
delta stream begins with the size of its base and the size of its result. Each of those numbers is read before the payload behind it is, and the
obvious thing to do with one is reserve that much memory and then fill it.

That is what git does, and what this tool did. It is fine everywhere except the place this tool is for.

## The failure

A loose object whose header says `tree 1099511627776\0` and whose file is twenty bytes long is not a terabyte-sized tree. It is four wrong bytes.
git reads the size, calls `xmallocz`, and dies:

```
fatal: Out of memory, malloc failed (tried to allocate 1099511627777 bytes)
```

Nothing is named. Nothing else in the repository is checked. The exit status is 128, which means git gave up rather than that anything is wrong with
the repository, and the one thing the operator needed -- which file is broken -- is the one thing not printed.

A pack entry does the same with its own header, and a delta stream does it with the size of its result. A repository can therefore make this tool ask
the allocator for any number that fits in a varint, and the answer to a big enough number is that the process dies. A tool whose entire purpose is
repositories that are already broken cannot read a corrupt number and then act on it.

## The bound

A deflate stream cannot expand by more than 1032 to 1. That is a property of the format rather than a guess about the data: the ratio comes from the
best case in the encoding, a fixed-Huffman block of length-258 matches, and no stream does better. So a claimed size past that is not a size the
available bytes could have produced, whatever else is wrong with the file.

`plausibleSize` in `internal/odb/inflatebound.go` is that comparison, at a ratio of 2048 rather than 1032. The margin is deliberate and it only goes
one way: refusing a valid object would be far worse than the allocation this exists to stop, so the bound sits well clear of anything a real stream
can reach, and a size that passes it is still checked the ordinary way afterwards.

- A **loose object** is bounded by the length of its own file.
- A **pack entry** is bounded by the bytes between the start of its stream and the end of the pack.
- A **delta result** is bounded differently, because it does not come from a stream at all: it comes from ops that copy out of the base. The cheapest
  op is one byte -- a copy command with no offset and no size bytes, meaning the first `0x10000` bytes of the base -- so a delta of `n` bytes cannot
  produce more than `n * 0x10000`. `maxDeltaOutput` is that, and the result is grown into rather than reserved, because every op is already bounded by
  the base and the last thing `applyDelta` does is check that the ops produced exactly the size the stream promised.

An object that fails one of these is reported the way any other unreadable object is reported, and the run carries on to the rest of the repository.

## Why this is a deliberate divergence

`git fsck` dies. This does not. That is the whole difference, and it is the second of the two places this tool deliberately parts company with git
(the first is `docs/alias-detection.md`).

It costs nothing on a healthy repository, because no valid object comes anywhere near the bound, so the output and the exit status are still git's
wherever git manages to produce them. It differs only where git stops being able to answer at all.

`TestLooseObjectCannotAskForMoreThanItsFileHolds` and `TestAnEntryCannotAskForMoreThanThePackHolds` pin both halves, and the first of them records
git's own behaviour on the same repository so the reason for the divergence stays checkable rather than remembered.
