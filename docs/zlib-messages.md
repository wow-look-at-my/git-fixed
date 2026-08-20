# zlib's messages

git prints its decompressor's own complaint before its caller adds one, so a corrupt object produces two lines:

```
error: inflate: data stream error (invalid block type)
error: unable to unpack header of .git/objects/0a/13cc0f7f941c492bd259b147e5e84bc23a8643
```

The first line is `git-zlib.c`'s `git_inflate()` printing `zerr_to_string(status)` and `strm->msg`. The message inside the brackets is zlib's own,
one of twenty strings compiled into `libz`.

Go's `compress/flate` reports every one of those cases as one error, `flate: corrupt input before offset N`. The reason is therefore not available
from the error the read returned, and it has to be worked out separately. `internal/zlibmsg` does that.

## What it is

`zlibmsg.Diagnose(raw, maxOut)` is a complete inflate whose only product is the first thing zlib would object to. It is plain, unoptimised code: it
decodes one bit at a time, keeps a 32 KiB window so a match can be checked, and computes the Adler-32 the hard way.

None of that costs anything, because it never runs on a stream that decodes. A read fails first, and only then is Diagnose asked why.

## The order of the tests is the answer

zlib checks the two header bytes in a fixed order, and that order decides which message a broken first byte produces:

1. The header checksum: `(CMF << 8 | FLG) % 31`. Most corrupt first bytes fail here, which is why `incorrect header check` is the message a
   truncated or overwritten file usually gets.
2. The compression method. `unknown compression method` appears only when the checksum happens to pass and the method is still not DEFLATE.
3. The window size. `invalid window size` needs a stream that asks for more than the 32 KiB git requested.
4. The dictionary bit, which is not an error at all: zlib returns `Z_NEED_DICT`, leaves no message, and git prints `needs dictionary (no message)`.

Reading those tests in the wrong order produces plausible messages that are wrong for most inputs. The same applies inside a block: an
over-subscribed code-length alphabet is `invalid code lengths set`, and the missing end-of-block symbol is only checked once every length has been
read.

## The output budget

A fault only counts when the reader actually reached it. git reads a loose object's header into 32 bytes and stops there, so a fault further down
the stream is one it never sees; the read of the contents finds it later. `Diagnose` therefore takes the room the failed read had, and stops
decoding once that room is full. `zlibmsg.Whole` asks about the whole stream.

This matters more than it sounds. Go's decompressor decodes ahead of what it was asked for, so it reports a fault zlib has not reached. The header
read in `internal/odb/loose.go` asks `Diagnose` whether zlib had a fault at all, rather than trusting its own error, for exactly that reason.

## Where it is used

- `internal/odb/loose.go` -- the header read (32 bytes of room), the read of the contents, and the streaming read of a big blob.
- `internal/odb/pack.go` -- `Pack.InflateMessage`, for an entry that will not decode. git gives that read one byte more than the index promised, so
  that a payload longer than that is noticed rather than cut short.
- `internal/odb/db.go` -- a read by object name that dies carries the line with it, in `FatalError.Inflate`.

## Tests

`internal/zlibmsg/zlibmsg_test.go` builds a stream for each of the sixteen messages a zlib stream can produce, bit by bit, because a compressor
writes none of them. That pins each message to one input.

The oracle is elsewhere, and it is the real check: `internal/fsckcmd/corrupt_test.go` fills one repository with 328 loose objects, each broken at
one byte, and requires the whole report to match the real `git fsck`. git links the real zlib, so agreeing with git is agreeing with zlib.
`TestCorruptPackBytes` does the same for a packfile.
