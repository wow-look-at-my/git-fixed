package fsck

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckRefnameFormat(t *testing.T) {
	for _, name := range []string{
		"refs/heads/main",
		"refs/tags/v1.0",
		"heads/a/b",
		"refs/heads/a.b.c",
		"refs/heads/{a}",
		"refs/heads/a-b",
	} {
		assert.True(t, CheckRefnameFormat(name, 0), "%q should be a valid reference name", name)
	}
	for _, name := range []string{
		"",
		"@",
		"main",             // a single-level name needs the flag
		"refs/heads/",      // an empty component
		"/refs/heads/main", // a leading separator
		"refs//heads/main", // an empty middle component
		"refs/heads/.hidden",
		"refs/heads/main.",
		"refs/heads/main.lock",
		"refs/heads/ma in",
		"refs/heads/ma~in",
		"refs/heads/ma^in",
		"refs/heads/ma:in",
		"refs/heads/ma?in",
		"refs/heads/ma[in",
		"refs/heads/ma\\in",
		"refs/heads/ma\x7fin",
		"refs/heads/ma\x01in",
		"refs/heads/a..b",
		"refs/heads/a@{b",
		"refs/heads/a*b", // an asterisk needs the pattern flag
	} {
		assert.False(t, CheckRefnameFormat(name, 0), "%q should not be a valid reference name", name)
	}
}

func TestCheckRefnameFormatOnelevel(t *testing.T) {
	assert.True(t, CheckRefnameFormat("main", RefnameAllowOnelevel))
	assert.True(t, CheckRefnameFormat("HEAD", RefnameAllowOnelevel))
	assert.False(t, CheckRefnameFormat("main.lock", RefnameAllowOnelevel))
	assert.False(t, CheckRefnameFormat("@", RefnameAllowOnelevel))
}

func TestCheckRefnameFormatPattern(t *testing.T) {
	assert.True(t, CheckRefnameFormat("refs/heads/*", RefnameRefspecPattern))
	assert.True(t, CheckRefnameFormat("refs/*/main", RefnameRefspecPattern))
	// git allows only a single "*" in a whole pattern.
	assert.False(t, CheckRefnameFormat("refs/*/*", RefnameRefspecPattern))
	assert.False(t, CheckRefnameFormat("refs/heads/*", 0))
}

func TestIsBranchRef(t *testing.T) {
	assert.True(t, IsBranchRef("refs/heads/main"))
	assert.False(t, IsBranchRef("refs/tags/v1"))
	assert.False(t, IsBranchRef("HEAD"))
}
