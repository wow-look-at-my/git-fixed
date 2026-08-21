package odb

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/zlibmsg"
)

// maxHeaderLen is git's MAX_HEADER_LEN: the first inflate of a loose object
// must produce the whole "<type> <size>\0" header within this many bytes.
const maxHeaderLen = 32

// LooseResult is what reading one loose object produced. It mirrors git's
// read_loose_object() closely enough that fsck can report the same lines.
type LooseResult struct {
	Type     gitobj.Type
	TypeName string
	Size     int64
	Contents []byte
	RealOID  gitobj.OID // hash of what the file actually holds
	// Errors holds the lines git prints from inside read_loose_object,
	// in order, before its caller adds its own.
	Errors []string
	// HashMismatch is set when the file decoded but hashes to another name.
	HashMismatch bool
	// Failed is set when the object could not be read at all.
	Failed bool
}

// inflateFailed records the complaint git's decompressor prints before its
// caller adds one. maxOut is how much output the failed read had room for:
// zlib stops once it has filled that, so it never reaches a fault beyond it.
func (res *LooseResult) inflateFailed(raw []byte, maxOut int64) {
	if msg := zlibmsg.Diagnose(raw, maxOut); msg != "" {
		res.Errors = append(res.Errors, msg)
	}
}

// ReadLoose decodes one loose object file and checks it against the object name
// its path claims.
func ReadLoose(path, shown string, expected gitobj.OID, algo *gitobj.Algo, bigFileThreshold int64) *LooseResult {
	res := &LooseResult{}
	f, err := os.Open(path)
	if err != nil {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to mmap %s: %s", shown, errnoText(err)))
		return res
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to unpack header of %s", shown))
		return res
	}
	m, err := mapReadOnly(path)
	if err != nil {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to mmap %s: %s", shown, errnoText(err)))
		return res
	}
	defer m.close()
	return readLooseBytes(m.bytes(), shown, expected, algo, bigFileThreshold, res)
}

// ReadLooseBytes decodes an in-memory loose object. Tests use it directly.
func ReadLooseBytes(raw []byte, shown string, expected gitobj.OID, algo *gitobj.Algo, bigFileThreshold int64) *LooseResult {
	return readLooseBytes(raw, shown, expected, algo, bigFileThreshold, &LooseResult{})
}

func readLooseBytes(raw []byte, shown string, expected gitobj.OID, algo *gitobj.Algo, bigFileThreshold int64, res *LooseResult) *LooseResult {
	br := bytes.NewReader(raw)
	zr, err := zlib.NewReader(br)
	if err != nil {
		res.Failed = true
		// git's decompressor prints its own complaint before its caller
		// adds one.
		res.inflateFailed(raw, maxHeaderLen)
		res.Errors = append(res.Errors, fmt.Sprintf("unable to unpack header of %s", shown))
		return res
	}
	defer zr.Close()

	var hdr [maxHeaderLen]byte
	n, err := readUpTo(zr, hdr[:])
	nul := bytes.IndexByte(hdr[:n], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		// Go's decompressor decodes ahead of what it was asked for, so
		// it reports a fault that zlib has not reached yet. git reads
		// the header into this many bytes and stops there, and gives up
		// here only for a fault inside those. Anything further down
		// belongs to the read of the contents.
		if msg := zlibmsg.Diagnose(raw, maxHeaderLen); msg != "" {
			res.Failed = true
			res.Errors = append(res.Errors, msg,
				fmt.Sprintf("unable to unpack header of %s", shown))
			return res
		}
	}
	if nul < 0 {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to unpack header of %s", shown))
		return res
	}
	typeName, size, ok := parseLooseHeader(string(hdr[:nul]))
	if !ok {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to parse header of %s", shown))
		return res
	}
	res.TypeName = typeName
	res.Type = gitobj.TypeFromName(typeName)
	res.Size = size
	if res.Type == gitobj.TypeBad {
		// git's parse_loose_header() stops at a type name it does not
		// know, and quotes the header it was reading.
		res.Failed = true
		res.Errors = append(res.Errors,
			fmt.Sprintf("unable to parse type from header '%s' of %s", hdr[:nul], shown))
		return res
	}

	if res.Type == gitobj.TypeBlob && size > bigFileThreshold {
		res.streamCheck(zr, raw, br, hdr[:n], nul, size, shown, expected, algo)
		return res
	}

	if !plausibleSize(size, int64(len(raw))) {
		// The header's size is bigger than this file could inflate to
		// however it is read, so it is damage rather than a size. Taking
		// it at its word here asks for that many bytes. see
		// inflatebound.go
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("unable to unpack contents of %s", shown))
		return res
	}
	out := make([]byte, size)
	pre := copy(out, hdr[nul+1:n])
	readErr := fillFrom(zr, out[pre:])
	trailing := br.Len()
	switch {
	case readErr != nil:
		res.Failed = true
		// The header is behind us, so this read runs to the end of the
		// stream and reaches whatever zlib objects to anywhere in it.
		res.inflateFailed(raw, zlibmsg.Whole)
		res.Errors = append(res.Errors,
			fmt.Sprintf("corrupt loose object '%s'", expected),
			fmt.Sprintf("unable to unpack contents of %s", shown))
		return res
	case !atStreamEnd(zr):
		// More inflated bytes than the header promised.
		res.Failed = true
		res.Errors = append(res.Errors,
			fmt.Sprintf("corrupt loose object '%s'", expected),
			fmt.Sprintf("unable to unpack contents of %s", shown))
		return res
	case trailing != 0:
		res.Failed = true
		res.Errors = append(res.Errors,
			fmt.Sprintf("garbage at end of loose object '%s'", expected),
			fmt.Sprintf("unable to unpack contents of %s", shown))
		return res
	}
	res.Contents = out
	res.RealOID = HashLiteral(algo, typeName, out)
	if res.RealOID != expected {
		res.Failed = true
		res.HashMismatch = true
	}
	return res
}

// streamCheck hashes a blob too large to hold in memory, the way git's
// check_stream_oid() does.
func (res *LooseResult) streamCheck(zr io.Reader, raw []byte, br *bytes.Reader, hdr []byte, nul int, size int64, shown string, expected gitobj.OID, algo *gitobj.Algo) {
	h := algo.New()
	h.Write(hdr[:nul+1])
	pre := hdr[nul+1:]
	if int64(len(pre)) > size {
		pre = pre[:size]
	}
	h.Write(pre)
	total := int64(len(pre))
	buf := make([]byte, 64*1024)
	var readErr error
	for total < size {
		want := int64(len(buf))
		if size-total < want {
			want = size - total
		}
		n, err := zr.Read(buf[:want])
		h.Write(buf[:n])
		total += int64(n)
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) || total != size {
		res.Failed = true
		res.inflateFailed(raw, zlibmsg.Whole)
		res.Errors = append(res.Errors, fmt.Sprintf("corrupt loose object '%s'", expected))
		return
	}
	if br.Len() != 0 {
		res.Failed = true
		res.Errors = append(res.Errors, fmt.Sprintf("garbage at end of loose object '%s'", expected))
		return
	}
	res.RealOID = gitobj.FromBytes(h.Sum(nil))
	if res.RealOID != expected {
		res.Failed = true
		res.Errors = append(res.Errors,
			fmt.Sprintf("hash mismatch for %s (expected %s)", shown, expected))
	}
}

// HashLiteral hashes content under a type name the caller supplies verbatim,
// which is how a loose object with an unknown type still gets a name.
func HashLiteral(algo *gitobj.Algo, typeName string, content []byte) gitobj.OID {
	h := algo.New()
	fmt.Fprintf(h, "%s %d", typeName, len(content))
	h.Write([]byte{0})
	h.Write(content)
	return gitobj.FromBytes(h.Sum(nil))
}

// Hash computes an object name from a known type and its content.
func Hash(algo *gitobj.Algo, t gitobj.Type, content []byte) gitobj.OID {
	return HashLiteral(algo, t.Name(), content)
}

// parseLooseHeader is git's parse_loose_header(): a type name, one space, and a
// size in canonical decimal. "010" is not canonical and is rejected.
func parseLooseHeader(hdr string) (typeName string, size int64, ok bool) {
	sp := -1
	for i := 0; i < len(hdr); i++ {
		if hdr[i] == ' ' {
			sp = i
			break
		}
	}
	if sp < 0 {
		return "", 0, false
	}
	typeName = hdr[:sp]
	rest := hdr[sp+1:]
	if rest == "" {
		return "", 0, false
	}
	d := int64(rest[0] - '0')
	if d > 9 {
		return "", 0, false
	}
	size = d
	i := 1
	if size != 0 {
		for i < len(rest) {
			c := int64(rest[i] - '0')
			if c > 9 {
				break
			}
			size = size*10 + c
			i++
		}
	}
	if i != len(rest) {
		return "", 0, false
	}
	return typeName, size, true
}

// readUpTo fills as much of buf as the reader offers before its first stop.
func readUpTo(r io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

// fillFrom reads len(buf) bytes and keeps the raw error. A clean early end and
// a truncated stream mean different things here: git zero-fills the payload for
// the first and calls the second corrupt.
func fillFrom(r io.Reader, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The stream ended before the header's size. git
				// keeps the zero-filled tail and lets the hash
				// check reject the object.
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// atStreamEnd reports whether the reader has nothing left, which is what tells
// a correct object from one whose payload runs past its declared size.
func atStreamEnd(r io.Reader) bool {
	var b [1]byte
	n, err := r.Read(b[:])
	return n == 0 && errors.Is(err, io.EOF)
}

func errnoText(err error) string {
	var perr *os.PathError
	if errors.As(err, &perr) {
		return perr.Err.Error()
	}
	return err.Error()
}
