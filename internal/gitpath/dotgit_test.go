package gitpath_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wow-look-at-my/git-fixed/internal/gitpath"
)

func TestIsDotGit(t *testing.T) {
	for _, name := range []string{
		".git",
		".GIT",
		".Git",
		".git/",
		".git\\",
		"git~1",
		"GIT~1",
		".git ",
		".git.",
		".git . . ",
		".gi\u200ct", // HFS+ drops a zero width non-joiner
		".g\ufeffit", // and a byte order mark
		".\uff47it",  // ZFS folds a fullwidth g to "g"
		"\uff0egit",  // and a fullwidth stop to "."
	} {
		assert.True(t, gitpath.IsDotGit(name), "%q should reach .git", name)
	}
	for _, name := range []string{
		"",
		".",
		".gi",
		".gitx",
		"git",
		"git~2",
		"dotgit",
		".git x",
		".git\xff",
		"\xff.git",
	} {
		assert.False(t, gitpath.IsDotGit(name), "%q should not reach .git", name)
	}
}

func TestIsDotGitmodules(t *testing.T) {
	for _, name := range []string{
		".gitmodules",
		".GITMODULES",
		".gitmodules ",
		".gitmodules.",
		".gitmodules:",
		"gitmod~1",
		"GITMOD~4",
		"gi7eba~1",         // the fall-back 8.3 short name
		"gi7eba~9",         // any digit, as git allows
		".gitmodule\u017f", // ext4 casefold: the long s folds to "s"
	} {
		assert.True(t, gitpath.IsDotGitmodules(name), "%q should reach .gitmodules", name)
	}
	for _, name := range []string{
		"",
		".gitmodule",
		".gitmodulesx",
		"gitmodules",
		"gitmod~5",
		"gi7eba~0",
		"gi7ebb~1",
	} {
		assert.False(t, gitpath.IsDotGitmodules(name), "%q should not reach .gitmodules", name)
	}
}

func TestIsOtherControlNames(t *testing.T) {
	assert.True(t, gitpath.IsDotGitignore(".gitignore"))
	assert.True(t, gitpath.IsDotGitignore("gi250a~1"))
	assert.False(t, gitpath.IsDotGitignore(".gitignor"))

	assert.True(t, gitpath.IsDotGitattributes(".gitattributes"))
	assert.True(t, gitpath.IsDotGitattributes("gi7d29~1"))
	assert.False(t, gitpath.IsDotGitattributes(".gitattribute"))

	assert.True(t, gitpath.IsDotMailmap(".mailmap"))
	assert.True(t, gitpath.IsDotMailmap("maba30~1"))
	assert.False(t, gitpath.IsDotMailmap(".mailma"))
}

func TestNTFSOnlyChecks(t *testing.T) {
	// The tree check applies these to each backslash-separated segment, so
	// they must answer for the NTFS spelling alone.
	assert.True(t, gitpath.IsNTFSDotGit(".git"))
	assert.True(t, gitpath.IsNTFSDotGit("git~1"))
	assert.True(t, gitpath.IsNTFSDotGit(".git:"))
	assert.False(t, gitpath.IsNTFSDotGit(".gi\u200ct"), "the HFS spelling is not the NTFS one")
	assert.False(t, gitpath.IsNTFSDotGit("gits"))

	assert.True(t, gitpath.IsNTFSDotGitmodules(".gitmodules"))
	assert.True(t, gitpath.IsNTFSDotGitmodules("gi7eba~1"))
	assert.False(t, gitpath.IsNTFSDotGitmodules(".gitmodule\u017f"))
}
