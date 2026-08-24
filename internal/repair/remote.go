package repair

// Recovering an object from a remote.

import (
	"errors"
	"fmt"
	"io"
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

// errNoRemote says the repository has nothing to fetch from, which is an ordinary state and not a failure.
var errNoRemote = errors.New("no remote is configured")

// errWouldClone says the remote will not serve the wanted objects by name.
var errWouldClone = errors.New("this remote does not serve objects by name, so reaching it means fetching every branch and tag")

// RemotePolicy says how the recovery ladder may use a remote.
type RemotePolicy struct {
	// Want is every object the run is looking for.
	Want []gitobj.OID
	// EveryRef allows the fallback for a server that refuses to serve an object by name.
	EveryRef bool
	// Progress is where git's own fetch progress goes.
	Progress io.Writer
}

// openRemote brings the wanted objects into a scratch repository.
//
// It asks for them by name first. A server that allows that sends what was
// asked for and nothing else, so the transfer is over in a moment however large
// the repository is. Most do allow it: uploadpack.allowAnySHA1InWant, or
// allowReachableSHA1InWant, which every object a reference reaches satisfies.
//
// Fetching every ref always works, because a ref is always servable. It is the
// fallback and not the first move, because it costs a copy of the entire
// repository to recover one object.
func openRemote(repo *gitrepo.Repo, policy RemotePolicy) (*remoteSource, error) {
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
	// A scratch repository must not inherit the damaged one's alternates, or
	// it would believe it already has the objects that are broken there.
	if err := run(dir, "git", "init", "--bare", "--object-format="+objectFormat, "."); err != nil {
		return clean(err)
	}
	if err := fetchWanted(dir, url, policy); err != nil {
		return clean(err)
	}

	db, err := odb.Open(filepath.Join(dir, "objects"), filepath.Join(dir, "objects"), repo.Algo)
	if err != nil {
		return clean(err)
	}
	return &remoteSource{dir: dir, db: db, algo: repo.Algo}, nil
}

// wantBatch is how many object names go into one fetch.
const wantBatch = 128

// fetchWanted asks for the objects by name, and falls back to every ref only
// when the policy allows it.
//
// A batch that fails takes the whole by-name attempt with it. A server that
// refuses one name refuses all of them, and a partial answer here would read as
// "the remote does not have it" about objects nobody got round to asking for.
func fetchWanted(dir, url string, policy RemotePolicy) error {
	if len(policy.Want) == 0 {
		// Nothing to ask for by name, so every ref is the only question
		// there is.
		if !policy.EveryRef {
			return errWouldClone
		}
	} else if err := fetchByName(dir, url, policy); err == nil {
		return nil
	} else if !policy.EveryRef {
		return fmt.Errorf("%w (%v)", errWouldClone, err)
	} else {
		say(policy.Progress, "The remote will not serve objects by name, so recovering %d of them\n"+
			"means fetching every branch and tag: a full copy of the repository, into\n"+
			"%s.\n", len(policy.Want), dir)
	}

	if err := runVerbose(dir, policy.Progress, "git", "fetch", "--progress", "--no-tags", "--force", url,
		"+refs/heads/*:refs/fixed/heads/*", "+refs/tags/*:refs/fixed/tags/*"); err != nil {
		return fmt.Errorf("fetching from %s: %w", url, err)
	}
	return nil
}

// fetchByName asks the server for the objects themselves.
func fetchByName(dir, url string, policy RemotePolicy) error {
	for chunk := range slices.Chunk(policy.Want, wantBatch) {
		args := []string{"fetch", "--progress", "--no-tags", "--force", url}
		for _, oid := range chunk {
			args = append(args, oid.String())
		}
		if err := runVerbose(dir, policy.Progress, "git", args...); err != nil {
			return err
		}
	}
	return nil
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

// leaked are the variables that would point a scratch repository back at the damaged one.
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

// say writes a line about what the remote step is about to do, when there is
// anybody to say it to.
func say(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}

// runVerbose is run with git's own progress passed straight through.
//
// A fetch is the one step here that takes minutes, and the only one whose
// progress somebody else already writes. Swallowing it, which is what
// CombinedOutput does, leaves a transfer of tens of gigabytes looking like a
// hung process.
func runVerbose(dir string, progress io.Writer, name string, args ...string) error {
	if progress == nil {
		return run(dir, name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = scratchEnv()
	cmd.Stdout = progress
	cmd.Stderr = progress
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
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
