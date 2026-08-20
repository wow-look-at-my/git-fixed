# Reference consistency

git 2.51 folded `git refs verify` into `git fsck`. A run now checks the reference database before it reads one object. The check reads the *files*,
not the references they name. A ref file with a stray byte, a name no reference may carry, or a symref that points outside the ref namespace is a
defect even when the object it names is perfectly good.

This is `internal/fsckcmd/refsverify.go`. It runs first, and its findings are printed before the object phases start.

## What a finding looks like

```
error: refs/heads/bad: badRefContent: 1234
warning: refs/heads/nolf: refMissingNewline: misses LF at the end
error: packed-refs line 3: badPackedRefEntry: 'garbage' has invalid oid
```

The three fields are the path, the message id in camel case, and the complaint. The path is the reference's name, relative to the git directory. A
linked worktree prefixes its own references with `worktrees/<id>/`. A packed-refs finding names the line it is on, or `packed-refs.header`.

An error sets bit 8 in the exit status (`ErrorRefs`). A warning sets nothing. The ref checks define no fatal level, and `fsck_vreport` prints an
informational message as a warning. Five ids are informational, so `refMissingNewline`, `trailingRefContent`, `symlinkRef`,
`symrefTargetIsNotARef` and `emptyPackedRefsFile` reach the user as warnings and change no exit status.

## The message ids

Sixteen ids come from git's ref backend. `internal/fsck/msgid.go` holds their severities, and `-c fsck.<id>=<level>` retunes them like any other.

| id | severity | what it means |
| --- | --- | --- |
| `badRefFiletype` | error | the ref is a directory, a socket, a FIFO, or another thing that is not a file |
| `badRefName` | error | the name breaks `check_refname_format` |
| `badRefContent` | error | the file holds no object name at all |
| `badRefOid` | error | the file holds the null object name |
| `refMissingNewline` | info | the content ends without a LF |
| `trailingRefContent` | info | bytes follow the object name, or more than one LF follows a symref target |
| `symlinkRef` | info | the ref is a symbolic link, which is the deprecated way to store a symref |
| `badHeadTarget` | error | `HEAD` points somewhere other than `refs/heads/` |
| `badReferentName` | error | a symref target breaks `check_refname_format` |
| `symrefTargetIsNotARef` | info | a symref target is a valid name but is not under `refs/` |
| `emptyPackedRefsFile` | info | `packed-refs` exists and holds nothing |
| `badPackedRefHeader` | error | the first line starts with `#` but is not `# pack-refs with: ` |
| `badPackedRefEntry` | error | an entry or a peeled line does not parse |
| `packedRefEntryNotTerminated` | error | a line runs to the end of the file with no LF |
| `packedRefUnsorted` | error | the header claims `sorted` and the entries are not |

## The three passes over one reference

`checkOneRef` follows `files_fsck_ref()`. It asks three questions, in this order, and stops at the first one that fails.

1. **The file type.** `lstat` decides. A regular file and a symbolic link both continue. Anything else is `badRefFiletype`, and the reference is not
   read. This ordering matters: opening a FIFO would block forever.
2. **The name.** A root reference is exempt, because its name is one component in capitals. Every other name goes through `check_refname_format`.
3. **The content.** A symbolic link reports `symlinkRef`, then its target is judged. A file that starts with `ref:` is a symref. Anything else must
   be an object name followed by exactly one LF.

A symbolic link carries no trailing byte, so the newline checks do not apply to one.

## Root references

A root reference lives beside `refs/` rather than under it: `HEAD`, `MERGE_HEAD`, `FETCH_HEAD`, `CHERRY_PICK_HEAD`. `fsck.IsRootRef` decides which
files in the git directory are references at all. A name qualifies when it is one component of upper-case letters, `-`, and `_`, or when it is one of
the six irregular ones git names explicitly (`HEAD`, `AUTO_MERGE`, `BISECT_EXPECTED_REV`, `NOTES_MERGE_PARTIAL`, `NOTES_MERGE_REF`,
`MERGE_AUTOSTASH`). Everything else in the git directory is a file that is not a reference, and the check ignores it.

`HEAD` gets one extra rule: its target must be under `refs/heads/`. A worktree's own `HEAD` gets the same rule, after the `worktrees/<id>/` prefix is
stripped off.

## packed-refs

The whole file has a grammar, and `checkPackedRefs` walks it:

- An empty file is `emptyPackedRefsFile`. A missing file is normal and reports nothing.
- A symlink is `badRefFiletype`, with a different message from the one a symlinked ref gets.
- The first line is the header when it starts with `#`. It must be `# pack-refs with: ` followed by traits. The trait `sorted` turns on the sort
  check at the end.
- An entry is `<oid> <refname>`. The object name must parse, a space must follow it, the name must hold no NUL byte, and the name must pass
  `check_refname_format`.
- A `^<oid>` line may follow an entry. It carries what a tag points at. Nothing else may follow the object name on that line.
- A last line with no LF is `packedRefEntryNotTerminated`. git reports it and then reads the line anyway.

The sort check compares names byte by byte, in the order the file stores them. It stops at the first pair out of order, as git does.

**The first line is judged twice.** git reads the first line to find out whether the file has a header, before it knows whether the file has one. A
first line with no newline is therefore reported once by that read and once by the loop that follows. This looks like a bug in git, and it is
reproduced here, because compatibility means the same set of lines.

## The reader and the checker are separate

`internal/gitrepo/refs.go` reads references so that the connectivity walk has roots. `refsverify.go` checks reference files. The two disagree on
purpose:

- The reader skips a FIFO, a device, and a dot file, because it must not hang and because git's own directory walk never yields a dot file. The
  checker still sees all three and complains about them.
- The reader gives the null object name to any reference it cannot parse, and marks it broken. That is what git hands its callbacks when it iterates
  including broken references.
- A `packed-refs` line that git's *reader* refuses is fatal, not a finding. git dies with `unexpected line in .git/packed-refs: <line>`, so
  `Repo.PackedRefsFatal` carries that message and the run prints it and stops.

## Tests

`internal/fsckcmd/refs_test.go` holds 23 differential subtests: ten content cases, three name cases, the symlink case, the file-type case
(built with `syscall.Mkfifo`), and eight packed-refs cases. Each one builds a repository, breaks one thing in it, and requires our output and the real
`git fsck`'s output to hold the same set of lines and the same exit status.

Those tests found seven real defects in this code: a panic on an unparsable ref, a hang on a FIFO, and five wrong-output paths.
