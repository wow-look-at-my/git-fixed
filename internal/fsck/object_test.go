package fsck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

const (
	hexA     = "0123456789abcdef0123456789abcdef01234567"
	hexB     = "89abcdef0123456789abcdef0123456789abcdef"
	goodDate = "1700000000 +0000"
)

// first returns the one message a check produced, or "" when it produced none.
func first(msgs []string) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[0]
}

func TestObjectDispatch(t *testing.T) {
	body := "tree " + hexA + "\nauthor A <a@e> " + goodDate + "\ncommitter A <a@e> " + goodDate + "\n\nm\n"
	_, msgs := collect(t, func(o *Options) int { return o.Object(nil, oidN(1), gitobj.TypeCommit, []byte(body)) })
	assert.Empty(t, msgs)

	_, msgs = collect(t, func(o *Options) int { return o.Object(nil, oidN(1), gitobj.TypeBlob, []byte("x")) })
	assert.Empty(t, msgs)

	_, msgs = collect(t, func(o *Options) int { return o.Object(nil, oidN(1), gitobj.Type(9), nil) })
	assert.Equal(t, "error: unknownType: unknown type '9' (internal fsck error)", first(msgs))
}

func TestVerifyHeaders(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"nul", "tree " + hexA + "\x00\n\nm\n", "nulInHeader: unterminated header: NUL at offset 45"},
		{"unterminated", "tree " + hexA, "unterminatedHeader: unterminated header"},
	} {
		_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(c.body)) })
		assert.Contains(t, first(msgs), c.want, c.name)
	}

	// A header block that ends in a newline but has no body is fine.
	body := "tree " + hexA + "\nauthor A <a@e> " + goodDate + "\ncommitter A <a@e> " + goodDate + "\n"
	_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(body)) })
	assert.Empty(t, msgs)
}

func TestCommitStructure(t *testing.T) {
	author := "author A <a@e> " + goodDate + "\n"
	committer := "committer A <a@e> " + goodDate + "\n"
	for _, c := range []struct{ name, body, want string }{
		{"no tree", "author A <a@e> " + goodDate + "\n\n", "missingTree: invalid format - expected 'tree' line"},
		{"bad tree", "tree zz\n" + author + committer + "\n", "badTreeSha1: invalid 'tree' line format - bad sha1"},
		{"bad parent", "tree " + hexA + "\nparent zz\n" + author + committer + "\n", "badParentSha1: invalid 'parent' line format - bad sha1"},
		{"no author", "tree " + hexA + "\n" + committer + "\n", "missingAuthor: invalid format - expected 'author' line"},
		{"two authors", "tree " + hexA + "\n" + author + author + committer + "\n", "multipleAuthors: invalid format - multiple 'author' lines"},
		{"no committer", "tree " + hexA + "\n" + author + "\n", "missingCommitter: invalid format - expected 'committer' line"},
	} {
		_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(c.body)) })
		assert.Contains(t, first(msgs), c.want, c.name)
	}

	// A parent line is optional and may repeat.
	body := "tree " + hexA + "\nparent " + hexA + "\nparent " + hexB + "\n" + author + committer + "\n"
	_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(body)) })
	assert.Empty(t, msgs)
}

func TestCommitNulInBody(t *testing.T) {
	body := "tree " + hexA + "\nauthor A <a@e> " + goodDate + "\ncommitter A <a@e> " + goodDate + "\n\nmsg\x00more\n"
	_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(body)) })
	assert.Contains(t, first(msgs), "nulInCommit: NUL byte in the commit object body")
}

// identCase builds a commit whose author line is the one under test.
func identCase(line string) string {
	return "tree " + hexA + "\nauthor " + line + "\ncommitter A <a@e> " + goodDate + "\n\nm\n"
}

func TestIdent(t *testing.T) {
	for _, c := range []struct{ name, line, want string }{
		{"leading angle", "<a@e> " + goodDate, "missingNameBeforeEmail"},
		{"bad name", "A> <a@e> " + goodDate, "badName"},
		{"no email", "A " + goodDate, "missingEmail"},
		{"no space before email", "A<a@e> " + goodDate, "missingSpaceBeforeEmail"},
		{"bad email", "A <a@e " + goodDate, "badEmail"},
		{"no space before date", "A <a@e>" + goodDate, "missingSpaceBeforeDate"},
		{"bad date", "A <a@e> xyz +0000", "badDate"},
		{"zero padded date", "A <a@e> 0123456789 +0000", "zeroPaddedDate"},
		{"overflow", "A <a@e> 99999999999999999999 +0000", "badDateOverflow"},
		{"bad timezone", "A <a@e> 1700000000 xx", "badTimezone"},
		{"short timezone", "A <a@e> 1700000000 +00", "badTimezone"},
	} {
		_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(identCase(c.line))) })
		assert.Contains(t, first(msgs), c.want, c.name)
	}

	// A timestamp of exactly "0" is legal, and so are extra blanks after
	// the email.
	for _, line := range []string{"A <a@e> 0 +0000", "A <a@e>  \t 1700000000 -0530", "A <> 1700000000 +0000"} {
		_, msgs := collect(t, func(o *Options) int { return o.Commit(nil, oidN(1), []byte(identCase(line))) })
		assert.Empty(t, msgs, line)
	}
}

// tagBody assembles a tag object from its four header lines.
func tagBody(object, typ, name, tagger string) string {
	b := "object " + object + "\ntype " + typ + "\ntag " + name + "\n"
	if tagger != "" {
		b += "tagger " + tagger + "\n"
	}
	return b + "\nmessage\n"
}

func TestTag(t *testing.T) {
	good := tagBody(hexA, "commit", "v1.0", "A <a@e> "+goodDate)
	ret, info := 0, TagInfo{}
	o := NewOptions(gitobj.SHA1)
	o.Error = func(*Options, any, gitobj.OID, gitobj.Type, Severity, MsgID, string) int { return 1 }
	ret, info = o.TagWithInfo(nil, oidN(1), []byte(good))
	assert.Zero(t, ret)
	assert.Equal(t, "v1.0", info.Name)
	assert.Equal(t, gitobj.TypeCommit, info.TargetType)
	assert.Equal(t, hexA, info.Object.String())
}

func TestTagErrors(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"no object", "type commit\n\n", "missingObject: invalid format - expected 'object' line"},
		{"bad object", tagBody("zz", "commit", "v1", "A <a@e> "+goodDate), "badObjectSha1: invalid 'object' line format - bad sha1"},
		{"no type", "object " + hexA + "\ntag v1\n\n", "missingTypeEntry: invalid format - expected 'type' line"},
		{"bad type", tagBody(hexA, "widget", "v1", "A <a@e> "+goodDate), "badType: invalid 'type' value"},
		{"no tag", "object " + hexA + "\ntype commit\n\n", "missingTagEntry: invalid format - expected 'tag' line"},
		{"bad tag name", tagBody(hexA, "commit", "v1..0", "A <a@e> "+goodDate), "badTagName: invalid 'tag' name: v1..0"},
		{"bad tagger", tagBody(hexA, "commit", "v1", "A <a@e> nope +0000"), "badDate"},
	} {
		_, msgs := collect(t, func(o *Options) int { return o.Tag(nil, oidN(1), []byte(c.body)) })
		assert.Contains(t, first(msgs), c.want, c.name)
	}
}

func TestTagExtraHeaderIsIgnoredByDefault(t *testing.T) {
	// Only mktag asks about a header after 'tagger', so fsck stays quiet until the check is turned on.
	body := "object " + hexA + "\ntype commit\ntag v1\ntagger A <a@e> " + goodDate + "\nextra x\n\nm\n"
	_, msgs := collect(t, func(o *Options) int { return o.Tag(nil, oidN(1), []byte(body)) })
	assert.Empty(t, msgs)

	_, msgs = collect(t, func(o *Options) int {
		o.SetSeverity(MsgExtraHeaderEntry, SevError)
		return o.Tag(nil, oidN(1), []byte(body))
	})
	assert.Contains(t, first(msgs), "extraHeaderEntry: invalid format - extra header(s) after 'tagger'")
}

func TestTagMissingTaggerIsOnlyAWarning(t *testing.T) {
	// Tags older than the tagger line exist, so git only warns.
	body := tagBody(hexA, "commit", "v1", "")
	ret, msgs := collect(t, func(o *Options) int { return o.Tag(nil, oidN(1), []byte(body)) })
	assert.Zero(t, ret)
	assert.Contains(t, first(msgs), "warn: missingTaggerEntry")
}

func TestTagTruncatedAfterHeaderLine(t *testing.T) {
	// A header line with no newline of its own never reaches the
	// unexpected-end reports, because the header check refuses the object
	// first. git carries the same pair of unreachable branches.
	for _, body := range []string{
		"object " + hexA + "\ntype commit",
		"object " + hexA + "\ntype commit\ntag v1",
	} {
		_, msgs := collect(t, func(o *Options) int { return o.Tag(nil, oidN(1), []byte(body)) })
		assert.Equal(t, []string{"error: unterminatedHeader: unterminated header"}, msgs)
	}
}

func TestParseTimestamp(t *testing.T) {
	ts, n := parseTimestamp([]byte("1700000000 +0000"))
	assert.Equal(t, uint64(1700000000), ts)
	assert.Equal(t, 10, n)

	_, n = parseTimestamp([]byte(" 1"))
	assert.Zero(t, n)

	assert.False(t, dateOverflows(1700000000))
	assert.True(t, dateOverflows(1<<63))
}

func TestDefaultErrorFunc(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	// The default callback counts an error and stays quiet about a warning.
	assert.Equal(t, 1, DefaultErrorFunc(o, nil, oidN(1), gitobj.TypeBlob, SevError, MsgBadName, "x"))
	assert.Zero(t, DefaultErrorFunc(o, nil, oidN(1), gitobj.TypeBlob, SevWarn, MsgBadName, "x"))
}

func TestCspn(t *testing.T) {
	assert.Equal(t, 3, cspn([]byte("abc<d"), "<>\n"))
	assert.Equal(t, 5, cspn([]byte("abcde"), "<>\n"), "no rejected byte means the whole slice")
	assert.Zero(t, cspn([]byte("<abc"), "<>\n"))
}

func TestValidHexLineAndAfterLine(t *testing.T) {
	require.True(t, validHexLine([]byte(hexA+"\n"), 40))
	assert.False(t, validHexLine([]byte(hexA+"x\n"), 40), "the hash must end the line")
	assert.False(t, validHexLine([]byte("zz\n"), 40))
	assert.False(t, validHexLine([]byte(hexA[:39]+"\n"), 40))

	assert.Equal(t, "next\n", string(afterLine([]byte(hexA+"\nnext\n"), 40)))
	// A malformed line advances by the hex it did hold plus the byte that stopped the scan.
	assert.Equal(t, "next\n", string(afterLine([]byte("abc\nnext\n"), 40)))
	assert.Equal(t, "z\nnext\n", string(afterLine([]byte("zz\nnext\n"), 40)))
}
