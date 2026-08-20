package fsck

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// oidN builds a distinguishable object name without hashing anything.
func oidN(n byte) gitobj.OID {
	var o gitobj.OID
	o.N = 20
	o.H[0] = n
	return o
}

// treeBytes encodes tree entries the way git stores them.
func treeBytes(entries ...[2]string) []byte {
	var b strings.Builder
	for i, e := range entries {
		b.WriteString(e[0] + " " + e[1] + "\x00")
		b.Write(oidN(byte(i + 1)).Raw())
	}
	return []byte(b.String())
}

// collect runs a check and returns the messages it produced, sorted so the
// comparison does not depend on the order the checks fire in.
func collect(t *testing.T, run func(o *Options) int) (int, []string) {
	t.Helper()
	o := NewOptions(gitobj.SHA1)
	var msgs []string
	o.Error = func(_ *Options, _ any, _ gitobj.OID, _ gitobj.Type, sev Severity, _ MsgID, message string) int {
		msgs = append(msgs, sev.String()+": "+message)
		if sev == SevError {
			return 1
		}
		return 0
	}
	ret := run(o)
	sort.Strings(msgs)
	return ret, msgs
}

func TestParseTree(t *testing.T) {
	buf := treeBytes([2]string{"100644", "a"}, [2]string{"40000", "b"}, [2]string{"120000", "c"}, [2]string{"160000", "d"})
	entries, err := ParseTree(buf, gitobj.SHA1)
	require.NoError(t, err)
	require.Len(t, entries, 4)

	assert.Equal(t, "a", entries[0].Name)
	assert.Equal(t, uint32(0o100644), entries[0].Mode)
	assert.True(t, entries[0].IsRegular())
	assert.True(t, entries[1].IsDir())
	assert.True(t, entries[2].IsSymlink())
	assert.True(t, entries[3].IsGitlink())
	assert.Equal(t, oidN(1), entries[0].OID)
	assert.Equal(t, byte('1'), entries[0].Raw[0], "Raw starts at the mode, so a zero pad is visible")
}

func TestParseTreeErrors(t *testing.T) {
	for name, buf := range map[string][]byte{
		"no separator": []byte("100644a\x00" + strings.Repeat("x", 20)),
		"no nul":       []byte("100644 a" + strings.Repeat("x", 20)),
		"short hash":   []byte("100644 a\x00short"),
		"empty name":   []byte("100644 \x00" + strings.Repeat("x", 20)),
		"bad mode":     []byte("10x644 a\x00" + strings.Repeat("x", 20)),
	} {
		_, err := ParseTree(buf, gitobj.SHA1)
		assert.Error(t, err, name)
	}
}

func TestParseTreeStopsAtBadEntry(t *testing.T) {
	buf := append(treeBytes([2]string{"100644", "a"}), []byte("bogus")...)
	entries, err := ParseTree(buf, gitobj.SHA1)
	assert.Error(t, err)
	assert.Len(t, entries, 1, "git reports on the entries it did read")
}

func TestTreeChecks(t *testing.T) {
	for _, c := range []struct {
		name  string
		buf   []byte
		want  string
		isErr bool
	}{
		{"bad mode", treeBytes([2]string{"100640", "a"}), "badFilemode: contains bad file modes", false},
		{"zero padded", treeBytes([2]string{"0100644", "a"}), "zeroPaddedFilemode: contains zero-padded file modes", false},
		{"null sha1", []byte("100644 a\x00" + strings.Repeat("\x00", 20)), "nullSha1: contains entries pointing to null sha1", false},
		{"dot", treeBytes([2]string{"100644", "."}), "hasDot: contains '.'", false},
		{"dotdot", treeBytes([2]string{"100644", ".."}), "hasDotdot: contains '..'", false},
		{"dotgit", treeBytes([2]string{"100644", ".git"}), "hasDotgit: contains '.git'", false},
		{"slash", treeBytes([2]string{"100644", "a/b"}), "fullPathname: contains full pathnames", false},
		{"unsorted", treeBytes([2]string{"100644", "b"}, [2]string{"100644", "a"}), "treeNotSorted: not properly sorted", true},
		{"duplicate", treeBytes([2]string{"100644", "a"}, [2]string{"40000", "a"}), "duplicateEntries: contains duplicate file entries", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ret, msgs := collect(t, func(o *Options) int { return o.Tree(nil, oidN(9), c.buf) })
			joined := strings.Join(msgs, "\n")
			assert.Contains(t, joined, c.want)
			if c.isErr {
				assert.Equal(t, 1, ret)
			}
		})
	}
}

func TestTreeLargePathname(t *testing.T) {
	long := strings.Repeat("x", 5000)
	_, msgs := collect(t, func(o *Options) int {
		return o.Tree(nil, oidN(9), treeBytes([2]string{"100644", long}))
	})
	assert.Contains(t, strings.Join(msgs, "\n"), "largePathname: contains excessively large pathname")
}

func TestTreeRecordsGitmodules(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	o.Error = func(*Options, any, gitobj.OID, gitobj.Type, Severity, MsgID, string) int { return 0 }
	buf := []byte("100644 .gitmodules\x00")
	buf = append(buf, oidN(7).Raw()...)
	o.Tree(nil, oidN(9), buf)
	// The blob is not read yet, so the run must be told to come back to it.
	modules, attrs := o.PendingBlobs()
	assert.Equal(t, []gitobj.OID{oidN(7)}, modules)
	assert.Empty(t, attrs)
}

func TestVerifyOrdered(t *testing.T) {
	// The candidate stack carries over between adjacent pairs, so each case
	// starts from an empty one.
	pair := func(mode1 uint32, name1 string, mode2 uint32, name2 string) int {
		var candidates []string
		return verifyOrdered(mode1, name1, mode2, name2, &candidates)
	}
	assert.Equal(t, treeOrdered, pair(0o100644, "a", 0o100644, "b"))
	assert.Equal(t, treeUnordered, pair(0o100644, "b", 0o100644, "a"))
	assert.Equal(t, treeHasDups, pair(0o100644, "a", 0o40000, "a"))
	// A directory sorts as though its name ended in a slash, so the tree
	// "foo/" comes after the blob "foo.bar", not before it.
	assert.Equal(t, treeOrdered, pair(0o100644, "foo", 0o100644, "foo.bar"))
	assert.Equal(t, treeOrdered, pair(0o100644, "foo.bar", 0o40000, "foo"))
	assert.Equal(t, treeUnordered, pair(0o40000, "foo", 0o100644, "foo.bar"))
}

func TestVerifyOrderedNonAdjacentDuplicate(t *testing.T) {
	// git's own comment names this sequence: the implied slash makes "foo"
	// and "foo/" duplicates even though three entries separate them.
	var candidates []string
	names := [][2]any{
		{uint32(0o100644), "foo"},
		{uint32(0o100644), "foo.bar"},
		{uint32(0o40000), "foo.bar"},
		{uint32(0o40000), "foo"},
	}
	last := treeOrdered
	for i := 1; i < len(names); i++ {
		last = verifyOrdered(names[i-1][0].(uint32), names[i-1][1].(string),
			names[i][0].(uint32), names[i][1].(string), &candidates)
	}
	assert.Equal(t, treeHasDups, last, "%v", candidates)
}

func TestDescribeAndNames(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	oid := oidN(3)
	assert.Equal(t, oid.String(), o.Describe(oid))
	o.PutObjectName(oid, "%s", "HEAD:file")
	assert.Empty(t, o.ObjectName(oid), "names are off until the run asks for them")

	o.EnableObjectNames()
	o.PutObjectName(oid, "%s", "HEAD:file")
	o.PutObjectName(oid, "%s", "other")
	assert.Equal(t, "HEAD:file", o.ObjectName(oid), "the first name seen wins")
	assert.Equal(t, fmt.Sprintf("%s (HEAD:file)", oid), o.Describe(oid))
}

func TestSkiplistSilencesReports(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	calls := 0
	o.Error = func(*Options, any, gitobj.OID, gitobj.Type, Severity, MsgID, string) int {
		calls++
		return 1
	}
	o.AddSkip(oidN(9))
	assert.True(t, o.Skipped(oidN(9)))
	assert.Zero(t, o.Tree(nil, oidN(9), treeBytes([2]string{"100644", ".git"})))
	assert.Zero(t, calls)
}

func TestSeverityTable(t *testing.T) {
	o := NewOptions(gitobj.SHA1)
	assert.Equal(t, SevWarn, o.Severity(MsgZeroPaddedFilemode))
	o.Strict = true
	assert.Equal(t, SevError, o.Severity(MsgZeroPaddedFilemode), "--strict promotes a warning")

	o.SetSeverity(MsgZeroPaddedFilemode, SevIgnore)
	assert.Equal(t, SevIgnore, o.Severity(MsgZeroPaddedFilemode))
	_, msgs := collect(t, func(oo *Options) int {
		oo.SetSeverity(MsgZeroPaddedFilemode, SevIgnore)
		return oo.Tree(nil, oidN(9), treeBytes([2]string{"0100644", "a"}))
	})
	assert.Empty(t, msgs)
}

func TestParseSeverity(t *testing.T) {
	for name, want := range map[string]Severity{
		"ignore": SevIgnore,
		"warn":   SevWarn,
		"error":  SevError,
	} {
		got, ok := ParseSeverity(name)
		require.True(t, ok, name)
		assert.Equal(t, want, got)
		assert.Equal(t, name, got.String())
	}
	_, ok := ParseSeverity("loud")
	assert.False(t, ok)
}

func TestMsgIDByName(t *testing.T) {
	id, ok := MsgIDByName("missingEmail")
	require.True(t, ok)
	assert.Equal(t, MsgMissingEmail, id)
	assert.Equal(t, "missingEmail", id.Name())

	// git folds case and drops underscores.
	id, ok = MsgIDByName("MISSING_EMAIL")
	require.True(t, ok)
	assert.Equal(t, MsgMissingEmail, id)

	_, ok = MsgIDByName("nosuchcheck")
	assert.False(t, ok)
}
