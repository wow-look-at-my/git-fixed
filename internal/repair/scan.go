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
	"sync/atomic"
	"time"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/git-fixed/internal/progress"
	"github.com/wow-look-at-my/go-containers/concurrentbag"
	"github.com/wow-look-at-my/go-containers/concurrentmap"
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
	// Type is what the object must be, from the link that named it, or TypeNone when only its name is known.
	Type gitobj.Type
	// Corrupt is set when a file for this object exists but will not decode.
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
	// Malformed is set when the ref file itself will not parse.
	Malformed bool
	// Missing is the object the ref names, when that object is the problem.
	Missing gitobj.OID
}

// Damage is everything one scan found.
type Damage struct {
	// Derived are the cache files that will not parse.
	Derived []string
	// Objects are the objects that are corrupt or gone.
	Objects []BadObject
	// Refs are the references that will not resolve.
	Refs []BadRef
	// Packs are the packfiles that will not verify.
	Packs []BadPack
	// Verified are the packs this scan read end to end or took on trust, each with the file it read.
	Verified []VerifiedPack
	// Index is .git/index when it will not parse, with the reason.
	Index *BadIndex
	// PackedRefs is packed-refs when it will not parse, with the reason.
	PackedRefs *BadPackedRefs
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

	bad  *concurrentmap.Map[gitobj.OID, *BadObject]
	seen *concurrentmap.Map[gitobj.OID, bool]
	// queue is a bag rather than a stack: the walk does not care what order it reaches objects in.
	queue *concurrentbag.Bag[queued]
	// anyBad is raised the moment an object first fails to read.
	anyBad atomic.Bool
	// pending counts what is queued plus what a worker is still reading.
	pending atomic.Int64

	// hunt is how many damaged objects the walk is looking for, when somebody else has already found them all.
	hunt int
	// found counts the damaged objects the walk has reached, and enough is the end of the walk.
	found atomic.Int64
	stop  atomic.Bool
	// errand is set on a walk that starts under the objects a pass has just put back.
	errand bool

	// trusted holds the packs that have been read end to end, object by object.
	trusted map[string]bool
	// verified is what this scan can hand the next scan of the same run: the packs it read or took on trust.
	verified []VerifiedPack
	meters   Meters
}

// VerifiedPack is a packfile a scan read end to end, with what the file was at the moment it did.
type VerifiedPack struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Scan reads the repository and reports what is damaged.
func Scan(repo *gitrepo.Repo, db *odb.DB, meters Meters) (*Damage, error) {
	return scan(repo, db, meters, nil, nil)
}

// rescan reads the repository again after a repair pass changed it.
func rescan(repo *gitrepo.Repo, db *odb.DB, meters Meters, verified []VerifiedPack) (*Damage, error) {
	return scan(repo, db, meters, nil, verified)
}

// Meters is where a scan draws its progress.
type Meters struct {
	// Stderr is where the meters are drawn.
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

// scan reads the repository, skipping the passes the caller's own fsck has
// already made.
//
// The two it can skip are the whole cost of a scan: verifying every packfile,
// and reading every object a reference leads to. Over a healthy repository of
// 229,960 objects they took a scan from 0.7s to 3.2s while finding nothing, and
// over a hundred million objects they are twenty minutes each.
//
// What may be skipped is decided bit by bit, because a verdict's bits answer
// different questions. A repository whose references are broken, whose
// commit-graph is wrong, or whose index will not parse has had every one of its
// objects read and approved by the fsck that found those faults, and reading
// them all again finds nothing.
//
// Everything else still runs, because fsck does not look at all of it. git
// never verifies info/packs, which is a cache for dumb HTTP clients, so a
// corrupt one leaves fsck happy and is still a file to put right.
func scan(repo *gitrepo.Repo, db *odb.DB, meters Meters, v *Verdict, verified []VerifiedPack) (*Damage, error) {
	s := &scanner{
		repo:   repo,
		db:     db,
		bad:    concurrentmap.New[gitobj.OID, *BadObject](),
		seen:   concurrentmap.New[gitobj.OID, bool](),
		queue:  concurrentbag.New[queued](),
		meters: meters,
	}
	d := &Damage{}
	s.scanDerived(d)
	// Every pack the caller's fsck read end to end is a pack this does not read again.
	s.trustNamed(v.verifiedPacks())
	s.trustUnchanged(verified)
	s.scanPacks(d)
	d.Verified = s.verified
	s.scanIndexes(d)
	// scanRefs first: reading the references is what makes git's own reader pass over packed-refs.
	s.scanRefs(d)
	s.scanPackedRefs(d)
	// When the fsck named every damaged object, the walk is not out looking for damage.
	s.hunt, _ = v.damageNamed()
	if !v.refsReach() {
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
		// A symref that points nowhere is a broken ref, but only when its target does not exist.
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

// Ref is the reference shape the scan reads.
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

// trustNamed records the packs somebody else has already read end to end.
func (s *scanner) trustNamed(paths []string) {
	s.trusted = make(map[string]bool, len(paths))
	for _, p := range paths {
		s.trust(p)
	}
}

// trustUnchanged records the packs an earlier scan of this same run read end to end.
func (s *scanner) trustUnchanged(packs []VerifiedPack) {
	for _, p := range packs {
		if fi, err := os.Stat(p.Path); err == nil && fi.Size() == p.Size && fi.ModTime().Equal(p.ModTime) {
			s.trust(p.Path)
		}
	}
}

// trust records one more pack this scan takes on trust, and notes what the file
// looked like, so a later scan of the same run can tell whether it still is
// that file.
func (s *scanner) trust(path string) {
	if s.trusted == nil {
		s.trusted = map[string]bool{}
	}
	if s.trusted[path] {
		return
	}
	s.trusted[path] = true
	fi, err := os.Stat(path)
	if err != nil {
		// Trusted for this scan, because somebody read it, but there is nothing to hand the next one.
		return
	}
	s.verified = append(s.verified, VerifiedPack{Path: path, Size: fi.Size(), ModTime: fi.ModTime()})
}

// collect turns the scan's tables into the answer, in a stable order.
func (s *scanner) collect(d *Damage) {
	for _, b := range s.bad.All() {
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
