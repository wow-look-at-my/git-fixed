# Progress

A run over a large repository spends minutes inside one phase. Until `internal/progress` existed it printed nothing at all for that whole time, so
there was no way to tell a slow run from a hung one.

`--dry-run` stands in for `git fsck`, and that governs the meters as much as it governs the findings. git shows a meter on five phases, and this
shows one on the same five, with the same titles, the same delays, and the same wording. The repair scan that runs afterwards gets two more, because
it is a second full pass over the repository and git has nothing there to copy.

## The five git shows, and the two it does not

| Title | Total | Delayed | Where |
| --- | --- | --- | --- |
| `Checking object directories` | 256 | no | `fsckcmd/objects.go`, `checkLooseDir` |
| `Checking objects` | packed objects, whole repository | no | `fsckcmd/objects.go`, `checkObjectDirs` |
| `Checking connectivity` | countless | yes | `fsckcmd/connectivity.go` |
| `Verifying reverse pack-indexes` | packs | yes | `fsckcmd/graphs.go` |
| `Checking ref database` | 1 | no | `fsckcmd/refsverify.go` |
| `Verifying packs` | packed objects, whole repository | no | `repair/packs.go`, `scanPacks` |
| `Checking what the references reach` | objects held, plus the damage already named | no | `repair/walk.go`, `walk` |
| `Checking what came back` | countless | no | `repair/walk.go`, `descend` |

Each title is `builtin/fsck.c`'s own string, at lines 203, 804, 931, 961 and 1088 of git 2.55.0. The two totals worth noticing are that
`Checking object directories` counts the 256 fanout directories rather than the objects in them -- so a repository with a handful of loose objects
finishes it at once -- and that `Checking objects` counts every packed object in the repository rather than in one pack, so a single meter spans
every pack.

The last three are the repair's, and each draws only for the work nobody has already done: a pack that fsck read end to end is not verified again,
and the walk does not run at all when fsck named no damage anything reachable wants. See "What the scan takes from the fsck" in `docs/repair.md`.
Between them they were the longest silence in a run.

The walk's total is an estimate and the only one here that can be wrong low. It counts against the objects the repository holds, and a walk exists
to find an object it does not hold: one that is missing is reached, counted, and has no file to have been counted in. So `Meter.raise` moves a total
up to meet a count that passes it, rather than printing 150%.

`Checking what came back` is the same walk, started under the objects a repair pass has just put back rather than at the references. It has no total
at all: what it will reach is whatever was hidden under those objects, which is the thing nobody knows yet. A run draws one per layer of damage --
seeing several is a chain being repaired from the top down, not a phase repeating itself.

## Turning it on

`--progress` and `--no-progress` decide it. With neither, the meter draws when stderr is a terminal, and `--verbose` turns it off whatever else was
asked for. `builtin/fsck.c:1038` does the same three things in the same order.

Nothing between the option and the meter tests it. `run.meterOn` returns `nil` when progress is off, and every method on a `*Meter` works on a nil
receiver and draws nothing, so a counting loop has no condition in it and a worker that steps a meter pays two atomics.

## What one draw costs

`Step` is called once per object, on every worker, so it must be nearly free when it draws nothing. It adds to an atomic counter and returns unless
the percentage moved or the timer came round. The percentage last drawn is its own atomic, which is what keeps the common case off the lock.

The redraw timer is a goroutine per meter that raises a flag. git arms a one second `SIGALRM` and draws on the next call after it fires; a flag
raised on a ticker is the same thing without a signal handler. Nothing in that goroutine writes: the meter is drawn by whichever worker next notices
the flag.

## Going forwards only

git advances the loose-object meter once per fanout directory, from one thread, in order (`fsck_subdir`, `builtin/fsck.c:788`). Here the directories
are checked on every core and finish out of order, so the same count arriving from several workers would make the meter jump backwards.

`Advance` moves the count to `n` only when `n` is ahead of it, by compare-and-swap. `Step` increments. Between them a meter only ever goes forwards,
whatever order the work finishes in.

## Divergences from git's drawing

Three, all cosmetic, none of them affecting a line this tool prints on stdout:

- **No terminal-width split.** git measures the terminal and breaks the title onto its own line when the two would not fit (`progress->split`,
  `progress.c:156`). This writes one line and lets the terminal wrap it.
- **No foreground check.** git skips a draw when stderr's terminal belongs to another process group (`is_foreground_fd`). A background run here still
  writes its meter.
- **No throughput.** git's meter can carry a byte rate. None of the fsck meters use it, so there is nothing to reproduce.

What is reproduced is the line itself: `title: %3d%% (n/total)` with a `\r`, or `title: n` when there is no total, and a last one whose counters
are followed by `, done.` and a newline. A meter that never drew -- because its phase beat its one second delay -- prints nothing at all, which is
git's `last_value != -1` guard at `progress.c:375`. A line shorter than the last one is padded with spaces, because the tail of the last one would
otherwise stay on screen. What follows the counters -- the clock and the marks -- is this tool's own, and the section below says why.

## What the run costs

Every meter line carries a bracketed field git has nothing to copy for: how long the phase has been running, and the largest resident set the run
has held. Swap joins it once there is any.

```
Checking objects:  34% (35596328/102713556) [2m47s, peak 78.00 GiB]
```

The clock is there because a number climbing with no time beside it says nothing about whether the phase is minutes or hours from the end. The mark
is there because a run over a repository larger than the machine is killed part way through, and the last line drawn is then the whole of what is
left to diagnose it by. `docs/memory.md` says where the marks come from and why the resident one is so much larger than anything the run allocated.
