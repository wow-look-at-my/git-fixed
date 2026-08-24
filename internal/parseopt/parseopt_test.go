package parseopt_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/parseopt"
)

// testSet builds an option table shaped like fsck's own.
func testSet() (*parseopt.Set, map[string]*int) {
	v := map[string]*int{}
	for _, name := range []string{"verbose", "unreachable", "dangling", "cache", "connectivity-only"} {
		n := 0
		v[name] = &n
	}
	return &parseopt.Set{
		Usage: []string{"git fsck [<options>] [<object>...]"},
		Opts: []*parseopt.Bool{
			{Short: 'v', Long: "verbose", Help: "be chatty", Value: v["verbose"]},
			{Long: "unreachable", Help: "show unreachable objects", Value: v["unreachable"]},
			{Long: "dangling", Help: "show dangling objects", Value: v["dangling"]},
			{Long: "cache", Help: "make index objects head nodes", Value: v["cache"]},
			{Long: "connectivity-only", Help: "check only connectivity", Value: v["connectivity-only"]},
		},
	}, v
}

func TestParseLongOptions(t *testing.T) {
	s, v := testSet()
	rest, err := s.Parse([]string{"--verbose", "--dangling"})
	require.NoError(t, err)
	assert.Empty(t, rest)
	assert.Equal(t, 1, *v["verbose"])
	assert.Equal(t, 1, *v["dangling"])
	assert.Equal(t, 0, *v["cache"])
}

func TestParseNegatedAndValued(t *testing.T) {
	s, v := testSet()
	_, err := s.Parse([]string{"--verbose", "--no-verbose"})
	require.NoError(t, err)
	assert.Equal(t, 0, *v["verbose"])

	s, v = testSet()
	_, err = s.Parse([]string{"--dangling=false"})
	require.NoError(t, err)
	assert.Equal(t, 0, *v["dangling"])

	// A --no- form with an explicit value inverts twice.
	s, v = testSet()
	_, err = s.Parse([]string{"--no-dangling=no"})
	require.NoError(t, err)
	assert.Equal(t, 1, *v["dangling"])

	s, _ = testSet()
	_, err = s.Parse([]string{"--dangling=maybe"})
	var usage parseopt.ErrUsage
	require.ErrorAs(t, err, &usage)
	assert.Equal(t, "option `dangling' takes no value", usage.Msg)
}

func TestParseAbbreviation(t *testing.T) {
	s, v := testSet()
	_, err := s.Parse([]string{"--unre"})
	require.NoError(t, err)
	assert.Equal(t, 1, *v["unreachable"])

	// "c" abbreviates both --cache and --connectivity-only.
	s, _ = testSet()
	_, err = s.Parse([]string{"--c"})
	var usage parseopt.ErrUsage
	require.ErrorAs(t, err, &usage)
	assert.Equal(t, "unknown option `c'", usage.Msg)

	s, v = testSet()
	_, err = s.Parse([]string{"--conn"})
	require.NoError(t, err)
	assert.Equal(t, 1, *v["connectivity-only"])
}

func TestParseShortOptions(t *testing.T) {
	s, v := testSet()
	_, err := s.Parse([]string{"-v"})
	require.NoError(t, err)
	assert.Equal(t, 1, *v["verbose"])

	s, _ = testSet()
	_, err = s.Parse([]string{"-x"})
	var usage parseopt.ErrUsage
	require.ErrorAs(t, err, &usage)
	assert.Equal(t, "unknown switch `x'", usage.Msg)
	assert.Equal(t, "unknown switch `x'", usage.Error())
}

func TestParseHelp(t *testing.T) {
	s, _ := testSet()
	for _, arg := range []string{"-h", "--help"} {
		_, err := s.Parse([]string{arg})
		var help parseopt.ErrHelp
		require.ErrorAs(t, err, &help)
		assert.Equal(t, "usage requested", help.Error())
	}
}

func TestParseStopsAtArguments(t *testing.T) {
	s, v := testSet()
	rest, err := s.Parse([]string{"-v", "HEAD", "--dangling"})
	require.NoError(t, err)
	assert.Equal(t, []string{"HEAD", "--dangling"}, rest)
	assert.Equal(t, 1, *v["verbose"])
	assert.Equal(t, 0, *v["dangling"], "an argument ends option parsing")
}

func TestParseDoubleDash(t *testing.T) {
	s, v := testSet()
	rest, err := s.Parse([]string{"-v", "--", "--dangling", "HEAD"})
	require.NoError(t, err)
	assert.Equal(t, []string{"--dangling", "HEAD"}, rest)
	assert.Equal(t, 0, *v["dangling"])

	// A lone "-" is an argument, not an option.
	s, _ = testSet()
	rest, err = s.Parse([]string{"-"})
	require.NoError(t, err)
	assert.Equal(t, []string{"-"}, rest)
}

func TestPrintUsage(t *testing.T) {
	s, _ := testSet()
	s.Usage = append(s.Usage, "git fsck --connectivity-only")
	var buf bytes.Buffer
	s.PrintUsage(&buf)
	out := buf.String()
	assert.True(t, strings.HasPrefix(out, "usage: git fsck [<options>] [<object>...]\n"))
	assert.Contains(t, out, "   or: git fsck --connectivity-only\n")
	assert.Contains(t, out, "    -v, --[no-]verbose")
	// A long name past the column width wraps onto its own line, as git's usage_with_options() does.
	assert.Contains(t, out, "--[no-]connectivity-only\n"+strings.Repeat(" ", 26)+"check only connectivity")
}
