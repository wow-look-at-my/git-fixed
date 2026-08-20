package repair

// The scan. It reads the repository and says what is wrong, as data rather
// than as a report. Nothing here writes.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// Need says who wants an object that is not usable.
type Need struct {
	// Ref names the reference that leads here, empty when nothing does.
	Ref string
	// Path is the route from that reference, as "<commit>:<dir>/<file>".
	Path string
}

// BadObject is one object the repository cannot produce.
type BadObject struct {
	OID gitobj.OID
	// Type is what the object must be, from the link that named it, or
	// TypeNone when only its name is known.
	Type gitobj.Type
	// Corrupt is set when a file for this object exists but will not decode.
	// A corrupt object has a file to quarantine; a missing one does not.
	Corrupt bool
	// Files are the paths holding the unusable copies, absolute.
	Files []string
	// Needs are the references and paths that want this object.
	Needs []Need
}

// BadRef is a reference the repository cannot use.
type BadRef struct {
	Name string
	// Path is the loose ref file, empty for a packed ref.
	Path string
	// Malformed is set when the ref file itself will not parse. The object it
	// meant to name may be perfectly fine.
	Malformed bool
	// Missing is the object the ref names, when that object is the problem.
	Missing gitobj.OID
}

// Damage is everything one scan found.
type Damage struct {
	// Derived are the cache files that will not parse. Every one is
	// rebuildable, so every one is safe to displace.
	Derived []string
	// Objects are the objects that are corrupt or gone.
	Objects []BadObject
	// Refs are the references that will not resolve.
	Refs []BadRef
	// Packs are the packfiles that will not verify.
	Packs []BadPack
	// Index is .git/index when it will not parse, with the reason. The index
	// is not a derived file: it holds staged work that exists nowhere else.
	Index *BadIndex
	// PackedRefs is packed-refs when it will not parse, with the reason.
	PackedRefs *BadPackedRefs
	// Unreachable counts the dangling and unreachable objects, which are not
	// damage. It is carried so a report can say so out loud.
	Unreachable int
}

// Empty reports whether the scan found nothing to repair.
func (d *Damage) Empty() bool {
	return len(d.Derived) == 0 && len(d.Objects) == 0 && len(d.Refs) == 0 &&
		len(d.Packs) == 0 && d.Index == nil && d.PackedRefs == nil
}

// scanner holds one scan's state.
type scanner struct {
	repo *gitrepo.Repo
	db   *odb.DB

	bad   map[string]*BadObject
	seen  map[string]bool
	queue []queued
}

type queued struct {
	oid  gitobj.OID
	typ  gitobj.Type
	need Need
}

// Scan reads the repository and reports what is damaged.
func Scan(repo *gitrepo.Repo, db *odb.DB) (*Damage, error) {
	return scan(repo, db, false)
}

// ScanTrustingFsck is Scan without the two passes a clean fsck has already
// made: verifying every packfile, and reading every object a reference leads
// to. Those two are the whole cost of a scan, and over a healthy repository of
// 229,960 objects they took it from 0.7s to 3.2s while finding nothing.
//
// The caller must have run a full default fsck and had it come back clean. That
// run reads every object in every pack and reports any that is missing or will
// not decode, which is exactly what the two passes look for. A narrower fsck
// does not qualify: --connectivity-only reads no object and --no-full skips the
// packs, so neither has looked.
//
// Everything else still runs, because fsck does not look at all of it. git
// never verifies info/packs, which is a cache for dumb HTTP clients, so a
// corrupt one leaves fsck happy and is still a file to put right.
func ScanTrustingFsck(repo *gitrepo.Repo, db *odb.DB) (*Damage, error) {
	return scan(repo, db, true)
}

func scan(repo *gitrepo.Repo, db *odb.DB, fsckWasClean bool) (*Damage, error) {
	s := &scanner{
		repo: repo,
		db:   db,
		bad:  map[string]*BadObject{},
		seen: map[string]bool{},
	}
	d := &Damage{}
	s.scanDerived(d)
	if !fsckWasClean {
		s.scanPacks(d)
	}
	s.scanIndexes(d)
	// scanRefs first: reading the references is what makes git's own reader
	// pass over packed-refs, and its verdict on that file is what the check
	// below reports.
	s.scanRefs(d)
	s.scanPackedRefs(d)
	if !fsckWasClean {
		s.walk()
	}
	s.collect(d)
	return d, nil
}

// derivedNames are the caches git rebuilds by itself. docs/repair.md says why
// .git/index is not one of them.
var derivedNames = []string{
	"info/commit-graph",
	"info/packs",
}

// scanDerived finds the rebuildable caches that will not parse.
//
// A cache is checked by reading its magic and version, not by verifying what it
// claims, because a cache that disagrees with the objects is displaced by the
// same rule: git rebuilds it either way, and it costs nothing to be wrong.
func (s *scanner) scanDerived(d *Damage) {
	objects := s.repo.ObjectsDir
	for _, name := range derivedNames {
		path := filepath.Join(objects, filepath.FromSlash(name))
		if broken, ok := checkDerived(path); ok && broken {
			d.Derived = append(d.Derived, path)
		}
	}
	// A commit-graph chain names its parts in a file of hashes.
	chainDir := filepath.Join(objects, "info", "commit-graphs")
	if entries, err := os.ReadDir(chainDir); err == nil {
		for _, e := range entries {
			path := filepath.Join(chainDir, e.Name())
			if broken, ok := checkDerived(path); ok && broken {
				d.Derived = append(d.Derived, path)
			}
		}
	}
	packDir := filepath.Join(objects, "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "multi-pack-index",
			strings.HasSuffix(name, ".rev"),
			strings.HasSuffix(name, ".bitmap"):
			path := filepath.Join(packDir, name)
			if broken, ok := checkDerived(path); ok && broken {
				d.Derived = append(d.Derived, path)
			}
		}
	}
	sort.Strings(d.Derived)
}

// derivedMagic is the signature each cache file starts with.
var derivedMagic = map[string][]byte{
	"commit-graph":      []byte("CGPH"),
	"multi-pack-index":  []byte("MIDX"),
	".rev":              []byte("RIDX"),
	".bitmap":           []byte("BITM"),
	"commit-graph-part": []byte("CGPH"),
}

// checkDerived reports whether a cache file is broken. ok is false when there
// is nothing to judge, which is the usual case: the file is not there.
func checkDerived(path string) (broken, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	base := filepath.Base(path)
	var magic []byte
	switch {
	case base == "multi-pack-index":
		magic = derivedMagic["multi-pack-index"]
	case base == "commit-graph":
		magic = derivedMagic["commit-graph"]
	case strings.HasSuffix(base, ".rev"):
		magic = derivedMagic[".rev"]
	case strings.HasSuffix(base, ".bitmap"):
		magic = derivedMagic[".bitmap"]
	case base == "packs":
		// objects/info/packs is a text list of pack names. It is broken when
		// it names a pack that is not there.
		return packsListStale(path, data), true
	default:
		// A commit-graph chain part is named by its hash, and the chain file
		// itself is a list of those names.
		if filepath.Base(filepath.Dir(path)) == "commit-graphs" {
			if base == "commit-graph-chain" {
				return false, false
			}
			magic = derivedMagic["commit-graph-part"]
		}
	}
	if magic == nil {
		return false, false
	}
	if len(data) < len(magic)+4 {
		return true, true
	}
	return string(data[:len(magic)]) != string(magic), true
}

// packsListStale reports whether objects/info/packs names a pack that is gone.
func packsListStale(path string, data []byte) bool {
	dir := filepath.Dir(filepath.Dir(path))
	for _, line := range strings.Split(string(data), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "P ")
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "pack", name)); err != nil {
			return true
		}
	}
	return false
}

// scanRefs reads every reference and queues the objects they name.
func (s *scanner) scanRefs(d *Damage) {
	for _, wt := range s.repo.Worktrees() {
		for _, ref := range s.repo.Refs(wt.Dir) {
			s.checkRef(d, wt.Dir, ref)
		}
		target, oid, ok := s.repo.Head(wt.Dir)
		switch {
		case !ok && target == "":
			d.Refs = append(d.Refs, BadRef{
				Name:      wt.RefName("HEAD"),
				Path:      filepath.Join(wt.Dir, "HEAD"),
				Malformed: true,
			})
		case ok:
			s.want(oid, gitobj.TypeAny, Need{Ref: wt.RefName("HEAD")})
		}
	}
}

// checkRef judges one reference.
func (s *scanner) checkRef(d *Damage, worktreeDir string, ref Ref) {
	name := ref.Name
	switch {
	case ref.Symref != "":
		// A symref that points nowhere is a broken ref, but only when its
		// target does not exist. A branch that has never been created is a
		// normal state for HEAD to be in.
		return
	case ref.Broken || !ref.OID.Valid():
		d.Refs = append(d.Refs, BadRef{
			Name:      name,
			Path:      s.refPath(worktreeDir, name),
			Malformed: true,
		})
	default:
		s.want(ref.OID, gitobj.TypeAny, Need{Ref: name})
	}
}

// Ref is the reference shape the scan reads, aliased so callers of this package
// do not have to import gitrepo for it.
type Ref = gitrepo.Ref

// refPath finds the loose file for a reference, empty when it is packed.
func (s *scanner) refPath(worktreeDir, name string) string {
	for _, dir := range []string{worktreeDir, s.repo.CommonDir} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Lstat(path); err == nil {
			return path
		}
	}
	return ""
}

// want queues an object the repository must be able to produce.
func (s *scanner) want(oid gitobj.OID, typ gitobj.Type, need Need) {
	if !oid.Valid() {
		return
	}
	key := oid.String()
	if s.seen[key] {
		// Record the extra need anyway: a report that names every route to a
		// missing object is worth more than one that names the first.
		if b := s.bad[key]; b != nil {
			b.Needs = appendNeed(b.Needs, need)
		}
		return
	}
	s.seen[key] = true
	s.queue = append(s.queue, queued{oid: oid, typ: typ, need: need})
}

// walk reads every queued object and follows what it points at, so the scan
// reaches everything a reference needs rather than only the tips.
func (s *scanner) walk() {
	for len(s.queue) > 0 {
		q := s.queue[len(s.queue)-1]
		s.queue = s.queue[:len(s.queue)-1]

		typ, data, err := s.db.Read(q.oid)
		if err != nil {
			s.note(q, err)
			continue
		}
		switch typ {
		case gitobj.TypeCommit:
			s.walkCommit(q, data)
		case gitobj.TypeTree:
			s.walkTree(q, data)
		case gitobj.TypeTag:
			s.walkTag(q, data)
		}
	}
}

// note records that an object could not be read.
func (s *scanner) note(q queued, err error) {
	key := q.oid.String()
	b := s.bad[key]
	if b == nil {
		b = &BadObject{OID: q.oid, Type: q.typ}
		b.Files, b.Corrupt = s.copiesOf(q.oid)
		s.bad[key] = b
	}
	b.Needs = appendNeed(b.Needs, q.need)
	_ = err
}

// copiesOf finds the files holding an object, and reports whether any of them
// exists. A file that exists but will not decode is corrupt; no file at all
// means the object was deleted.
func (s *scanner) copiesOf(oid gitobj.OID) (files []string, corrupt bool) {
	name := oid.String()
	for _, dir := range s.db.Dirs {
		path := filepath.Join(dir.Path, name[:2], name[2:])
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	// A packed copy is not a file of its own, and a pack is never displaced
	// for one bad entry: the other objects in it are fine.
	return files, len(files) > 0
}

// walkCommit follows a commit's tree and parents.
func (s *scanner) walkCommit(q queued, data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if hex, ok := strings.CutPrefix(line, "tree "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeTree, Need{
					Ref:  q.need.Ref,
					Path: q.oid.String() + ":",
				})
			}
			continue
		}
		if hex, ok := strings.CutPrefix(line, "parent "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeCommit, Need{Ref: q.need.Ref})
			}
		}
	}
}

// walkTree follows a tree's entries.
func (s *scanner) walkTree(q queued, data []byte) {
	entries, _ := fsck.ParseTree(data, s.repo.Algo)
	for _, e := range entries {
		if e.IsGitlink() {
			// A submodule's commit lives in the submodule, not here.
			continue
		}
		typ := gitobj.TypeBlob
		if e.IsDir() {
			typ = gitobj.TypeTree
		}
		s.want(e.OID, typ, Need{
			Ref:  q.need.Ref,
			Path: joinPath(q.need.Path, string(e.Name)),
		})
	}
}

// walkTag follows a tag to what it names.
func (s *scanner) walkTag(q queued, data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if hex, ok := strings.CutPrefix(line, "object "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeAny, Need{Ref: q.need.Ref})
			}
			return
		}
	}
}

// joinPath builds the route to an object for the report.
func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	if strings.HasSuffix(prefix, ":") {
		return prefix + name
	}
	return prefix + "/" + name
}

// appendNeed keeps the needs unique, so a report does not repeat itself.
func appendNeed(needs []Need, n Need) []Need {
	if n.Ref == "" && n.Path == "" {
		return needs
	}
	for _, have := range needs {
		if have == n {
			return needs
		}
	}
	return append(needs, n)
}

// collect turns the scan's tables into the answer, in a stable order.
func (s *scanner) collect(d *Damage) {
	for _, b := range s.bad {
		d.Objects = append(d.Objects, *b)
	}
	sort.Slice(d.Objects, func(i, j int) bool {
		return d.Objects[i].OID.Compare(d.Objects[j].OID) < 0
	})
	sort.Slice(d.Refs, func(i, j int) bool { return d.Refs[i].Name < d.Refs[j].Name })
}

// Describe renders one bad object for a person to read.
func (b BadObject) Describe() string {
	state := "missing"
	if b.Corrupt {
		state = "corrupt"
	}
	var where []string
	for _, n := range b.Needs {
		switch {
		case n.Ref != "" && n.Path != "":
			where = append(where, fmt.Sprintf("%s -> %s", n.Ref, n.Path))
		case n.Ref != "":
			where = append(where, n.Ref)
		case n.Path != "":
			where = append(where, n.Path)
		}
	}
	if len(where) == 0 {
		return fmt.Sprintf("%s %s", state, b.OID)
	}
	return fmt.Sprintf("%s %s, needed by %s", state, b.OID, strings.Join(where, ", "))
}
