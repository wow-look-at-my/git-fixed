package odb

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// deflate compresses bytes the way git writes a loose object.
func deflate(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, err := w.Write(raw)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// looseObject returns the compressed form of "<type> <size>NUL<content>".
func looseObject(t *testing.T, typeName, content string) []byte {
	t.Helper()
	return deflate(t, []byte(typeName+" "+itoa(len(content))+"\x00"+content))
}

// truncated compresses raw and then cuts the compressed bytes short, so the
// inflate itself runs out rather than the payload.
func truncated(t *testing.T, raw string) []byte {
	t.Helper()
	z := deflate(t, []byte(raw))
	return z[:len(z)-6]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestParseLooseHeader(t *testing.T) {
	for _, c := range []struct {
		hdr  string
		name string
		size int64
	}{
		{"blob 0", "blob", 0},
		{"blob 12", "blob", 12},
		{"commit 1024", "commit", 1024},
		{"whatever 7", "whatever", 7},
	} {
		name, size, ok := parseLooseHeader(c.hdr)
		require.True(t, ok, c.hdr)
		assert.Equal(t, c.name, name)
		assert.Equal(t, c.size, size)
	}
	for _, hdr := range []string{
		"blob",   // no size at all
		"blob ",  // an empty size
		"blob x", // not a number
		"blob 12x",
		"blob 010", // git refuses a size that is not canonical
		"blob 00",
	} {
		_, _, ok := parseLooseHeader(hdr)
		assert.False(t, ok, "%q should not parse", hdr)
	}
}

func TestHash(t *testing.T) {
	// The empty blob is the object name every git repository shares.
	assert.Equal(t, gitobj.SHA1.Empty, Hash(gitobj.SHA1, gitobj.TypeBlob, nil))
	assert.Equal(t, gitobj.SHA1.EmptyTree, Hash(gitobj.SHA1, gitobj.TypeTree, nil))
	assert.Equal(t, gitobj.SHA256.Empty, Hash(gitobj.SHA256, gitobj.TypeBlob, nil))
	assert.Equal(t, gitobj.SHA1.Empty, HashLiteral(gitobj.SHA1, "blob", nil))
}

func TestReadLooseBytes(t *testing.T) {
	content := "hello\n"
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte(content))
	res := ReadLooseBytes(looseObject(t, "blob", content), "shown", oid, gitobj.SHA1, 1<<20)
	assert.False(t, res.Failed)
	assert.Empty(t, res.Errors)
	assert.False(t, res.HashMismatch)
	assert.Equal(t, gitobj.TypeBlob, res.Type)
	assert.Equal(t, "blob", res.TypeName)
	assert.Equal(t, int64(len(content)), res.Size)
	assert.Equal(t, content, string(res.Contents))
	assert.Equal(t, oid, res.RealOID)
}

func TestReadLooseBytesHashMismatch(t *testing.T) {
	other := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte("different"))
	res := ReadLooseBytes(looseObject(t, "blob", "hello\n"), "shown", other, gitobj.SHA1, 1<<20)
	assert.True(t, res.HashMismatch)
	assert.True(t, res.Failed, "an object under the wrong name has not been read")
	assert.Empty(t, res.Errors, "the caller words this one, not the reader")
	assert.NotEqual(t, other, res.RealOID)
}

func TestReadLooseBytesShortPayloadIsAHashMismatch(t *testing.T) {
	// The stream ends cleanly but before the size the header promised.
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte("hello\n"))
	res := ReadLooseBytes(deflate(t, []byte("blob 99\x00hello\n")), "shown", oid, gitobj.SHA1, 1<<20)
	assert.True(t, res.HashMismatch)
	assert.Empty(t, res.Errors)
	assert.Len(t, res.Contents, 99)
}

func TestReadLooseBytesFailures(t *testing.T) {
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte("hello\n"))
	for _, c := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"not compressed", []byte("plain text, not zlib"), "inflate: data stream error (incorrect header check)"},
		{"header too long", deflate(t, []byte(strings.Repeat("x", 64))), "unable to unpack header of shown"},
		{"bad header", deflate(t, []byte("blob x\x00hello\n")), "unable to parse header of shown"},
		{"truncated stream", truncated(t, "blob 40\x00"+strings.Repeat("a", 40)), "corrupt loose object '" + oid.String() + "'"},
		{"trailing garbage", append(deflate(t, []byte("blob 6\x00hello\n")), 'x'), "garbage at end of loose object '" + oid.String() + "'"},
	} {
		res := ReadLooseBytes(c.raw, "shown", oid, gitobj.SHA1, 1<<20)
		assert.Contains(t, strings.Join(res.Errors, "\n"), c.want, c.name)
	}
}

func TestReadLooseBytesUnknownType(t *testing.T) {
	// git reads a type name it does not know without failing, so the caller can report it itself.
	raw := looseObject(t, "widget", "x")
	oid := HashLiteral(gitobj.SHA1, "widget", []byte("x"))
	res := ReadLooseBytes(raw, "shown", oid, gitobj.SHA1, 1<<20)
	assert.Equal(t, "widget", res.TypeName)
	assert.Equal(t, gitobj.TypeBad, res.Type)
	assert.False(t, res.HashMismatch)
}

func TestReadLooseBytesBigFile(t *testing.T) {
	// Past the threshold the contents are checked as a stream and not kept.
	content := strings.Repeat("a", 4096)
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte(content))
	res := ReadLooseBytes(looseObject(t, "blob", content), "shown", oid, gitobj.SHA1, 100)
	assert.Empty(t, res.Errors)
	assert.False(t, res.HashMismatch)
	assert.Nil(t, res.Contents, "a big object is not kept in memory")
	assert.Equal(t, int64(len(content)), res.Size)
}

func TestReadLoose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obj")
	content := "hello\n"
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte(content))
	require.NoError(t, os.WriteFile(path, looseObject(t, "blob", content), 0o444))
	res := ReadLoose(path, "shown", oid, gitobj.SHA1, 1<<20)
	assert.Empty(t, res.Errors)
	assert.Equal(t, content, string(res.Contents))

	res = ReadLoose(filepath.Join(dir, "missing"), "shown", oid, gitobj.SHA1, 1<<20)
	assert.True(t, res.Failed)
	assert.Contains(t, res.Errors[0], "unable to mmap shown")

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o444))
	res = ReadLoose(empty, "shown", oid, gitobj.SHA1, 1<<20)
	assert.True(t, res.Failed)
	assert.Equal(t, []string{"unable to unpack header of shown"}, res.Errors)
}
