package gittest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseGitVersion covers the shapes a git build puts in its version line.
// The differential gate depends on reading this correctly: a version this
// parser refuses is a version RequireGit will not run against.
func TestParseGitVersion(t *testing.T) {
	for _, c := range []struct {
		line string
		want gitVersion
		ok   bool
	}{
		{"git version 2.55.0", gitVersion{2, 55, 0}, true},
		{"git version 2.43.0", gitVersion{2, 43, 0}, true},
		{"git version 2.39.5 (Apple Git-154)", gitVersion{2, 39, 5}, true},
		{"git version 2.56.0.rc1", gitVersion{2, 56, 0}, true},
		{"git version 2.55.0.windows.1", gitVersion{2, 55, 0}, true},
		{"git version 2.55", gitVersion{2, 55, 0}, true},
		{"", gitVersion{}, false},
		{"git version", gitVersion{}, false},
		{"hg version 2.55.0", gitVersion{}, false},
		{"git version banana", gitVersion{}, false},
	} {
		t.Run(c.line, func(t *testing.T) {
			got, ok := parseGitVersion(c.line)
			require.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestGitVersionLess covers the comparison RequireGit rejects an old git with.
func TestGitVersionLess(t *testing.T) {
	assert.True(t, gitVersion{2, 43, 0}.Less(MinGit))
	assert.True(t, gitVersion{2, 54, 9}.Less([3]int{2, 55, 0}))
	assert.False(t, gitVersion{2, 55, 0}.Less(MinGit))
	assert.False(t, gitVersion{2, 55, 1}.Less(MinGit))
	assert.False(t, gitVersion{3, 0, 0}.Less(MinGit))
}
