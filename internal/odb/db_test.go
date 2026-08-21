package odb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

func TestUnquoteC(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`"plain"`, "plain"},
		{`"a\nb"`, "a\nb"},
		{`"a\tb"`, "a\tb"},
		{`"a\rb"`, "a\rb"},
		{`"a\ab"`, "a\ab"},
		{`"a\bb"`, "a\bb"},
		{`"a\fb"`, "a\fb"},
		{`"a\vb"`, "a\vb"},
		{`"a\"b"`, `a"b`},
		{`"a\\b"`, `a\b`},
		{`"a\101b"`, "aAb"},  // an octal escape
		{`"a\0101"`, "a\b1"}, // three octal digits at most, so \010 then "1"
		{`"quoted" trailing`, "quoted"},
	} {
		got, ok := unquoteC(c.in)
		require.True(t, ok, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
	for _, in := range []string{
		``,
		`"`,
		`unquoted`,
		`"unterminated`,
		`"trailing backslash\`,
		`"bad escape \q"`,
	} {
		_, ok := unquoteC(in)
		assert.False(t, ok, "%q should not unquote", in)
	}
}

func TestReadAlternates(t *testing.T) {
	dir := t.TempDir()
	objects := filepath.Join(dir, "objects")
	require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o777))
	content := "# a comment\n" +
		"\n" +
		"/abs/path\n" +
		"../shared/objects\n" +
		"\"/quoted/pa\\th\"\n" +
		"\"unterminated\n"
	require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(content), 0o666))

	got := readAlternates(objects)
	assert.Equal(t, []string{
		"/abs/path",
		filepath.Join(dir, "shared", "objects"),
		"/quoted/pa\th",
	}, got)

	assert.Nil(t, readAlternates(filepath.Join(dir, "nowhere")))
}

func TestOpenEmptyObjectDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pack"), 0o777))
	db, err := Open(dir, ".git/objects", gitobj.SHA1)
	require.NoError(t, err)
	defer db.Close()

	require.Len(t, db.Dirs, 1)
	assert.Equal(t, dir, db.Dirs[0].Path)
	assert.Equal(t, ".git/objects", db.Dirs[0].Display)
	assert.Empty(t, db.Packs())

	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, []byte("nothing here"))
	assert.False(t, db.Has(oid))
	assert.False(t, db.HasPacked(oid))
	_, ok := db.Find(oid)
	assert.False(t, ok)
}

func TestOpenFollowsAlternates(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main", "objects")
	alt := filepath.Join(root, "alt", "objects")
	for _, d := range []string{main, alt} {
		require.NoError(t, os.MkdirAll(filepath.Join(d, "pack"), 0o777))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(main, "info"), 0o777))
	require.NoError(t, os.WriteFile(filepath.Join(main, "info", "alternates"), []byte(alt+"\n"), 0o666))

	db, err := Open(main, ".git/objects", gitobj.SHA1)
	require.NoError(t, err)
	defer db.Close()
	require.Len(t, db.Dirs, 2)
	assert.Equal(t, alt, db.Dirs[1].Path)

	// An object in the alternate is found through the main directory.
	content := []byte("shared\n")
	oid := Hash(gitobj.SHA1, gitobj.TypeBlob, content)
	hex := oid.String()
	require.NoError(t, os.MkdirAll(filepath.Join(alt, hex[:2]), 0o777))
	require.NoError(t, os.WriteFile(filepath.Join(alt, hex[:2], hex[2:]),
		looseObjectBytes(t, "blob", content), 0o444))

	typ, buf, err := db.Read(oid)
	require.NoError(t, err)
	assert.Equal(t, gitobj.TypeBlob, typ)
	assert.Equal(t, content, buf)
	assert.True(t, db.Has(oid))
	assert.False(t, db.HasPacked(oid), "a loose object is not a packed one")
}

// looseObjectBytes builds the on-disk form of a loose object.
func looseObjectBytes(t *testing.T, typeName string, content []byte) []byte {
	t.Helper()
	return deflate(t, append([]byte(typeName+" "+itoa(len(content))+"\x00"), content...))
}
