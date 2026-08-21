# Alias detection: names that reach `.git`

A tree entry named `.git` is refused, because checking it out would let the repository's own control files be written by a checkout. The attack is not
the literal name: it is any name that the filesystem RESOLVES to `.git`. A filesystem that folds case, drops code points, normalizes Unicode, or
derives a short name gives an attacker many spellings of one file.

`internal/gitpath` answers one question: could this tree entry name open `.git`, `.gitmodules`, `.gitattributes`, `.gitignore`, or `.mailmap` on some
filesystem? The tree check asks it for every entry.

## What git checks, and what we add

git checks two filesystems: HFS+ and NTFS. It does so unconditionally, on every platform, because the repository is what travels -- a tree written on
Linux is checked out on a Mac. This implementation keeps both checks exactly as git spells them, and adds two more filesystems on the same reasoning.

| filesystem | rule                                                       | example that resolves to `.gitmodules` |
|------------|------------------------------------------------------------|----------------------------------------|
| HFS+       | ignores 16 format code points anywhere in the name          | `.gitm<U+200C>odules`                  |
| NTFS       | 8.3 short names; trailing spaces and periods are stripped   | `gi7eba~1`, `.gitmodules.`             |
| ext4       | Unicode case folding, when the `casefold` feature is on     | `.gitmoduleſ` (U+017F folds to `s`)    |
| ZFS        | Unicode normalization, and optional case insensitivity      | `.gitmodule` + `s` written as NFC/NFD  |

**This is deliberately a superset of git.** A name that only ext4 or only ZFS would resolve is reported by this implementation and not by git, so on a
repository holding such a name the two disagree. That is the intended behaviour: the check exists to stop a checkout from writing the control files,
and both filesystems really do resolve those names. The differential tests do not build such a repository, because there is nothing to agree about.

There is no option to turn this off. A default-off security check is a check nobody has.

## The four rules

**HFS+** (`isHFSDotGeneric`) walks the name one code point at a time and skips the ignorable ones: U+200C-U+200F, U+202A-U+202E, U+206A-U+206F, and
U+FEFF. What is left must be `.` then the control name, ASCII-case-insensitively, then the end of the name or a separator. Malformed UTF-8 ends the
comparison as a non-match.

**NTFS** (`isNTFSDotGit`, `isNTFSDotGeneric`) accepts three spellings: the plain name, the regular 8.3 short name (`gitmod~1` through `gitmod~4`), and
the fall-back short name NTFS derives when those are taken. git hard-codes that fall-back table and so do we: `gi7eba` for `.gitmodules`, `gi250a` for
`.gitignore`, `gi7d29` for `.gitattributes`, `maba30` for `.mailmap`. Any of the three may be followed by spaces and periods, which NTFS strips, or by
`:`, which starts an alternate data stream. `.git` has its own function because its short name is `git~1`.

The tree check applies the NTFS `.git` test again to each segment after a backslash, because NTFS reads a backslash as a directory separator and git
does not.

**ext4** (`isExt4DotGeneric`) compares under Unicode case folding, which is what an ext4 directory created with the `casefold` feature does. This
reaches past the ASCII folding in the NTFS test: U+017F, the long s, folds to `s`, so `.gitmoduleſ` opens `.gitmodules` there.

**ZFS** (`isZFSDotGeneric`) compares under NFKD, plus case folding. One comparison covers every `normalization=` setting a dataset can have, because
two names equal under formC, formD, or formKC are equal under formKD too. The case folding on top covers `casesensitivity=insensitive` and `=mixed`.

## Cost

Every check starts at `couldReach`, which rules a name out on its first two bytes. Each of these spellings begins with a period, a tilde, the first
two letters of an 8.3 short name (`gi` or `ma`), or a code point HFS+ ignores -- and every ignored code point is outside ASCII. Nothing any of the
four filesystems do puts one of those at the front of a name that does not have it: a case fold and a normalization both leave an ASCII byte alone,
and neither deletes one. So `README` is answered in two comparisons instead of six checks.

It runs once per tree entry per control name, which is five times for every entry in the repository, and it was a twelfth of a whole run. Two bytes
rather than one because `g` and `m` begin a great many ordinary names and only `gi` and `ma` begin one of these; `Makefile` and `main.go` still go
the long way. The claim is not obvious from any one check, so a differential test sweeps both bytes against the unfiltered checks:
`internal/gitpath/couldreach_test.go`.

The two added rules cost nothing on an ordinary repository. `matches` takes an ASCII fast path first: a name with no byte above 0x7F cannot be
normalized into a different name, and ext4's fold over ASCII is the fold the HFS test already applied, so neither new rule can find anything the two
original ones did not. Every entry in a normal tree takes that path. A name that does contain non-ASCII bytes is checked for NFKD normality before any
copy is made, so even there the allocation only happens for a name that is genuinely not in normal form.
