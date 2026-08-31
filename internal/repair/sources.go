package repair

// The recovery ladder.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// Recovered is one object put back, and where it came from.
type Recovered struct {
	OID    gitobj.OID
	Type   gitobj.Type
	Source string
}

// Sources knows every place this repository can produce an object from.
//
// It holds the index and the worktree maps because building them costs one pass
// and answers every object, where doing it per object would re-read the
// worktree once for each thing that went missing.
type Sources struct {
	repo *gitrepo.Repo
	db   *odb.DB

	// byOID maps an object name to the worktree paths the index says hold it.
	byOID map[string][]string
	// entries is the index, for rebuilding a tree.
	entries []gitrepo.IndexEntry
	// trees is the index folded back into tree objects, built once.
	trees map[string][]byte
	// remote is the scratch checkout a remote's objects arrive in.
	remote *remoteSource
	// policy says what the remote may be asked for, and whether a fetch of every ref is allowed at all.
	policy RemotePolicy
	// remoteErr says why the remote could not be consulted.
	remoteErr error
}

// NewSources reads the index once and prepares the local sources. policy is
// how the remote at the bottom of the ladder may be used.
func NewSources(repo *gitrepo.Repo, db *odb.DB, policy RemotePolicy) *Sources {
	s := &Sources{repo: repo, db: db, policy: policy, byOID: map[string][]string{}}
	for _, wt := range repo.Worktrees() {
		idx, _, err := repo.ReadIndex(wt.IndexPath())
		if err != nil || idx == nil {
			continue
		}
		s.entries = append(s.entries, idx.Entries...)
		for _, e := range idx.Entries {
			if !e.OID.Valid() {
				continue
			}
			base := wt.Path
			if base == "" {
				base = repo.WorkTree
			}
			if base == "" {
				continue
			}
			s.byOID[e.OID.String()] = append(s.byOID[e.OID.String()], filepath.Join(base, filepath.FromSlash(e.Name)))
		}
	}
	return s
}

// Close releases the scratch repository, when one was needed.
func (s *Sources) Close() {
	if s.remote != nil {
		s.remote.Close()
	}
}

// Retarget points the local sources at a reopened repository and keeps the
// remote, because refetching it costs another copy of it.
func (s *Sources) Retarget(repo *gitrepo.Repo, db *odb.DB) {
	remote, remoteErr := s.remote, s.remoteErr
	*s = *NewSources(repo, db, s.policy)
	s.remote, s.remoteErr = remote, remoteErr
}

// Prime fetches what this pass is about to ask a remote for, in one go and only
// for the objects no local source answers.
//
// The remote is the last rung, so an object the worktree or another copy here
// already holds must never be fetched at all.
func (s *Sources) Prime(bad []BadObject) {
	if s.remoteErr != nil {
		return
	}
	var need []gitobj.OID
	for _, b := range bad {
		if _, ok := s.local(b); !ok {
			need = append(need, b.OID)
		}
	}
	if len(need) == 0 {
		// Everything damaged has a local answer, so there is no remote to open.
		return
	}
	if s.remote == nil {
		r, err := openRemote(s.repo, s.policy)
		if err != nil {
			s.remoteErr = err
			return
		}
		s.remote = r
	}
	if err := s.remote.want(need); err != nil {
		s.remoteErr = err
	}
}

// Found is one object's bytes, before they are written back.
type Found struct {
	Type    gitobj.Type
	Content []byte
	Source  string
}

// Find reads an object's bytes from the first source that has them. It does
// not write: the caller must quarantine the corrupt file before it writes.
func (s *Sources) Find(b BadObject) (Found, error) {
	if f, ok := s.local(b); ok {
		return f, nil
	}
	if f, ok := s.check(b, "a remote", s.fromRemote); ok {
		return f, nil
	}
	return Found{}, fmt.Errorf("no source has %s", b.OID)
}

// local reads the bytes out of the first source in this repository that has
// them, which is the order the ladder wants: a local source costs a read and
// cannot fail halfway, and the name pins the content, so every source that
// answers at all yields the identical bytes. Prime uses it to decide what is
// worth asking a remote for.
func (s *Sources) local(b BadObject) (Found, bool) {
	type attempt struct {
		name string
		get  func(BadObject) (gitobj.Type, []byte, bool)
	}
	for _, a := range []attempt{
		{"another copy in this repository", s.fromDuplicate},
		{"the worktree", s.fromWorktree},
		{"a rebuild from the index", s.fromIndexTree},
	} {
		if f, ok := s.check(b, a.name, a.get); ok {
			return f, true
		}
	}
	return Found{}, false
}

// check runs one source and keeps what it produced only when that is the object
// asked for.
func (s *Sources) check(b BadObject, name string, get func(BadObject) (gitobj.Type, []byte, bool)) (Found, bool) {
	typ, content, ok := get(b)
	if !ok {
		return Found{}, false
	}
	if odb.Hash(s.repo.Algo, typ, content).Compare(b.OID) != 0 {
		// The source produced something that is not this object.
		return Found{}, false
	}
	return Found{Type: typ, Content: content, Source: name}, true
}

// Write puts a found object into the repository. WriteLoose hashes it again and
// refuses anything that does not match, which is the check that makes a
// recovery the original object rather than an approximation.
func (s *Sources) Write(b BadObject, f Found, objectsDir string) (Recovered, error) {
	oid, err := odb.WriteLoose(objectsDir, s.repo.Algo, f.Type, f.Content, b.OID)
	if err != nil {
		return Recovered{}, err
	}
	return Recovered{OID: oid, Type: f.Type, Source: f.Source}, nil
}

// fromDuplicate finds a second, readable copy already in the repository.
//
// The object database reads a name from wherever it can: a pack, another object
// directory, an alternate. A corrupt loose file that also exists packed is the
// common case, and the packed copy is already the object.
func (s *Sources) fromDuplicate(b BadObject) (gitobj.Type, []byte, bool) {
	if !b.Corrupt {
		// Nothing on disk claims to be this object, so there is no duplicate to prefer.
		return 0, nil, false
	}
	for _, path := range b.Files {
		// Read the loose copies directly rather than through the database.
		res := odb.ReadLoose(path, path, b.OID, s.repo.Algo, 0)
		if res != nil && !res.Failed && res.Contents != nil {
			return res.Type, res.Contents, true
		}
	}
	if s.db.HasPacked(b.OID) {
		if typ, data, err := s.db.Read(b.OID); err == nil {
			return typ, data, true
		}
	}
	return 0, nil, false
}

// fromWorktree reads a missing blob back out of the checked-out file.
//
// The index says which path holds which object, so this reads exactly the files
// that could answer. It never searches: a file whose content has changed since
// it was staged simply fails the hash check and is not used.
func (s *Sources) fromWorktree(b BadObject) (gitobj.Type, []byte, bool) {
	for _, path := range s.byOID[b.OID.String()] {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if odb.Hash(s.repo.Algo, gitobj.TypeBlob, data).Compare(b.OID) == 0 {
			return gitobj.TypeBlob, data, true
		}
	}
	return 0, nil, false
}

// fromIndexTree rebuilds a missing tree from what the index says it held.
//
// A tree's bytes are decided entirely by its entries, so the rebuild proves itself by hashing to the name
// that went missing. A rebuild that does not match is discarded.
func (s *Sources) fromIndexTree(b BadObject) (gitobj.Type, []byte, bool) {
	if len(s.entries) == 0 {
		return 0, nil, false
	}
	if s.trees == nil {
		// The wanted tree can be at any depth, and the index does not say which directory it is.
		s.trees = buildTrees(s.entries, s.repo.Algo)
	}
	if content, ok := s.trees[b.OID.String()]; ok {
		return gitobj.TypeTree, content, true
	}
	return 0, nil, false
}

// fromRemote reads an object out of what a remote already sent. Prime does the
// fetching, so this never reaches the network.
func (s *Sources) fromRemote(b BadObject) (gitobj.Type, []byte, bool) {
	if s.remote == nil {
		return 0, nil, false
	}
	return s.remote.get(b.OID)
}

// RemoteError reports why the remote could not be consulted, nil when it was not needed or it worked.
func (s *Sources) RemoteError() error {
	if errors.Is(s.remoteErr, errNoRemote) {
		return nil
	}
	return s.remoteErr
}

// treeNode is one directory while the index is being folded back into trees.
type treeNode struct {
	entries map[string]fsck.TreeEntry
	dirs    map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{entries: map[string]fsck.TreeEntry{}, dirs: map[string]*treeNode{}}
}

// buildTrees folds the index back into tree objects and returns each one's
// serialized bytes, keyed by the name it hashes to.
//
// Only stage-zero entries take part. A path in conflict has several versions
// and no single tree ever held it.
func buildTrees(entries []gitrepo.IndexEntry, algo *gitobj.Algo) map[string][]byte {
	root := newTreeNode()
	for _, e := range entries {
		if e.Stage != 0 || !e.OID.Valid() {
			continue
		}
		node := root
		parts := splitPath(e.Name)
		for _, dir := range parts[:len(parts)-1] {
			next := node.dirs[dir]
			if next == nil {
				next = newTreeNode()
				node.dirs[dir] = next
			}
			node = next
		}
		name := parts[len(parts)-1]
		node.entries[name] = fsck.TreeEntry{Mode: e.Mode, Name: []byte(name), OID: e.OID}
	}
	out := map[string][]byte{}
	serializeTree(root, algo, out)
	return out
}

// serializeTree writes one directory and everything under it, depth first
// because a directory's own bytes contain its subdirectories' names.
func serializeTree(node *treeNode, algo *gitobj.Algo, out map[string][]byte) gitobj.OID {
	all := make([]fsck.TreeEntry, 0, len(node.entries)+len(node.dirs))
	for _, e := range node.entries {
		all = append(all, e)
	}
	for name, child := range node.dirs {
		oid := serializeTree(child, algo, out)
		all = append(all, fsck.TreeEntry{Mode: 0o040000, Name: []byte(name), OID: oid})
	}
	sort.Slice(all, func(i, j int) bool {
		return treeSortName(all[i]) < treeSortName(all[j])
	})

	var buf []byte
	for _, e := range all {
		buf = append(buf, fmt.Sprintf("%o ", e.Mode)...)
		buf = append(buf, e.Name...)
		buf = append(buf, 0)
		buf = append(buf, e.OID.Raw()...)
	}
	oid := odb.Hash(algo, gitobj.TypeTree, buf)
	out[oid.String()] = buf
	return oid
}

// treeSortName is the key git sorts tree entries by: a directory sorts as though its name ended in a slash.
func treeSortName(e fsck.TreeEntry) string {
	if e.Mode&0o170000 == 0o040000 {
		return string(e.Name) + "/"
	}
	return string(e.Name)
}

// splitPath breaks an index path into its components. Index paths always use
// forward slashes, whatever the host.
func splitPath(name string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			parts = append(parts, name[start:i])
			start = i + 1
		}
	}
	return append(parts, name[start:])
}
