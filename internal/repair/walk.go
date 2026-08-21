package repair

// The walk: reading every object a reference leads to, and reading every
// object under one this run has just put back.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/git-fixed/internal/progress"
	"github.com/wow-look-at-my/go-containers/concurrentbag"
	"github.com/wow-look-at-my/go-containers/concurrentmap"
)

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

// descend follows the objects a pass has just put back, and reports what is
// under them that the repository still cannot produce.
//
// A missing tree hides everything below it, so one pass repairs one layer and
// the layer under it only becomes visible once that pass is done. Finding that
// layer by scanning the repository again walks every object every reference
// reaches -- five minutes over a hundred million of them, once per layer of a
// chain nobody chose the length of -- to arrive under an object this run is
// holding.
//
// It reads nothing the next full scan would not have had to read. The previous
// walk stopped at the object that was missing, so nothing below it has been
// looked at yet, and everything else reachable it already approved.
//
// An object that reads as damaged again is reported again, and must be: a
// corrupt entry in a pack shadows the loose copy just written, and the caller
// is what notices that it has recovered this one before.
func descend(repo *gitrepo.Repo, db *odb.DB, meters Meters, from []BadObject, verified []VerifiedPack) (*Damage, error) {
	s := &scanner{
		repo:   repo,
		db:     db,
		bad:    concurrentmap.New[gitobj.OID, *BadObject](),
		seen:   concurrentmap.New[gitobj.OID, bool](),
		queue:  concurrentbag.New[queued](),
		meters: meters,
		errand: true,
	}
	// The packs still stand, so their blobs still need no inflating.
	s.trustUnchanged(verified)
	for _, b := range from {
		var need Need
		if len(b.Needs) > 0 {
			need = b.Needs[0]
		}
		s.want(b.OID, b.Type, need.Ref, &pathNode{name: need.Path})
	}
	s.walk()
	d := &Damage{Verified: s.verified}
	s.collect(d)
	return d, nil
}

// want queues an object the repository must be able to produce.
func (s *scanner) want(oid gitobj.OID, typ gitobj.Type, ref string, path *pathNode) {
	if !oid.Valid() {
		return
	}
	if !s.seen.TryAdd(oid, true) {
		// Record the extra need anyway: a report that names every route to a
		// missing object is worth more than one that names the first.
		if s.knownBad(oid) {
			s.recordNeed(oid, Need{Ref: ref, Path: path.String()})
		}
		return
	}
	// Counted before it is queued. The other order lets a worker find an
	// empty bag between the two and call the walk finished.
	s.pending.Add(1)
	s.queue.Add(queued{oid: oid, typ: typ, ref: ref, path: path})
}

// wantEntry is want for a tree entry, which is where nearly every call comes
// from.
//
// It takes the containing tree's node and the entry's own bytes instead of a
// finished pathNode, because an object that has been queued already needs
// neither. History shares its trees: the same subtree hangs off every commit
// that did not change it, and the same blob hangs off thousands of trees, so
// most calls here are about an object the walk has met before. Each of those
// was costing a node, a string copy of the name, and a walk up the whole route
// to build a string that was dropped on the next line. Rendering a route only
// for an object that turns out to be bad is the whole reason pathNode exists.
func (s *scanner) wantEntry(oid gitobj.OID, typ gitobj.Type, ref string, parent *pathNode, name []byte) {
	if !oid.Valid() {
		return
	}
	if !s.seen.TryAdd(oid, true) {
		if s.knownBad(oid) {
			s.recordNeed(oid, Need{Ref: ref, Path: entryPath(parent, name)})
		}
		return
	}
	s.pending.Add(1)
	s.queue.Add(queued{oid: oid, typ: typ, ref: ref, path: &pathNode{parent: parent, name: string(name)}})
}

// entryPath renders the route to one tree entry.
func entryPath(parent *pathNode, name []byte) string {
	return (&pathNode{parent: parent, name: string(name)}).String()
}

// knownBad reports whether this object has already been found unreadable.
//
// It is one atomic load, then a read where Compute would take the shard's write
// lock. Nearly every
// call is about an object that is perfectly fine, and taking a write lock to
// discover that -- once per repeated tree entry, on every worker -- serializes
// the walk behind a map that has nothing to say.
//
// An object that becomes bad after this asks is missed, and was missed before:
// the walk reads an object after every route to it has been offered, so a
// route seen early is not held anywhere to be attached later. The object itself
// is still reported, with the routes that came after it was read.
func (s *scanner) knownBad(oid gitobj.OID) bool {
	return s.anyBad.Load() && s.bad.Contains(oid)
}

// recordNeed adds another route to an object already known to be bad.
func (s *scanner) recordNeed(oid gitobj.OID, need Need) {
	s.bad.Compute(oid, func(old *BadObject, loaded bool) (*BadObject, bool) {
		if !loaded {
			// Nothing to add to, and nothing to create: this object read
			// back perfectly well.
			return nil, true
		}
		old.Needs = appendNeed(old.Needs, need)
		return old, false
	})
}

// walk reads every queued object and follows what it points at, so the scan
// reaches everything a reference needs rather than only the tips.
//
// It has two shapes. With no verdict to go on it is a search, and it reads
// everything. With one it is an errand: the fsck has already named what cannot
// be produced, and the walk goes and finds the route to each of those and
// stops. An errand reports the first route to each damaged object rather than
// every route, which is the price of not reading a hundred million objects to
// list the rest of them.
func (s *scanner) walk() {
	// There is no total to count against: the queue grows as the walk finds
	// what each object points at, so the number of objects it will reach is
	// not known until it has reached them. git's own connectivity meter
	// counts the same way, and for the same reason.
	// The walk reads each object it reaches once, so the objects the
	// repository holds, plus the ones somebody has already said it does not,
	// is what it will read. Until this had a total it showed a rising number
	// that said nothing about how far along it was. It is still an estimate:
	// a walk with no verdict behind it meets a missing object without warning,
	// and the meter raises its own total when that happens.
	title, total := "Checking what the references reach", s.objectCount()+int64(s.hunt)
	if s.errand {
		// An errand starts under one object rather than at the references,
		// so there is no total: what it will reach is whatever was hidden
		// under that object, which is the thing nobody knows yet.
		title, total = "Checking what came back", 0
	}
	m := s.meters.start(title, total)
	defer m.Finish()

	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.walkWorker(m)
		}()
	}
	wg.Wait()
}

// provenBlob reports whether this object is a blob that has already been read
// and hashed.
//
// A blob points at nothing, so the walk asks it one question: can the
// repository produce it. A pack that has been read end to end has answered that
// for every object in it, by decoding each one and requiring it to hash to the
// name the index gives it. Inflating a blob again to hear the same answer is
// the longest part of this pass, and most of a repository's bytes are blobs.
//
// The type comes from the object's own header rather than from the tree entry
// that named it. A tree that calls a tree a blob is a fault of its own, and it
// must not be able to talk the walk out of a whole subtree.
func (s *scanner) provenBlob(oid gitobj.OID) bool {
	if len(s.trusted) == 0 {
		return false
	}
	typ, p, ok := s.db.TypeInPack(oid)
	return ok && typ == gitobj.TypeBlob && s.trusted[p.File]
}

// walkWorker reads what the bag holds until the walk is finished.
//
// An empty bag does not mean the walk is over: another worker may be part way
// through a tree that will queue a thousand more objects. pending counts both
// what is waiting and what is being read, so zero is the only honest answer to
// "is there any more".
func (s *scanner) walkWorker(m *progress.Meter) {
	for {
		if s.stop.Load() {
			// Every object the fsck could not produce has a route to it
			// now, and the fsck says there is nothing else to find. What
			// is left in the bag is objects that are known to be fine.
			return
		}
		q, ok := s.queue.TryTake()
		if !ok {
			if s.pending.Load() == 0 {
				return
			}
			// Somebody else is still reading. Stand aside rather than
			// spin on the bag's heads.
			runtime.Gosched()
			continue
		}
		m.Step()
		if s.provenBlob(q.oid) {
			// Nothing to read and nothing to follow. This is most of the
			// repository's bytes.
			s.pending.Add(-1)
			continue
		}
		if typ, data, err := s.db.Read(q.oid); err != nil {
			s.note(q, err)
		} else {
			switch typ {
			case gitobj.TypeCommit:
				s.walkCommit(q, data)
			case gitobj.TypeTree:
				s.walkTree(q, data)
			case gitobj.TypeTag:
				s.walkTag(q, data)
			}
		}
		// Last, so that everything this object queued is counted before
		// this one stops counting.
		s.pending.Add(-1)
	}
}

// note records that an object could not be read.
func (s *scanner) note(q queued, err error) {
	need := q.need()
	// copiesOf stats the object directories, so it runs outside the shard
	// lock. A second worker reaching the same object builds the same answer
	// and one of the two is dropped.
	files, corrupt := s.copiesOf(q.oid)
	first := false
	s.bad.Compute(q.oid, func(old *BadObject, loaded bool) (*BadObject, bool) {
		if !loaded {
			old = &BadObject{OID: q.oid, Type: q.typ, Files: files, Corrupt: corrupt}
			first = true
		}
		old.Needs = appendNeed(old.Needs, need)
		return old, false
	})
	if first && s.hunt > 0 && s.found.Add(1) >= int64(s.hunt) {
		s.stop.Store(true)
	}
	// Raised after the object is in the map, so a reader that sees the flag
	// finds the object behind it.
	s.anyBad.Store(true)
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
//
// The links are all in the header, and the header ends at the first empty line.
// Never split the whole object: that copies every byte of every commit message
// into a string, to read the two lines at the top.
func (s *scanner) walkCommit(q queued, data []byte) {
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, newline)
		data = rest
		if len(line) == 0 {
			// The message starts here, and it names nothing.
			return
		}
		if hex, ok := bytes.CutPrefix(line, []byte("tree ")); ok {
			if oid, ok := s.parseLink(hex); ok {
				s.want(oid, gitobj.TypeTree, q.ref, &pathNode{name: q.oid.String() + ":"})
			}
			continue
		}
		if hex, ok := bytes.CutPrefix(line, []byte("parent ")); ok {
			if oid, ok := s.parseLink(hex); ok {
				s.want(oid, gitobj.TypeCommit, q.ref, nil)
			}
		}
	}
}

var newline = []byte("\n")

// parseLink reads the object name off a header line.
func (s *scanner) parseLink(hex []byte) (gitobj.OID, bool) {
	return s.repo.Algo.Parse(string(bytes.TrimSpace(hex)))
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
		s.wantEntry(e.OID, typ, q.ref, q.path, e.Name)
	}
}

// walkTag follows a tag to what it names.
func (s *scanner) walkTag(q queued, data []byte) {
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, newline)
		data = rest
		if len(line) == 0 {
			return
		}
		if hex, ok := bytes.CutPrefix(line, []byte("object ")); ok {
			if oid, ok := s.parseLink(hex); ok {
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

// objectCount is how many objects the repository holds, packed and loose.
//
// It is the number the walk's meter counts against. A pack that will not open
// contributes nothing, which is right: the walk cannot read what is in it
// either.
func (s *scanner) objectCount() int64 {
	total := int64(0)
	for _, p := range s.db.Packs() {
		if p.OpenErr == nil {
			total += int64(p.Num)
		}
	}
	hexsz := s.repo.Algo.HexSize
	for _, dir := range s.db.Dirs {
		for i := range 256 {
			entries, err := os.ReadDir(filepath.Join(dir.Path, fmt.Sprintf("%02x", i)))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if len(e.Name()) == hexsz-2 {
					total++
				}
			}
		}
	}
	return total
}
