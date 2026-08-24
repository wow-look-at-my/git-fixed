package gitobj_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

func TestAlgoByName(t *testing.T) {
	assert.Same(t, gitobj.SHA1, gitobj.AlgoByName("sha1"))
	assert.Same(t, gitobj.SHA256, gitobj.AlgoByName("sha256"))
	assert.Nil(t, gitobj.AlgoByName("sha512"))
	assert.Nil(t, gitobj.AlgoByName(""))
}

func TestEmptyConstants(t *testing.T) {
	assert.Equal(t, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", gitobj.SHA1.Empty.String())
	assert.Equal(t, "4b825dc642cb6eb9a060e54bf8d69288fbee4904", gitobj.SHA1.EmptyTree.String())
	assert.Len(t, gitobj.SHA256.Empty.String(), 64)
	assert.Len(t, gitobj.SHA256.EmptyTree.String(), 64)
}

func TestParse(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef01234567"
	oid, ok := gitobj.SHA1.Parse(hex)
	require.True(t, ok)
	assert.Equal(t, hex, oid.String())
	assert.True(t, oid.Valid())
	assert.False(t, oid.IsNull())

	_, ok = gitobj.SHA1.Parse(hex[:39])
	assert.False(t, ok, "a short name is not a full object name")
	_, ok = gitobj.SHA1.Parse(hex + "0")
	assert.False(t, ok, "a long name is not a full object name")
	_, ok = gitobj.SHA1.Parse("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	assert.False(t, ok, "a non-hex name is not an object name")
	_, ok = gitobj.SHA256.Parse(hex)
	assert.False(t, ok, "a sha1 name is not a sha256 name")
}

func TestParseHexBytes(t *testing.T) {
	// Upper-case input decodes, and trailing bytes are ignored: the packed-refs reader relies on both.
	oid, ok := gitobj.SHA1.ParseHexBytes([]byte("0123456789ABCDEF0123456789abcdef01234567 refs/heads/x"))
	require.True(t, ok)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", oid.String())

	_, ok = gitobj.SHA1.ParseHexBytes([]byte("0123"))
	assert.False(t, ok)
	_, ok = gitobj.SHA1.ParseHexBytes([]byte("g123456789abcdef0123456789abcdef01234567"))
	assert.False(t, ok)
	_, ok = gitobj.SHA1.ParseHexBytes([]byte("0g23456789abcdef0123456789abcdef01234567"))
	assert.False(t, ok)
}

func TestNullAndRaw(t *testing.T) {
	null := gitobj.SHA1.Null()
	assert.True(t, null.IsNull())
	assert.True(t, null.Valid())
	assert.Equal(t, "0000000000000000000000000000000000000000", null.String())
	assert.Len(t, null.Raw(), 20)

	var zero gitobj.OID
	assert.False(t, zero.Valid())
	assert.False(t, zero.IsNull(), "an object name with no hash at all is not the null name")
	assert.Empty(t, zero.String())

	raw := make([]byte, 20)
	raw[19] = 0xff
	assert.Equal(t, "00000000000000000000000000000000000000ff", gitobj.SHA1.FromRaw(raw).String())
	assert.Equal(t, "00000000000000000000000000000000000000ff", gitobj.FromBytes(raw).String())
}

func TestCompare(t *testing.T) {
	a, _ := gitobj.SHA1.Parse("0000000000000000000000000000000000000001")
	b, _ := gitobj.SHA1.Parse("0000000000000000000000000000000000000002")
	assert.Equal(t, -1, a.Compare(b))
	assert.Equal(t, 1, b.Compare(a))
	assert.Equal(t, 0, a.Compare(a))
}

func TestTypeNames(t *testing.T) {
	for _, c := range []struct {
		typ  gitobj.Type
		name string
	}{
		{gitobj.TypeNone, "none"},
		{gitobj.TypeCommit, "commit"},
		{gitobj.TypeTree, "tree"},
		{gitobj.TypeBlob, "blob"},
		{gitobj.TypeTag, "tag"},
		{gitobj.TypeOfsDelta, "ofs-delta"},
		{gitobj.TypeRefDelta, "ref-delta"},
		{gitobj.TypeBad, ""},
		{gitobj.TypeAny, ""},
	} {
		assert.Equal(t, c.name, c.typ.Name())
	}
}

func TestTypeFromName(t *testing.T) {
	assert.Equal(t, gitobj.TypeCommit, gitobj.TypeFromName("commit"))
	assert.Equal(t, gitobj.TypeTree, gitobj.TypeFromName("tree"))
	assert.Equal(t, gitobj.TypeBlob, gitobj.TypeFromName("blob"))
	assert.Equal(t, gitobj.TypeTag, gitobj.TypeFromName("tag"))
	assert.Equal(t, gitobj.TypeBad, gitobj.TypeFromName("Commit"))
	assert.Equal(t, gitobj.TypeBad, gitobj.TypeFromName("none"))
}

func TestTypeHeader(t *testing.T) {
	assert.Equal(t, "blob 12\x00", string(gitobj.TypeBlob.Header(12)))
	assert.Equal(t, "commit 0\x00", string(gitobj.TypeCommit.Header(0)))
}
