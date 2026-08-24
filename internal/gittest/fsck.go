package gittest

import (
	"bytes"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Result is one fsck run's output and exit status.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Lines returns every output line from both streams, sorted.
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

// MinGit is the oldest git these tests can compare against.
var MinGit = [3]int{2, 55, 0}

// RequireGit fails the test when the system git is missing or too old, because
// a comparison against it is the whole point of these tests. A skip would leave
// a green run that checked nothing.
func RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("these tests compare against the system git, which is not installed: %v", err)
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatalf("running git --version: %v", err)
	}
	line := strings.TrimSpace(string(out))
	version, ok := parseGitVersion(line)
	if !ok {
		t.Fatalf("cannot read a version out of %q", line)
	}
	if version.Less(MinGit) {
		t.Fatalf("these tests compare against git %d.%d.%d or newer, and this is %q.\n"+
			"On Ubuntu: sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git",
			MinGit[0], MinGit[1], MinGit[2], line)
	}
	t.Logf("comparing against %s", line)
}

// gitVersion is a release, as its three leading numbers.
type gitVersion [3]int

// Less reports whether v comes before other.
func (v gitVersion) Less(other [3]int) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return false
}

// parseGitVersion reads the numbers out of a "git version 2.55.0" line. A build
// may add its own text after them, such as "(Apple Git-154)", and a release
// candidate may add a suffix to a number, so each field stops at its first
// non-digit.
func parseGitVersion(line string) (gitVersion, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return gitVersion{}, false
	}
	var v gitVersion
	for i, part := range strings.SplitN(fields[2], ".", 3) {
		if i >= len(v) {
			break
		}
		digits := part
		if cut := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); cut >= 0 {
			digits = part[:cut]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return gitVersion{}, false
		}
		v[i] = n
	}
	return v, true
}
