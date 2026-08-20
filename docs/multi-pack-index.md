# Multi-pack-index verification

A `multi-pack-index` is one lookup table across several packs: it maps an object name to a pack and an offset, so git does not have to search each
pack's own index in turn. `git fsck` runs `git multi-pack-index verify` over it, which asks whether every entry still points at the object it claims.
A wrong entry makes git read the wrong bytes for an object name, so fsck sets bit 32 (`ErrorMultiPackIndex`) in the exit status.

`internal/fsckcmd/midx.go` reads `objects/pack/multi-pack-index` directly. Not having one is normal and silent.

## Two failure vocabularies

git treats structural damage and content mismatches differently, and so does this.

**A broken header makes git `die()`.** A bad signature, an unrecognised version, or a hash version that disagrees with the repository stops the whole
run: the message goes to stderr as a `fatal:`, and fsck exits 128 rather than setting a status bit. `noteFatalMsg` records the message, and `Run`
prints it after flushing what the run had already found -- git's own order.

**A missing required chunk prints `fatal:` and returns.** Confusingly, git prints these with the `fatal:` prefix but continues to the rest of fsck, so
the exit status is the ordinary bit. Both the prefix and the continuation are copied here deliberately.

**Everything else is an `error:`** counted toward the bit.

## Reported three times

A file too small to parse produces three lines:

```
error: multi-pack-index file <as git prints a path> is too small
error: multi-pack-index file <the absolute path> is too small
error: multi-pack-index file exists, but failed to parse
```

git reads the index once itself and once inside `multi-pack-index verify`. The first read names the file the way git prints a path; the second names
it by the `--object-dir` argument fsck passes, which is absolute. The third line is the caller's summary. Printing fewer than three lines disagrees
with git.

## The checks

1. **Header.** `MIDX`, version 1, hash version matching the repository, and enough bytes for the chunk table.
2. **Required chunks present.** `PNAM` (pack names), `OIDF` (fanout), `OIDL` (object names), `OOFF` (pack and offset per object). `LOFF` (large
   offsets) is optional and only needed if an entry references it.
3. **Pack names in order.** The names are NUL-separated, must number exactly what the header claims, and must be strictly increasing.
4. **File checksum.**
5. **Every named pack opens.** A pack that does not is reported by position, and its objects are reported individually below.
6. **Fanout monotonic**, and the object names strictly increasing.
7. **Every entry resolves.** For each object: the pack index must be in range and its pack must have opened; the named pack's own index must contain
   the object; and the offset recorded here -- read from `OOFF`, or from `LOFF` when the high bit is set -- must equal the offset that pack's index
   gives for it.

Unlike the commit-graph checks, a failure here does not stop the pass. git reports every entry it can and this does too, which is why a
multi-pack-index that is out of date with its packs produces one line per stale object rather than one line overall.
