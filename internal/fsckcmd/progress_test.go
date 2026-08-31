package fsckcmd_test

// The progress meter. A run over a large repository is minutes of silence
// without it, which is half of what this was reported for.

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/memwatch"
)

// withProgress runs a check with the meter on and returns what it wrote.
func withProgress(t *testing.T, dir string) (stderr string, code int) {
	t.Helper()
	o := fsckcmd.DefaultOptions()
	var out, errBuf bytes.Buffer
	o.Stdout = &out
	o.Stderr = &errBuf
	o.Dir = dir
	o.Workers = 4
	o.ShowProgress = true
	code = fsckcmd.Run(o)
	return errBuf.String(), code
}

// packedRepo is a repository whose objects are all in a single pack, which is what
// gives the object phase something to count.
func packedRepo(t *testing.T) *gittest.Repo {
	t.Helper()
	r := gittest.New(t)
	for i := range 5 {
		r.Write("f", "revision "+strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "revision "+strconv.Itoa(i))
	}
	r.Git("repack", "-adq")
	return r
}

func TestProgressNamesEveryPhaseGitNames(t *testing.T) {
	gittest.RequireGit(t)
	r := packedRepo(t)
	stderr, code := withProgress(t, r.Dir)
	assert.Equal(t, 0, code, "the repository is sound: %s", stderr)

	// git shows a meter on each phase below without delay.
	for _, want := range []string{
		"Checking ref database: 100% (1/1) ",
		"Checking object directories: 100% (256/256) ",
	} {
		assert.Contains(t, stderr, want)
	}
	assert.Regexp(t, `Checking objects: 100% \(\d+/\d+\) \[\d+[smh][^]]*\], done\.`, stderr)
	assert.NotContains(t, stderr, "Checking connectivity",
		"a phase that beats its delay prints nothing, as git's delayed progress does")
	assert.NotContains(t, stderr, "Verifying reverse pack-indexes")
}

// TestProgressCountsEveryPackedObject keeps the meter honest. A count that does
// not reach the total is a meter that stopped saying anything part way through.
func TestProgressCountsEveryPackedObject(t *testing.T) {
	gittest.RequireGit(t)
	r := packedRepo(t)
	want := strings.TrimSpace(r.Git("count-objects", "-v"))
	inPack := ""
	for _, line := range strings.Split(want, "\n") {
		if rest, ok := strings.CutPrefix(line, "in-pack: "); ok {
			inPack = strings.TrimSpace(rest)
		}
	}
	require.NotEmpty(t, inPack, "the test repository must have a pack")

	stderr, _ := withProgress(t, r.Dir)
	assert.Regexp(t, `Checking objects: 100% \(`+inPack+`/`+inPack+`\) \[[^]]+\], done\.`, stderr)
}

// TestProgressIsOffByDefaultForANonTerminal keeps the meter out of output a
// caller is reading. git decides the same way, from whether stderr is a
// terminal, and the differential tests depend on it: they compare stderr.
func TestProgressIsOffByDefaultForANonTerminal(t *testing.T) {
	gittest.RequireGit(t)
	r := packedRepo(t)
	o := fsckcmd.DefaultOptions()
	var out, errBuf bytes.Buffer
	o.Stdout = &out
	o.Stderr = &errBuf
	o.Dir = r.Dir
	require.Equal(t, 0, fsckcmd.Run(o))
	assert.Empty(t, errBuf.String(), "a run nobody asked for progress from prints none")
}

// TestProgressRedrawsInPlace keeps the meter to a single line while it runs: every
// update but the last returns the cursor rather than ending the line.
func TestProgressRedrawsInPlace(t *testing.T) {
	gittest.RequireGit(t)
	r := packedRepo(t)
	stderr, _ := withProgress(t, r.Dir)
	require.Contains(t, stderr, "Checking ref database")
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "Checking ref database") {
			continue
		}
		// The line ends at the "done." update, and everything before it on
		// that line was written over.
		assert.True(t, strings.HasSuffix(line, ", done."),
			"an update that is not the last must not end its line: %q", line)
	}
}

// TestProgressSaysWhatTheRunCosts covers the field the meters carry that git
// has nothing to copy for. A run over a repository larger than the machine is
// killed part way through, and the last line drawn is the whole of what is
// left to diagnose it by.
func TestProgressSaysWhatTheRunCosts(t *testing.T) {
	gittest.RequireGit(t)
	if _, ok := memwatch.Peak(); !ok {
		t.Skip("this system publishes no memory marks, so no meter carries one")
	}
	r := packedRepo(t)
	stderr, code := withProgress(t, r.Dir)
	assert.Equal(t, 0, code, "the repository is sound: %s", stderr)
	assert.Regexp(t,
		`Checking objects: 100% \(\d+/\d+\) \[\d+[smh][^]]*, peak [\d.]+ (bytes|KiB|MiB|GiB|TiB)\], done\.`,
		stderr, "every meter line says the clock and the high-water mark")
}
