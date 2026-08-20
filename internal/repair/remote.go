package repair

// Recovering an object from a remote.
//
// Fetching into the damaged repository does not work, and the reason is worth
// knowing: fetch negotiation is driven by what the repository says it has, and
// a repository holding a corrupt object says it has that object -- the file is
// on disk under the right name. The remote is told not to send it, the fetch
// succeeds, and nothing is fixed. So the objects arrive in a scratch
// repository, which has nothing and therefore asks for everything.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// remoteSource is a scratch repository holding what a remote sent.
type remoteSource struct {
	dir  string
	db   *odb.DB
	algo *gitobj.Algo
}

// errNoRemote says the repository has nothing to fetch from, which is an
// ordinary state and not a failure.
var errNoRemote = errors.New("no remote is configured")

// openRemote fetches every ref a remote offers into a scratch repository.
//
// Everything is fetched rather than the specific objects, because a server only
// has to serve objects by name when it is configured to, and most are not. Refs
// are always servable, and the objects a ref reaches come with it.
func openRemote(repo *gitrepo.Repo) (*remoteSource, error) {
	url := firstRemoteURL(repo)
	if url == "" {
		return nil, errNoRemote
	}
	dir, err := os.MkdirTemp("", "git-fixed-remote-")
	if err != nil {
		return nil, err
	}
	clean := func(err error) (*remoteSource, error) {
		os.RemoveAll(dir)
		return nil, err
	}

	objectFormat := "sha1"
	if repo.Algo != nil && repo.Algo.Name != "" {
		objectFormat = repo.Algo.Name
	}
	if err := run(dir, "git", "init", "--bare", "--object-format="+objectFormat, "."); err != nil {
		return clean(err)
	}
	// A scratch repository must not inherit the damaged one's alternates, or
	// it would believe it already has the objects that are broken there.
	if err := run(dir, "git", "fetch", "--no-tags", "--force", url,
		"+refs/heads/*:refs/fixed/heads/*", "+refs/tags/*:refs/fixed/tags/*"); err != nil {
		return clean(fmt.Errorf("fetching from %s: %w", url, err))
	}

	db, err := odb.Open(filepath.Join(dir, "objects"), filepath.Join(dir, "objects"), repo.Algo, true)
	if err != nil {
		return clean(err)
	}
	return &remoteSource{dir: dir, db: db, algo: repo.Algo}, nil
}

// get reads one object out of what the remote sent.
func (r *remoteSource) get(oid gitobj.OID) (gitobj.Type, []byte, bool) {
	typ, data, err := r.db.Read(oid)
	if err != nil {
		return 0, nil, false
	}
	return typ, data, true
}

// Close removes the scratch repository.
func (r *remoteSource) Close() {
	if r.db != nil {
		r.db.Close()
	}
	os.RemoveAll(r.dir)
}

// firstRemoteURL picks the remote to ask. origin wins when it is there, because
// that is the one a clone set up and the one most likely to be complete.
func firstRemoteURL(repo *gitrepo.Repo) string {
	if url, ok := repo.Config.Get("remote.origin.url"); ok && url != "" {
		return url
	}
	best := ""
	for _, e := range repo.Config.Entries() {
		key := strings.ToLower(e.Key)
		if !strings.HasPrefix(key, "remote.") || !strings.HasSuffix(key, ".url") {
			continue
		}
		if e.Value != nil && *e.Value != "" && (best == "" || *e.Value < best) {
			best = *e.Value
		}
	}
	return best
}

// leaked are the variables that would point a scratch repository back at the
// damaged one. They are REMOVED, never set empty: git rejects an empty
// GIT_WORK_TREE outright with "not allowed without specifying GIT_DIR", which
// takes the whole remote path out without saying so.
var leaked = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_CEILING_DIRECTORIES",
}

// scratchEnv is the environment a scratch repository's git runs under.
func scratchEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(leaked, name) {
			env = append(env, kv)
		}
	}
	// Nothing is watching, so a credential prompt would hang the repair.
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// run executes a git command in dir, reporting what it printed when it fails.
func run(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = scratchEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
