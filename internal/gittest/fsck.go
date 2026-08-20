package gittest

import (
	"bytes"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// Result is one fsck run's output and exit status.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Lines returns every output line from both streams, sorted. A parallel run
// finishes objects in a different order from git's single-threaded walk, and
// git's own order depends on readdir and on an internal hash table, so the set
// of lines is the part that has to agree.
func (r Result) Lines() []string {
	var out []string
	for _, s := range []string{r.Stdout, r.Stderr} {
		for _, line := range strings.Split(s, "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
	}
	sort.Strings(out)
	return out
}

// GitFsck runs the system git's fsck in the repository.
func (r *Repo) GitFsck(args ...string) Result {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"fsck"}, args...)...)
	cmd.Dir = r.Dir
	cmd.Env = Env()
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		r.t.Fatalf("running git fsck: %v", err)
	}
	return Result{Stdout: out.String(), Stderr: errBuf.String(), Code: code}
}

// RequireGit fails the test when the system git is missing, because a
// comparison against it is the whole point of these tests. It also records the
// version, because git changes the wording of a message between releases and a
// failure here is usually the first sign of it.
func RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("these tests compare against the system git, which is not installed: %v", err)
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatalf("running git --version: %v", err)
	}
	t.Logf("comparing against %s", strings.TrimSpace(string(out)))
}
