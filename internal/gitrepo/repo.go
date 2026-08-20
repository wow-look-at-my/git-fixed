// Package gitrepo finds a repository and reads the parts of it fsck needs:
// configuration, references, reflogs, the index, and linked worktrees.
package gitrepo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitconfig"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Repo is an open repository.
type Repo struct {
	// GitDir is the repository's own directory, which is the common
	// directory for the main worktree.
	GitDir string
	// CommonDir holds refs, objects, and configuration. A linked worktree
	// has its own GitDir but shares this.
	CommonDir string
	// WorkTree is the checkout, empty for a bare repository.
	WorkTree string
	// ObjectsDir is where loose objects and packs live.
	ObjectsDir string
	// DisplayGitDir and DisplayObjectsDir name the same directories the way
	// git prints them. git works from the top of the worktree, so it writes
	// ".git/objects/aa/bb..." where this process holds an absolute path.
	DisplayGitDir     string
	DisplayObjectsDir string
	// Algo is the repository's object hash.
	Algo *gitobj.Algo
	// Config is every setting in effect, later entries winning.
	Config *Config

	// packed caches the packed reference table, read at most once.
	packedOnce sync.Once
	packed     map[string]gitobj.OID
}

// ErrNotARepo is returned when no repository contains the starting directory.
var ErrNotARepo = errors.New("not a git repository")

// Open finds the repository that contains dir, honouring the same environment
// variables git does.
func Open(dir string) (*Repo, error) {
	gitDir, shown, err := discover(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{GitDir: gitDir, CommonDir: gitDir, DisplayGitDir: shown}
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		p := strings.TrimSpace(string(common))
		if !filepath.IsAbs(p) {
			p = filepath.Join(gitDir, p)
		}
		r.CommonDir = filepath.Clean(p)
	}
	r.ObjectsDir = filepath.Join(r.CommonDir, "objects")
	r.DisplayObjectsDir = filepath.Join(r.DisplayGitDir, "objects")
	if v := os.Getenv("GIT_OBJECT_DIRECTORY"); v != "" {
		r.ObjectsDir = v
		r.DisplayObjectsDir = v
	}
	r.Config, err = loadConfig(r.CommonDir)
	if err != nil {
		return nil, err
	}
	r.Algo = gitobj.SHA1
	if name, ok := r.Config.Get("extensions.objectformat"); ok {
		algo := gitobj.AlgoByName(strings.ToLower(name))
		if algo == nil {
			return nil, fmt.Errorf("unknown repository hash algorithm '%s'", name)
		}
		r.Algo = algo
	}
	r.WorkTree = worktreeOf(gitDir)
	return r, nil
}

// discover walks up from dir looking for a repository, the way git's
// setup_git_directory() does. It returns the directory to read from and the
// name git would print for it: git changes to the top of the worktree first, so
// it names the repository ".git" or "." rather than by an absolute path.
func discover(dir string) (path, shown string, err error) {
	if v := os.Getenv("GIT_DIR"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", "", err
		}
		return abs, v, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for {
		candidate := filepath.Join(abs, ".git")
		st, err := os.Stat(candidate)
		switch {
		case err == nil && st.IsDir():
			return candidate, ".git", nil
		case err == nil && st.Mode().IsRegular():
			target, err := readGitFile(candidate)
			if err != nil {
				return "", "", err
			}
			return target, target, nil
		}
		// A bare repository is recognised by its own contents.
		if isGitDir(abs) {
			return abs, ".", nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", "", ErrNotARepo
		}
		abs = parent
	}
}

// readGitFile follows a ".git" file, which a linked worktree or a submodule
// leaves in place of a directory.
func readGitFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", fmt.Errorf("invalid gitfile format: %s", path)
	}
	target := strings.TrimSpace(rest)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), nil
}

func isGitDir(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, "objects")); err != nil || !st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, "refs")); err != nil || !st.IsDir() {
		return false
	}
	head, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(head))
	return strings.HasPrefix(s, "ref:") || len(s) == 40 || len(s) == 64
}

// worktreeOf returns the checkout that owns a .git directory, or "" when the
// repository is bare.
func worktreeOf(gitDir string) string {
	if v := os.Getenv("GIT_WORK_TREE"); v != "" {
		return v
	}
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir)
	}
	return ""
}

// Config is a flattened view of every configuration file that applies.
type Config struct {
	entries []gitconfig.Entry
	byKey   map[string][]string
}

// Get returns the last value set for a key.
func (c *Config) Get(key string) (string, bool) {
	v, ok := c.byKey[strings.ToLower(key)]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// GetAll returns every value set for a key, in the order they were read.
func (c *Config) GetAll(key string) []string { return c.byKey[strings.ToLower(key)] }

// Entries returns every setting in reading order.
func (c *Config) Entries() []gitconfig.Entry { return c.entries }

// Bool reads a boolean setting, using git's rules for what counts as true.
func (c *Config) Bool(key string, def bool) bool {
	v, ok := c.Get(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "", "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	if n, err := strconv.ParseInt(v, 0, 64); err == nil {
		return n != 0
	}
	return def
}

// Int reads a setting that may carry a k, m, or g suffix.
func (c *Config) Int(key string, def int64) int64 {
	v, ok := c.Get(key)
	if !ok || v == "" {
		return def
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'k', 'K':
		mult, v = 1024, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1024*1024, v[:len(v)-1]
	case 'g', 'G':
		mult, v = 1024*1024*1024, v[:len(v)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def
	}
	return n * mult
}

// loadConfig reads the system, global, and repository files in git's order, so
// that the repository's own settings win.
func loadConfig(commonDir string) (*Config, error) {
	c := &Config{byKey: map[string][]string{}}
	var paths []string
	if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		paths = append(paths, "/etc/gitconfig")
	}
	if v := os.Getenv("GIT_CONFIG_GLOBAL"); v != "" {
		paths = append(paths, v)
	} else {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			if home, err := os.UserHomeDir(); err == nil {
				xdg = filepath.Join(home, ".config")
			}
		}
		if xdg != "" {
			paths = append(paths, filepath.Join(xdg, "git", "config"))
		}
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, ".gitconfig"))
		}
	}
	if v := os.Getenv("GIT_CONFIG_SYSTEM"); v != "" {
		paths = append(paths, v)
	}
	paths = append(paths, filepath.Join(commonDir, "config"))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		entries, err := gitconfig.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("bad config line in file %s", p)
		}
		for _, e := range entries {
			c.entries = append(c.entries, e)
			value := ""
			if e.Value != nil {
				value = *e.Value
			}
			key := strings.ToLower(e.Key)
			c.byKey[key] = append(c.byKey[key], value)
		}
	}
	// Environment overrides come last so they win, as git's do.
	for i := 0; ; i++ {
		key := os.Getenv("GIT_CONFIG_KEY_" + strconv.Itoa(i))
		if key == "" {
			break
		}
		value := os.Getenv("GIT_CONFIG_VALUE_" + strconv.Itoa(i))
		lower := strings.ToLower(key)
		c.entries = append(c.entries, gitconfig.Entry{Key: lower, Value: &value})
		c.byKey[lower] = append(c.byKey[lower], value)
	}
	return c, nil
}
