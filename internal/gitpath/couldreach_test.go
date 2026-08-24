package gitpath

// The two-byte filter in front of every check here is an assertion about all four filesystems at once.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reaches asks every check, with no filter in front of any of them. It is
// matches with the first line taken out, and ".git" keeps its own NTFS check
// for the same reason matches gives it one: there is no 8.3 short name for a
// name that is already that short.
func reaches(name []byte, n needle) bool {
	if n == dotGit {
		return isHFSDotGeneric(name, n) || isNTFSDotGit(name) ||
			isExt4DotGeneric(name, n) || isZFSDotGeneric(name, n)
	}
	return isHFSDotGeneric(name, n) || isNTFSDotGeneric(name, n) ||
		isExt4DotGeneric(name, n) || isZFSDotGeneric(name, n)
}

// everyNeedle is each control name, so a sweep asks about both 8.3 prefixes that begin differently as well as.
var everyNeedle = []needle{dotGit, dotGitmodules, dotGitignore, dotGitattrs, dotMailmap}

// TestTheFilterRulesOutNothingTheChecksAccept sweeps both bytes the filter
// looks at, against every tail that could finish one of these names.
func TestTheFilterRulesOutNothingTheChecksAccept(t *testing.T) {
	tails := []string{
		"", "t", "t~1", "t~1.", "tmodules", "ilmap", "7eba~1", "ba30~1",
		"‌t", "tſ", ":x",
	}
	for first := range 256 {
		for second := range 256 {
			for _, tail := range tails {
				name := append([]byte{byte(first), byte(second)}, tail...)
				for _, n := range everyNeedle {
					if reaches(name, n) && !couldReach(name) {
						require.Fail(t, "the filter threw away a name that reaches a control name",
							"%q reaches %q", name, string(n))
					}
				}
			}
		}
	}
}

// TestTheFilterRulesOutNothingOnAShortName covers the lengths the sweep above
// cannot reach, where the filter has fewer bytes to look at than it wants.
func TestTheFilterRulesOutNothingOnAShortName(t *testing.T) {
	for first := range 256 {
		for _, name := range [][]byte{{}, {byte(first)}} {
			for _, n := range everyNeedle {
				if reaches(name, n) && !couldReach(name) {
					require.Fail(t, "the filter threw away a short name",
						"%q reaches %q", name, string(n))
				}
			}
		}
	}
}

// TestAnOrdinaryNameIsRuledOut is the other half: the filter has to say no to
// nearly everything, or it saves nothing.
func TestAnOrdinaryNameIsRuledOut(t *testing.T) {
	// "Makefile" is not here, and neither is "main.go": the filter looks at
	// two bytes and "ma" begins ".mailmap" as well as both of those.
	for _, name := range []string{
		"README", "src", "LICENSE", "index.html", "a.c", "go.mod", "model.py", "vendor",
	} {
		assert.False(t, couldReach([]byte(name)), "%q", name)
	}
	for _, name := range []string{
		".git", "git~1", "GIT~1", "maba30~1", "．git", "~1234567", "gi7eba~1",
	} {
		assert.True(t, couldReach([]byte(name)), "%q", name)
	}
}
