package gitconfig_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitconfig"
)

// flatten renders the entries as "key=value" strings, with a bare key written
// as "key" alone, so a whole parse is one comparison.
func flatten(t *testing.T, src string) []string {
	t.Helper()
	entries, err := gitconfig.Parse([]byte(src))
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Value == nil {
			out = append(out, e.Key)
			continue
		}
		out = append(out, e.Key+"="+*e.Value)
	}
	return out
}

func TestParseSections(t *testing.T) {
	assert.Equal(t, []string{"core.bare=true"}, flatten(t, "[core]\n\tbare = true\n"))
	assert.Equal(t, []string{"core.bare=true"}, flatten(t, "[CORE]\n\tBARE = true\n"), "section and key fold to lower case")
	assert.Equal(t, []string{"submodule.Sub/Dir.url=x"},
		flatten(t, "[submodule \"Sub/Dir\"]\n\turl = x\n"), "a subsection keeps its case")
	assert.Equal(t, []string{"core.sub.key=1"}, flatten(t, "[core.sub]\nkey = 1\n"), "the dotted spelling folds too")
	assert.Equal(t, []string{`a.b"c.k=1`}, flatten(t, "[a \"b\\\"c\"]\nk = 1\n"), "a subsection honours escapes")
}

func TestParseValues(t *testing.T) {
	assert.Equal(t, []string{"a.k="}, flatten(t, "[a]\nk =\n"))
	assert.Equal(t, []string{"a.k"}, flatten(t, "[a]\nk\n"), "a bare key has no value at all")
	assert.Equal(t, []string{"a.k=x y"}, flatten(t, "[a]\nk = x y\n"))
	assert.Equal(t, []string{"a.k=x"}, flatten(t, "[a]\nk = x   \n"), "trailing blanks are dropped")
	assert.Equal(t, []string{"a.k=x  y"}, flatten(t, "[a]\nk = \"x  y\"\n"), "quotes keep blanks")
	assert.Equal(t, []string{"a.k=x"}, flatten(t, "[a]\nk = x # comment\n"))
	assert.Equal(t, []string{"a.k=x"}, flatten(t, "[a]\nk = x ; comment\n"))
	assert.Equal(t, []string{"a.k=x#y"}, flatten(t, "[a]\nk = \"x#y\"\n"), "a quoted comment character is data")
	assert.Equal(t, []string{"a.k=xy"}, flatten(t, "[a]\nk = x\\\ny\n"), "a backslash newline continues the value")
	assert.Equal(t, []string{"a.k=\t\n\b\"\\"}, flatten(t, `[a]`+"\nk = \\t\\n\\b\\\"\\\\\n"))
	assert.Equal(t, []string{"a.k=1", "a.j=2"}, flatten(t, "[a]\nk=1\nj=2\n"))
	assert.Equal(t, []string{"a.k=1"}, flatten(t, "[a]\nk=1"), "a file may end without a newline")
}

func TestParseCommentsAndBlanks(t *testing.T) {
	src := "# leading\n; also leading\n\n[a]\n\n  k = 1\n#trailing\n"
	assert.Equal(t, []string{"a.k=1"}, flatten(t, src))
}

func TestParseLineNumbers(t *testing.T) {
	entries, err := gitconfig.Parse([]byte("[a]\nk = 1\n\nj = 2\n"))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, 2, entries[0].Line)
	assert.Equal(t, 4, entries[1].Line)
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"key outside a section":     "k = 1\n",
		"key does not start":        "[a]\n1k = 1\n",
		"unterminated section":      "[a\n",
		"empty section name":        "[]\n",
		"bad section header":        "[a!]\n",
		"unterminated subsection":   "[a \"b\n",
		"bad after subsection":      "[a \"b\" c]\n",
		"missing equals":            "[a]\nk ! 1\n",
		"unterminated quoted value": "[a]\nk = \"x\n",
		"bad escape":                "[a]\nk = \\q\n",
	} {
		_, err := gitconfig.Parse([]byte(src))
		require.Error(t, err, name)
		assert.ErrorIs(t, err, gitconfig.ErrParse, name)
	}
}

func TestForEach(t *testing.T) {
	var seen []string
	err := gitconfig.ForEach([]byte("[a]\nk = 1\nj\n"), func(key string, value *string) error {
		if value == nil {
			seen = append(seen, key)
			return nil
		}
		seen = append(seen, key+"="+*value)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.k=1", "a.j"}, seen)

	// A callback error stops the walk, and a parse error still surfaces after the entries that did parse.
	stop := errors.New("stop")
	err = gitconfig.ForEach([]byte("[a]\nk = 1\n"), func(string, *string) error { return stop })
	assert.ErrorIs(t, err, stop)

	err = gitconfig.ForEach([]byte("[a]\nk = 1\n!bad\n"), func(string, *string) error { return nil })
	assert.ErrorIs(t, err, gitconfig.ErrParse)
}

func TestSplitKey(t *testing.T) {
	section, sub, name, hasSub := gitconfig.SplitKey("submodule.sub/dir.url")
	assert.Equal(t, "submodule", section)
	assert.Equal(t, "sub/dir", sub)
	assert.Equal(t, "url", name)
	assert.True(t, hasSub)

	section, sub, name, hasSub = gitconfig.SplitKey("core.bare")
	assert.Equal(t, "core", section)
	assert.Empty(t, sub)
	assert.Equal(t, "bare", name)
	assert.False(t, hasSub)

	section, sub, name, hasSub = gitconfig.SplitKey("core")
	assert.Equal(t, "core", section)
	assert.Empty(t, sub)
	assert.Empty(t, name)
	assert.False(t, hasSub)

	// A subsection may itself contain dots; only the last one splits off the name.
	_, sub, name, hasSub = gitconfig.SplitKey("submodule.a.b.c.url")
	assert.Equal(t, "a.b.c", sub)
	assert.Equal(t, "url", name)
	assert.True(t, hasSub)
}
