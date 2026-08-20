package repair

// The recovery ladder. Given an object name, find the bytes that hash to it.
//
// Every source ends at the same check, in odb.WriteLoose: content that does not
// hash to the name being recovered is not that object and is refused. That is
// what makes a recovery provably the original and not an approximation.

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
	// remote is the scratch checkout a remote's objects arrive in, opened at
	// most once and only when a local source has already failed.
	remote *remoteSource
	// remoteErr says why the remote could not be consulted, so a run can tell
	// the difference between "gone" and "unreachable".
	remoteErr error
}

// NewSources reads the index once and prepares the local sources.
func NewSources(repo *gitrepo.Repo, db *odb.DB) *Sources {
	s := &Sources{repo: repo, db: db, byOID: map[string][]string{}}
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

// Recover finds the bytes for one object and writes it back into the
// repository. It reports which source answered.
//
// The order is local first. A local source costs a read and cannot fail
// halfway, and every source yields identical bytes anyway, because the name
// pins the content.
func (s *Sources) Recover(b BadObject, objectsDir string) (Recovered, error) {
	type attempt struct {
		name string
		get  func(BadObject) (gitobj.Type, []byte, bool)
	}
	for _, a := range []attempt{
		{"another copy in this repository", s.fromDuplicate},
		{"the worktree", s.fromWorktree},
		{"a rebuild from the index", s.fromIndexTree},
		{"a remote", s.fromRemote},
	} {
		typ, content, ok := a.get(b)
		if !ok {
			continue
		}
		oid, err := odb.WriteLoose(objectsDir, s.repo.Algo, typ, content, b.OID)
		if err != nil {
			// The source produced something that is not this object. That is
			// the check doing its job, so try the next source rather than
			// writing what would be a different object under this name.
			continue
		}
		return Recovered{OID: oid, Type: typ, Source: a.name}, nil
	}
	return Recovered{}, fmt.Errorf("no source has %s", b.OID)
}

// fromDuplicate finds a second, readable copy already in the repository.
//
// The object database reads a name from wherever it can: a pack, another object
// directory, an alternate. A corrupt loose file that also exists packed is the
// common case, and the packed copy is already the object.
func (s *Sources) fromDuplicate(b BadObject) (gitobj.Type, []byte, bool) {
	if !b.Corrupt {
		// Nothing on disk claims to be this object, so there is no duplicate
		// to prefer. The database already said it cannot produce it.
		return 0, nil, false
	}
	for _, path := range b.Files {
		// Read the loose copies directly rather than through the database,
		// which caches its verdict about a name it has already failed on.
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
// A tree's bytes are decided entirely by its entries: mode, name, object name,
// sorted git's way. So a tree can be rebuilt exactly, and the rebuild proves
// itself by hashing to the name that went missing. A rebuild that does not
// match is discarded, which is what happens when the index has moved on.
func (s *Sources) fromIndexTree(b BadObject) (gitobj.Type, []byte, bool) {
	if len(s.entries) == 0 {
		return 0, nil, false
	}
	// The wanted tree can be at any depth, and the index does not say which
	// directory it is. Build every directory's tree and look for the name.
	trees := buildTrees(s.entries, s.repo.Algo)
	if content, ok := trees[b.OID.String()]; ok {
		return gitobj.TypeTree, content, true
	}
	return 0, nil, false
}

// fromRemote fetches the object from a configured remote.
//
// A remote that cannot be reached is recorded rather than passed over. A source
// that fails quietly turns "your remote is unreachable" into "your objects are
// gone", which is the wrong thing to tell someone about their repository.
func (s *Sources) fromRemote(b BadObject) (gitobj.Type, []byte, bool) {
	if s.remoteErr != nil {
		return 0, nil, false
	}
	if s.remote == nil {
		r, err := openRemote(s.repo)
		if err != nil {
			s.remoteErr = err
			return 0, nil, false
		}
		s.remote = r
	}
	return s.remote.get(b.OID)
}

// RemoteError reports why the remote could not be consulted, nil when it was
// not needed or it worked. errNoRemote is not an error to show: a repository
// with no remote is an ordinary repository.
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

// treeSortName is the key git sorts tree entries by: a directory sorts as
// though its name ended in a slash, which is why "foo.txt" comes before "foo/".
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
