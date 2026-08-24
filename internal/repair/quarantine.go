// Package repair puts a damaged repository back the way it was.
//
// see docs/repair.md
package repair

// The quarantine. Every removal in this package goes through it, so a repair
// run can be undone in full and no repair can lose data.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// quarantineRoot is where a run's displaced files live.
const quarantineRoot = "git-fixed/quarantine"

// manifestName holds one run's record, written next to the files it displaced.
const manifestName = "manifest.json"

// Displaced records one file the run moved out of the way.
type Displaced struct {
	// From is the path the file had, relative to the git directory.
	From string `json:"from"`
	// To is where it is now, relative to the run's quarantine directory.
	To string `json:"to"`
	// Why says which repair displaced it, for the report and for anyone reading the manifest later.
	Why string `json:"why"`
}

// Manifest is a run's whole record.
type Manifest struct {
	// Run names the run, and is the directory the files live in.
	Run string `json:"run"`
	// Files are every file the run displaced, in the order it displaced them.
	Files []Displaced `json:"files"`
}

// Quarantine holds files a run has taken out of the repository.
type Quarantine struct {
	gitDir string
	run    string

	mu   sync.Mutex
	man  Manifest
	made bool
}

// NewQuarantine prepares a quarantine for one run.
func NewQuarantine(gitDir, run string) *Quarantine {
	return &Quarantine{gitDir: gitDir, run: run, man: Manifest{Run: run}}
}

// Dir is where this run's files go.
func (q *Quarantine) Dir() string {
	return filepath.Join(q.gitDir, filepath.FromSlash(quarantineRoot), q.run)
}

// Files reports what the run has displaced so far.
func (q *Quarantine) Files() []Displaced {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Displaced(nil), q.man.Files...)
}

// Take moves one file out of the repository and records why. path is absolute.
//
// A file that is not there is not an error: two repairs can name the same
// derived file, and the second one has nothing left to do.
func (q *Quarantine) Take(path, why string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	rel, err := q.relative(path)
	if err != nil {
		return err
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureDir(); err != nil {
		return err
	}
	dest := filepath.Join(q.Dir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	if err := move(path, dest); err != nil {
		return fmt.Errorf("quarantining %s: %w", rel, err)
	}
	q.man.Files = append(q.man.Files, Displaced{From: rel, To: rel, Why: why})
	return q.writeManifest()
}

// relative names a path the way the manifest stores it: relative to the git
// directory, with forward slashes.
//
// A worktree file or an alternate object store can sit outside the git
// directory. Those keep their absolute path, prefixed so a restore knows not to
// join it onto anything.
func (q *Quarantine) relative(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(q.gitDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "abs" + filepath.ToSlash(abs), nil
	}
	return filepath.ToSlash(rel), nil
}

// resolve turns a manifest path back into a path on this machine.
func (q *Quarantine) resolve(rel string) string {
	if strings.HasPrefix(rel, "abs/") {
		return filepath.FromSlash(strings.TrimPrefix(rel, "abs"))
	}
	return filepath.Join(q.gitDir, filepath.FromSlash(rel))
}

func (q *Quarantine) ensureDir() error {
	if q.made {
		return nil
	}
	if err := os.MkdirAll(q.Dir(), 0o777); err != nil {
		return err
	}
	q.made = true
	return nil
}

func (q *Quarantine) writeManifest() error {
	data, err := json.MarshalIndent(q.man, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(q.Dir(), manifestName), append(data, '\n'), 0o666)
}

// replacedDir holds what an undo had to move out of the way, inside the run it is undoing.
const replacedDir = "replaced"

// Undo puts a run's files back where they came from.
//
// Most of what a run displaces, it also replaces: it writes a whole index over
// a broken one, a valid packed-refs over a malformed one, a recovered object
// over a corrupt one. So the path an undo restores to is usually occupied, and
// refusing to overwrite it -- which this used to do -- meant the undo failed on
// every run worth undoing.
//
// Nothing is overwritten even so. Whatever is in the way moves into the run's
// own "replaced" directory first, keeping its path, so an undo deletes no more
// than a repair does and can itself be picked apart by hand.
func Undo(gitDir, run string) ([]Displaced, error) {
	q := NewQuarantine(gitDir, run)
	data, err := os.ReadFile(filepath.Join(q.Dir(), manifestName))
	if err != nil {
		return nil, fmt.Errorf("reading the manifest for run %s: %w", run, err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, fmt.Errorf("the manifest for run %s is not readable: %w", run, err)
	}

	var restored []Displaced
	for i := len(man.Files) - 1; i >= 0; i-- {
		f := man.Files[i]
		src := filepath.Join(q.Dir(), filepath.FromSlash(f.To))
		dest := q.resolve(f.From)
		if _, err := os.Lstat(src); err != nil {
			// Already restored by an earlier undo of the same run.
			continue
		}
		if _, err := os.Lstat(dest); err == nil {
			aside := filepath.Join(q.Dir(), replacedDir, filepath.FromSlash(f.To))
			if err := os.MkdirAll(filepath.Dir(aside), 0o777); err != nil {
				return restored, err
			}
			if err := move(dest, aside); err != nil {
				return restored, fmt.Errorf("setting aside the current %s: %w", f.From, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
			return restored, err
		}
		if err := move(src, dest); err != nil {
			return restored, fmt.Errorf("restoring %s: %w", f.From, err)
		}
		restored = append(restored, f)
	}
	return restored, nil
}

// Runs lists the quarantined runs a repository holds, newest name last.
func Runs(gitDir string) []string {
	entries, err := os.ReadDir(filepath.Join(gitDir, filepath.FromSlash(quarantineRoot)))
	if err != nil {
		return nil
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	return runs
}

// move renames a file, falling back to a copy when the two paths are on
// different filesystems. A worktree and its git directory are not always on the
// same one.
func move(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	if err := copyFile(src, dest); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// A loose object is mode 0444, and a copy of one must be writable long enough to write it.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return os.Chmod(dest, info.Mode().Perm())
}

// ReplacedDir is where an undo of this run put what it had to move out of the way.
func ReplacedDir(gitDir, run string) string {
	dir := filepath.Join(NewQuarantine(gitDir, run).Dir(), replacedDir)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}
