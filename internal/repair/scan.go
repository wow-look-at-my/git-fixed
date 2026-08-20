package repair

// The scan. It reads the repository and says what is wrong, as data rather
// than as a report. Nothing here writes.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/git-fixed/internal/progress"
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
//
// Both tables are keyed on the object name itself. Keying them on its hex
// spelling cost a 40-byte string for every tree entry in the repository, most
// of them thrown away at once, and kept one per object for the whole walk.
type scanner struct {
	repo *gitrepo.Repo
	db   *odb.DB

	bad   map[gitobj.OID]*BadObject
	seen  map[gitobj.OID]bool
	queue []queued

	meters Meters
}

type queued struct {
	oid gitobj.OID
	typ gitobj.Type
	ref string
	// path is the route from that reference, kept as a link back to the
	// containing tree rather than as a whole string. see pathNode.
	path *pathNode
}

// pathNode is one step of the route to an object, sharing everything above it
// with its siblings.
//
// A joined string per entry looks harmless and is not: a directory of a
// thousand entries copies its own path a thousand times, and every copy stays
// in the queue until its object is read. A node is one pointer and one name,
// and the whole route above it is one pointer away.
//
// It is only ever rendered for an object that turns out to be bad.
type pathNode struct {
	parent *pathNode
	name   string
}

// String renders the route, from the reference down.
func (p *pathNode) String() string {
	if p == nil {
		return ""
	}
	var parts []string
	for n := p; n != nil; n = n.parent {
		parts = append(parts, n.name)
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), ":") {
			b.WriteByte('/')
		}
		b.WriteString(parts[i])
	}
	return b.String()
}

// need renders what a report says about one queued object.
func (q queued) need() Need { return Need{Ref: q.ref, Path: q.path.String()} }

// Scan reads the repository and reports what is damaged. meters draws the two
// passes that take the time, or is the zero value when nobody asked.
func Scan(repo *gitrepo.Repo, db *odb.DB, meters Meters) (*Damage, error) {
	return scan(repo, db, meters, false)
}

// Meters is where a scan draws its progress. A scan of a broken repository
// reads every pack and then every object a reference leads to, which is a
// second pass over everything the fsck before it already read, and until this
// existed it printed nothing for the whole of it.
type Meters struct {
	// Stderr is where the meters are drawn. A nil writer, or Show left
	// false, means no meter is started at all.
	Stderr io.Writer
	Show   bool
}

// start begins one meter, or returns nil when this scan draws none.
func (m Meters) start(title string, total int64) *progress.Meter {
	if !m.Show || m.Stderr == nil {
		return nil
	}
	return progress.Start(m.Stderr, title, total)
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
	// The two passes that would draw a meter are the two this skips, so a
	// trusting scan has nothing to show and finishes in well under a second.
	return scan(repo, db, Meters{}, true)
}

func scan(repo *gitrepo.Repo, db *odb.DB, meters Meters, fsckWasClean bool) (*Damage, error) {
	s := &scanner{
		repo:   repo,
		db:     db,
		bad:    map[gitobj.OID]*BadObject{},
		seen:   map[gitobj.OID]bool{},
		meters: meters,
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
			s.want(oid, gitobj.TypeAny, wt.RefName("HEAD"), nil)
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
		s.want(ref.OID, gitobj.TypeAny, name, nil)
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
func (s *scanner) want(oid gitobj.OID, typ gitobj.Type, ref string, path *pathNode) {
	if !oid.Valid() {
		return
	}
	if s.seen[oid] {
		// Record the extra need anyway: a report that names every route to a
		// missing object is worth more than one that names the first.
		if b := s.bad[oid]; b != nil {
			b.Needs = appendNeed(b.Needs, Need{Ref: ref, Path: path.String()})
		}
		return
	}
	s.seen[oid] = true
	s.queue = append(s.queue, queued{oid: oid, typ: typ, ref: ref, path: path})
}

// walk reads every queued object and follows what it points at, so the scan
// reaches everything a reference needs rather than only the tips.
func (s *scanner) walk() {
	// There is no total to count against: the queue grows as the walk finds
	// what each object points at, so the number of objects it will reach is
	// not known until it has reached them. git's own connectivity meter
	// counts the same way, and for the same reason.
	m := s.meters.start("Checking what the references reach", 0)
	defer m.Finish()
	for len(s.queue) > 0 {
		q := s.queue[len(s.queue)-1]
		s.queue = s.queue[:len(s.queue)-1]
		m.Step()

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
	b := s.bad[q.oid]
	if b == nil {
		b = &BadObject{OID: q.oid, Type: q.typ}
		b.Files, b.Corrupt = s.copiesOf(q.oid)
		s.bad[q.oid] = b
	}
	b.Needs = appendNeed(b.Needs, q.need())
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
				s.want(oid, gitobj.TypeTree, q.ref, &pathNode{name: q.oid.String() + ":"})
			}
			continue
		}
		if hex, ok := strings.CutPrefix(line, "parent "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeCommit, q.ref, nil)
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
		s.want(e.OID, typ, q.ref, &pathNode{parent: q.path, name: string(e.Name)})
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
				s.want(oid, gitobj.TypeAny, q.ref, nil)
			}
			return
		}
	}
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
