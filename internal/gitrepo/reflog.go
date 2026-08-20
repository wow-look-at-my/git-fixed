package gitrepo

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/go-containers/set"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// ReflogEntry is one line of a reflog.
type ReflogEntry struct {
	Old       gitobj.OID
	New       gitobj.OID
	Timestamp int64
}

// ReflogNames lists every reference that has a log, sorted by name.
func (r *Repo) ReflogNames(worktreeDir string) []string {
	var out []string
	seen := set.New[string]()
	walk := func(root string, perWorktreeOnly bool) {
		logs := filepath.Join(root, "logs")
		_ = filepath.WalkDir(logs, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // an unreadable subtree holds no log
			}
			rel, err := filepath.Rel(logs, path)
			if err != nil {
				return nil
			}
			name := filepath.ToSlash(rel)
			if perWorktreeOnly && !isPerWorktree(name) && name != "HEAD" {
				return nil
			}
			if !perWorktreeOnly && worktreeDir != root && isPerWorktree(name) {
				return nil
			}
			if seen.Add(name) {
				out = append(out, name)
			}
			return nil
		})
	}
	if worktreeDir != "" && worktreeDir != r.CommonDir {
		walk(worktreeDir, true)
	}
	walk(r.CommonDir, false)
	sort.Strings(out)
	return out
}

// Reflog reads one reference's log, oldest entry first.
func (r *Repo) Reflog(worktreeDir, name string) []ReflogEntry {
	path := filepath.Join(r.CommonDir, "logs", filepath.FromSlash(name))
	if worktreeDir != "" && worktreeDir != r.CommonDir && (isPerWorktree(name) || name == "HEAD") {
		path = filepath.Join(worktreeDir, "logs", filepath.FromSlash(name))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ReflogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	hexsz := r.Algo.HexSize
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2*hexsz+1 {
			continue
		}
		old, ok1 := r.Algo.ParseHexBytes([]byte(line))
		newer, ok2 := r.Algo.ParseHexBytes([]byte(line[hexsz+1:]))
		if !ok1 || !ok2 {
			continue
		}
		e := ReflogEntry{Old: old, New: newer}
		// The timestamp follows the committer identity, just before the
		// time zone and the tab that starts the message.
		if tab := strings.IndexByte(line, '\t'); tab > 0 {
			fields := strings.Fields(line[:tab])
			if len(fields) >= 2 {
				if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					e.Timestamp = ts
				}
			}
		}
		out = append(out, e)
	}
	return out
}
