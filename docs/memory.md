# Memory

A run over a large repository prints a climbing count and nothing about what it is costing the machine, so a run that is about to be killed for
memory looks exactly like a run that is halfway through. `internal/memwatch` puts the high-water marks where that count already is: on every meter
line, and once more on the line the run ends with.

## The three marks

| Mark | Where it comes from | Exact? |
| --- | --- | --- |
| Resident | `VmHWM` in `/proc/self/status` | yes, the kernel keeps it |
| Anonymous | `RssAnon`, sampled | a spike between two samples is missed |
| Swap | `VmSwap`, sampled | the same |

`VmHWM` is the kernel's own high-water mark, so it is right whenever it is read, however seldom that is. The other two lines are what the process
holds *now*, and the kernel keeps no mark for either, so `memwatch` keeps the largest it has seen. A mark only ever goes up: a later read that
catches a smaller number, or that fails outright, leaves the mark where the worst moment put it.

Each mark is reached at its own moment. They do not describe one instant, and the smaller ones are not a share of the larger one.

## The packfile pages, and giving them back

A pack is read through a mapping of the whole file (`odb.mapReadOnly`), and every page of it the scan touches is faulted in from the page cache and
counted against this process from that moment. The mapping is read-only and the pages are clean, and neither of those exempts a page from RSS. Every
byte of a pack is read -- the CRC pass scans the mapping end to end, and the object walk reads every entry -- so a run that did nothing about it
ended holding the whole of every pack. On a hundred-gigabyte pack that is a hundred gigabytes of resident memory for bytes the run has finished with,
next to a heap a fraction of the size.

So the passes that read a pack hand its pages back as they go. `Pack.Release` is `madvise(MADV_DONTNEED)` over the pack's mapping: the file stays
mapped, the kernel keeps its page cache, and a later read of any of it faults the page back rather than reading the disk again. One `releaser` counts
what all three passes read and sweeps every `releaseEvery` (256 MiB), because a single pack can be larger than the machine and a sweep at the end of
one comes after the moment that decides whether the run survives. The pack is swept once more when it has been read end to end. What this costs is a
minor fault on a page that is read twice; what it saves is the difference between a resident set the size of the repository and one the size of the
run.

Measured on a 215,981-object repository whose packfile is 1.13 GB, which `scripts/make-bench-repo.sh <dir> 2500 20000 25 16384` builds: peak
resident falls from 1.17 GiB to 317 MiB, and the fsck says exactly what it said before. What is left is the sweep window, the pack's index, and the
run's own heap.

The pack's index keeps its pages. `OIDAt` and `Find` read it for every object in every phase, so it is memory the run is using rather than memory it
has finished with -- about 28 bytes an object, which is 2.8 GiB on a hundred million.

The resident mark is what `top` shows and what the kernel decides an out-of-memory kill by, so it is the mark the meter carries. The closing line
carries the anonymous mark beside it: that second figure is the object table, the edges and the buffers -- what the run itself holds, and what the
machine cannot take back under pressure.

The two answer different questions. A resident mark well above the anonymous one is a run holding pages of a mapping, which the kernel can reclaim as
it needs them and which the sweep above hands back on its own. An anonymous mark near the heap ceiling in `cmd/git-fixed/memlimit.go` is a run whose
collector is running back-to-back, taking half the CPU to stay under a limit it cannot reach.

The resident mark never goes down, because `VmHWM` never does: it records the worst moment the run reached, and pages handed back afterwards do not
lower it. A run whose resident mark is far above its anonymous one reached that mark before a sweep, not while it was holding it.

## Where they are printed

The marks ride with the progress meters: `--progress` turns both on, `--no-progress` turns both off, with neither they follow whether stderr is a
terminal, and both go to stderr. That is not a coincidence of implementation. `--dry-run` stands in for `git fsck`, and a line printed there that
git does not print is a bug, so the marks appear exactly where the meters already diverge and nowhere else.

A meter line carries the resident mark, and swap once there is any of it:

```
Checking objects:  34% (35596328/102713556) [2m47s, peak 78.00 GiB]
Checking objects:  36% (37122880/102713556) [2m58s, peak 78.00 GiB +1.20 GiB swap]
```

Swap is silent until it is not zero. A run that never swapped has nothing to say about swap, and a `0 bytes` on every line of every run would be
noise on the line that matters.

The run ends with the whole of it:

```
Peak memory: 78.00 GiB resident (12.30 GiB of it this process's own), nothing swapped.
```

The mark is on the meter as well as at the end because a run the kernel kills never reaches the end. The last line drawn is then the whole of what
is left to diagnose it by, and it is the line that says how close the machine came.

## Sizes

`memwatch.Bytes` renders a size the way git renders one: a binary unit and two decimal places, as `git count-objects -H` prints `333.84 KiB`, and
whole bytes below a KiB. git prints no memory figure anywhere, so there is nothing to be compatible with here -- this is only so that the figures
read like the ones git prints beside them.

## What is not read, and where

A value in a unit this does not understand is refused, and so is a file with no `VmHWM` in it: a figure on a progress line is read as a
measurement, and a guessed one would be worse than no line at all. A system that publishes no `/proc/self/status` -- which is every system that is
not Linux -- gets meters with no memory field and no closing line, as it gets no heap ceiling from `cmd/git-fixed/memlimit.go` either.

The file is read at most once every 250ms, however often the meters draw. `Meter.Step` runs once per object on every worker, and a file read on
each draw would be paid for by the phase it is reporting on. The resident mark is the kernel's and loses nothing by being read seldom; the interval
is what decides how finely the other two are sampled.

`cmd/git-fixed/profile.go` measures something else and is off unless asked for. `GIT_FIXED_MEMPROFILE` writes a heap profile and prints the peak
live *heap*, which is the Go allocator's own number and moves when a per-object structure changes. The marks here are the operating system's, and
they count the mapped packfiles the heap profile cannot see.
