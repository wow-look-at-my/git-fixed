package fsck

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

func TestCheckSubmoduleName(t *testing.T) {
	for _, name := range []string{"sub", "a/b", "..a", "a..", "a/..b", "dotdot.."} {
		assert.NoError(t, checkSubmoduleName(name), "%q is an ordinary submodule name", name)
	}
	for _, name := range []string{"", "..", "../x", "a/..", "a/../b", "..\\x", "a\\..\\b"} {
		assert.Error(t, checkSubmoduleName(name), "%q escapes the modules directory", name)
	}
}

func TestCheckSubmoduleURL(t *testing.T) {
	for _, u := range []string{
		"https://example.com/repo.git",
		"http://user@example.com/repo.git",
		"http://user:pass@example.com/repo.git",
		"git@example.com:repo.git",
		"./sub",
		"../sub",
		"git://example.com/repo.git",
		"ssh://example.com/repo.git",
		"https::https://example.com/repo.git",
	} {
		assert.NoError(t, checkSubmoduleURL(u), "%q is an ordinary submodule url", u)
	}
	for _, u := range []string{
		"--upload-pack=sh",
		"-u./payload",
		"./sub\nmore",
		"../%0a/sub",  // a newline smuggled through percent-encoding
		"../../:22/x", // escapes past its own root, the submodule URL exploit git's fsck rejects
		"..//host/x",
		"https://exam\nple.com/repo.git",
		"https://",
	} {
		assert.Error(t, checkSubmoduleURL(u), "%q should be refused", u)
	}
}

func TestCountLeadingDotdots(t *testing.T) {
	n, rest := countLeadingDotdots("../../a")
	assert.Equal(t, 2, n)
	assert.Equal(t, "a", rest)

	n, rest = countLeadingDotdots("./a")
	assert.Zero(t, n)
	assert.Equal(t, "a", rest)

	n, rest = countLeadingDotdots(".\\..\\a")
	assert.Equal(t, 1, n, "a backslash separates too")
	assert.Equal(t, "a", rest)

	n, rest = countLeadingDotdots("a/b")
	assert.Zero(t, n)
	assert.Equal(t, "a/b", rest)
}

func TestCredentialHostFromURL(t *testing.T) {
	for _, c := range []struct{ url, host string }{
		{"https://example.com/repo.git", "example.com"},
		{"https://example.com", "example.com"},
		{"https://user@example.com/x", "example.com"},
		{"https://user:pass@example.com:443/x", "example.com:443"},
		{"https://exam%70le.com/x", "example.com"},
		{"https://example.com?q=1", "example.com"},
	} {
		host, err := credentialHostFromURL(c.url)
		require.NoError(t, err, c.url)
		assert.Equal(t, c.host, host, c.url)
	}
	for _, u := range []string{"example.com/x", "://example.com", "https://exam\nple.com/x"} {
		_, err := credentialHostFromURL(u)
		assert.Error(t, err, u)
	}
}

func TestURLDecode(t *testing.T) {
	assert.Equal(t, "plain", urlDecode("plain"))
	assert.Equal(t, "a b", urlDecode("a%20b"))
	assert.Equal(t, "a\nb", urlDecode("a%0Ab"))
	// git leaves an invalid escape exactly as it found it.
	assert.Equal(t, "a%zzb", urlDecode("a%zzb"))
	assert.Equal(t, "a%2", urlDecode("a%2"))
}

func TestURLToCurlURL(t *testing.T) {
	for _, c := range []struct {
		in, out string
		ok      bool
	}{
		{"http://a/b", "http://a/b", true},
		{"https://a/b", "https://a/b", true},
		{"ftp://a/b", "ftp://a/b", true},
		{"ftps::https://a/b", "https://a/b", true},
		{"ssh://a/b", "", false},
		{"a@b:c", "", false},
	} {
		out, ok := urlToCurlURL(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		assert.Equal(t, c.out, out, c.in)
	}
}

// gitmodulesBlob names a blob as .gitmodules and then checks it, mirroring how
// git links a tree entry to the blob it points at before checking that blob.
func gitmodulesBlob(t *testing.T, content string) []string {
	t.Helper()
	oid := oidN(7)
	var msgs []string
	o := NewOptions(gitobj.SHA1)
	o.Error = func(_ *Options, _ any, _ gitobj.OID, _ gitobj.Type, _ Severity, _ MsgID, message string) int {
		msgs = append(msgs, message)
		return 1
	}
	o.foundGitmodules(oid)
	o.Blob(nil, oid, []byte(content))
	return msgs
}

func TestBlobGitmodules(t *testing.T) {
	assert.Empty(t, gitmodulesBlob(t, "[submodule \"a\"]\n\turl = https://example.com/a\n\tpath = a\n"))

	assert.Contains(t, gitmodulesBlob(t, "[submodule \"../evil\"]\n\turl = https://e/a\n")[0],
		"disallowed submodule name: ../evil")
	assert.Contains(t, gitmodulesBlob(t, "[submodule \"a\"]\n\turl = --upload-pack=sh\n")[0],
		"disallowed submodule url: --upload-pack=sh")
	assert.Contains(t, gitmodulesBlob(t, "[submodule \"a\"]\n\tpath = --x\n")[0],
		"disallowed submodule path: --x")
	assert.Contains(t, gitmodulesBlob(t, "[submodule \"a\"]\n\tupdate = !sh\n")[0],
		"disallowed submodule update setting: !sh")
	assert.Contains(t, gitmodulesBlob(t, "[submodule \"a\"]\n\turl\n!bad\n")[0],
		"could not parse gitmodules blob")

	// A setting outside the submodule section is not fsck's business.
	assert.Empty(t, gitmodulesBlob(t, "[core]\n\tbare = --x\n"))
	// Nor is a submodule section with no subsection name.
	assert.Empty(t, gitmodulesBlob(t, "[submodule]\n\turl = --x\n"))
}

func TestBlobGitmodulesTooLarge(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	var msgs []string
	o.Error = func(_ *Options, _ any, _ gitobj.OID, _ gitobj.Type, _ Severity, _ MsgID, message string) int {
		msgs = append(msgs, message)
		return 1
	}
	o.foundGitmodules(oidN(7))
	assert.Equal(t, 1, o.Blob(nil, oidN(7), nil))
	assert.Equal(t, []string{"gitmodulesLarge: .gitmodules too large to parse"}, msgs)
}

func TestBlobGitattributes(t *testing.T) {
	check := func(content string) []string {
		var msgs []string
		o := NewOptions(gitobj.SHA1)
		o.Error = func(_ *Options, _ any, _ gitobj.OID, _ gitobj.Type, _ Severity, _ MsgID, message string) int {
			msgs = append(msgs, message)
			return 1
		}
		o.foundGitattributes(oidN(8))
		o.Blob(nil, oidN(8), []byte(content))
		return msgs
	}
	assert.Empty(t, check("*.go text\n*.png binary\n"))
	assert.Empty(t, check("*.go text"), "a file may end without a newline")

	long := strings.Repeat("x", attrMaxLineLength) + "\n"
	assert.Contains(t, check(long)[0], ".gitattributes has too long lines to parse")
	assert.Contains(t, check("ok\n" + long)[0], ".gitattributes has too long lines to parse")
	// git stops reading at a NUL, so a long line after it does not count.
	assert.Empty(t, check("ok\n\x00"+long))
}

func TestReportMissingAndNonBlob(t *testing.T) {
	for _, c := range []struct{ kind, missing, nonBlob string }{
		{".gitmodules", "gitmodulesMissing: unable to read .gitmodules blob", "gitmodulesBlob: non-blob found at .gitmodules"},
		{".gitattributes", "gitattributesMissing: unable to read .gitattributes blob", "gitattributesBlob: non-blob found at .gitattributes"},
	} {
		_, msgs := collect(t, func(o *Options) int { return o.ReportMissingBlob(nil, oidN(1), c.kind) })
		assert.Contains(t, first(msgs), c.missing)

		_, msgs = collect(t, func(o *Options) int {
			return o.ReportNonBlob(nil, oidN(1), gitobj.TypeTree, c.kind)
		})
		assert.Contains(t, first(msgs), c.nonBlob)
	}
}

func TestBlobIgnoresUnnamedBlobs(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	o.Error = func(*Options, any, gitobj.OID, gitobj.Type, Severity, MsgID, string) int {
		t.Fatal("a blob no tree named as .gitmodules has nothing to check")
		return 1
	}
	assert.Zero(t, o.Blob(nil, oidN(7), []byte("[submodule \"../evil\"]\n\turl = --x\n")))
}
