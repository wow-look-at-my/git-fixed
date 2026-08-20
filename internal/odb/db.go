package odb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wow-look-at-my/go-containers/set"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Dir is one object directory: the repository's own, or an alternate.
type Dir struct {
	// Display is the path git would print for this directory.
	Display string
	Path    string
	Packs   []*Pack
}

// DB is every object directory a repository can read from.
type DB struct {
	Algo             *gitobj.Algo
	Dirs             []*Dir
	BigFileThreshold int64

	packs []*Pack

	inflaters sync.Pool

	// bad lists packed objects that would not decode, the way a packed_git
	// keeps its own bad-object list.
	badMu sync.Mutex
	bad   set.Set[gitobj.OID]
}

// Open maps the object directory and its alternates. The sequential flag says
// the caller will read every pack end to end.
func Open(objectsDir, displayDir string, algo *gitobj.Algo, sequential bool) (*DB, error) {
	db := &DB{Algo: algo, BigFileThreshold: 512 * 1024 * 1024, bad: set.New[gitobj.OID]()}
	db.inflaters.New = func() any { return &Inflater{} }
	seen := set.New[string]()
	var add func(path, display string, depth int) error
	add = func(path, display string, depth int) error {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if depth > 5 || !seen.Add(abs) {
			return nil
		}
		d := &Dir{Path: path, Display: display}
		db.Dirs = append(db.Dirs, d)
		// An alternate is named by an absolute path, or by a path
		// relative to this directory, and git prints it as it resolved
		// it either way.
		for _, alt := range readAlternates(path) {
			if err := add(alt, alt, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(objectsDir, displayDir, 0); err != nil {
		return nil, err
	}
	for _, d := range db.Dirs {
		if err := d.loadPacks(algo, sequential); err != nil {
			return nil, err
		}
		db.packs = append(db.packs, d.Packs...)
	}
	return db, nil
}

// Close releases every mapping the database holds.
func (db *DB) Close() {
	for _, p := range db.packs {
		p.Close()
	}
}

// Packs lists every pack, in the order git would walk them.
func (db *DB) Packs() []*Pack { return db.packs }

func (d *Dir) loadPacks(algo *gitobj.Algo, sequential bool) error {
	entries, err := os.ReadDir(filepath.Join(d.Path, "pack"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pack-") && strings.HasSuffix(e.Name(), ".idx") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		file := filepath.Join(d.Path, "pack", n)
		shown := filepath.Join(d.Display, "pack", n)
		p, err := OpenPack(file, shown, algo, sequential)
		if err != nil {
			// git skips an index it cannot map and says so once the
			// caller asks it to verify the pack. Record the failure
			// as a broken pack so nothing is dropped in silence.
			d.Packs = append(d.Packs, &Pack{
				IdxPath: shown,
				IdxFile: file,
				Path:    strings.TrimSuffix(shown, ".idx") + ".pack",
				File:    strings.TrimSuffix(file, ".idx") + ".pack",
				Algo:    algo,
				OpenErr: err,
			})
			continue
		}
		d.Packs = append(d.Packs, p)
	}
	return nil
}

// readAlternates parses objects/info/alternates the way git's
// link_alt_odb_entries() does.
func readAlternates(objectsDir string) []string {
	data, err := os.ReadFile(filepath.Join(objectsDir, "info", "alternates"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '"' {
			if unq, ok := unquoteC(line); ok {
				line = unq
			} else {
				continue
			}
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(objectsDir, line)
		}
		out = append(out, filepath.Clean(line))
	}
	return out
}

// unquoteC undoes git's quote_c_style() for an alternates entry.
func unquoteC(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' {
		return "", false
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		i++
		switch c {
		case '"':
			return b.String(), true
		case '\\':
			if i >= len(s) {
				return "", false
			}
			e := s[i]
			i++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte(7)
			case 'b':
				b.WriteByte(8)
			case 'f':
				b.WriteByte(12)
			case 'v':
				b.WriteByte(11)
			case '"', '\\':
				b.WriteByte(e)
			default:
				if e >= '0' && e <= '7' && i+1 < len(s) {
					v := int(e - '0')
					for k := 0; k < 2 && i < len(s) && s[i] >= '0' && s[i] <= '7'; k++ {
						v = v*8 + int(s[i]-'0')
						i++
					}
					b.WriteByte(byte(v))
				} else {
					return "", false
				}
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

// Location says where an object lives.
type Location struct {
	Pack      *Pack
	PackIdx   uint32
	Loose     bool
	LoosePath string
	// LooseShown is the same file as git would name it in a message.
	LooseShown string
}

// Find locates an object. git looks in packs before loose files, and so do we.
func (db *DB) Find(oid gitobj.OID) (Location, bool) {
	for _, p := range db.packs {
		if p.OpenErr != nil {
			continue
		}
		if i, ok := p.Find(oid); ok {
			return Location{Pack: p, PackIdx: i}, true
		}
	}
	hex := oid.String()
	for _, d := range db.Dirs {
		path := filepath.Join(d.Path, hex[:2], hex[2:])
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			return Location{
				Loose:      true,
				LoosePath:  path,
				LooseShown: filepath.Join(d.Display, hex[:2], hex[2:]),
			}, true
		}
	}
	return Location{}, false
}

// Has reports whether the object exists anywhere in the database.
func (db *DB) Has(oid gitobj.OID) bool {
	_, ok := db.Find(oid)
	return ok
}

// Read returns an object's type and content, resolving deltas as needed.
func (db *DB) Read(oid gitobj.OID) (gitobj.Type, []byte, error) {
	loc, ok := db.Find(oid)
	if !ok {
		return gitobj.TypeNone, nil, fmt.Errorf("object %s is missing", oid)
	}
	if loc.Loose {
		res := ReadLoose(loc.LoosePath, loc.LooseShown, oid, db.Algo, 1<<62)
		if res.Failed {
			if len(res.Errors) > 0 {
				return gitobj.TypeNone, nil, fmt.Errorf("%s", res.Errors[0])
			}
			return gitobj.TypeNone, nil, fmt.Errorf("object %s is corrupt", oid)
		}
		return res.Type, res.Contents, nil
	}
	in := db.inflaters.Get().(*Inflater)
	defer db.inflaters.Put(in)
	typ, data, err := db.readPacked(loc.Pack, loc.Pack.OffsetAt(loc.PackIdx), in, 0)
	if err != nil {
		// git remembers a packed object that would not decode and lets
		// the first reader report it. The next reader of the same
		// object finds it on that list and dies instead.
		if db.markBad(oid) {
			return gitobj.TypeNone, nil, corruptPacked(loc.Pack, oid)
		}
		return gitobj.TypeNone, nil, err
	}
	return typ, data, nil
}

// markBad records a packed object that would not decode, and reports whether it
// was already on the list.
func (db *DB) markBad(oid gitobj.OID) bool {
	db.badMu.Lock()
	defer db.badMu.Unlock()
	return !db.bad.Add(oid)
}

// MarkBadPacked puts an object on the bad list without reading it, for a caller
// that found the problem some other way, such as a full pack check.
func (db *DB) MarkBadPacked(oid gitobj.OID) { db.markBad(oid) }

// maxDeltaChain bounds delta recursion. git's own limit for writing is 50; a
// pack that exceeds this by a wide margin is corrupt or hostile.
const maxDeltaChain = 4096

func (db *DB) readPacked(p *Pack, off int64, in *Inflater, depth int) (gitobj.Type, []byte, error) {
	if depth > maxDeltaChain {
		return gitobj.TypeNone, nil, fmt.Errorf("delta chain in %s is too deep", p.Path)
	}
	h, err := p.ReadHeader(off)
	if err != nil {
		return gitobj.TypeNone, nil, err
	}
	switch h.Type {
	case gitobj.TypeOfsDelta, gitobj.TypeRefDelta:
		var baseType gitobj.Type
		var base []byte
		if h.Type == gitobj.TypeOfsDelta {
			baseType, base, err = db.readPacked(p, h.BaseOff, in, depth+1)
		} else {
			if i, ok := p.Find(h.BaseOID); ok {
				baseType, base, err = db.readPacked(p, p.OffsetAt(i), in, depth+1)
			} else {
				baseType, base, err = db.Read(h.BaseOID)
			}
		}
		if err != nil {
			return gitobj.TypeNone, nil, err
		}
		delta, err := in.Inflate(p, h.DataOff, h.Size)
		if err != nil {
			return gitobj.TypeNone, nil, err
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return gitobj.TypeNone, nil, err
		}
		return baseType, out, nil
	default:
		data, err := in.Inflate(p, h.DataOff, h.Size)
		if err != nil {
			return gitobj.TypeNone, nil, err
		}
		return h.Type, data, nil
	}
}

// Info returns an object's type and size without decoding its payload.
func (db *DB) Info(oid gitobj.OID) (gitobj.Type, int64, error) {
	loc, ok := db.Find(oid)
	if !ok {
		return gitobj.TypeNone, 0, fmt.Errorf("object %s is missing", oid)
	}
	if loc.Loose {
		res := ReadLoose(loc.LoosePath, loc.LooseShown, oid, db.Algo, 0)
		if res.Type == gitobj.TypeNone && res.TypeName == "" {
			return gitobj.TypeNone, 0, fmt.Errorf("object %s is corrupt", oid)
		}
		return res.Type, res.Size, nil
	}
	in := db.inflaters.Get().(*Inflater)
	defer db.inflaters.Put(in)
	p, off := loc.Pack, loc.Pack.OffsetAt(loc.PackIdx)
	for depth := 0; depth <= maxDeltaChain; depth++ {
		h, err := p.ReadHeader(off)
		if err != nil {
			return gitobj.TypeNone, 0, err
		}
		switch h.Type {
		case gitobj.TypeOfsDelta:
			off = h.BaseOff
		case gitobj.TypeRefDelta:
			i, found := p.Find(h.BaseOID)
			if !found {
				return db.Info(h.BaseOID)
			}
			off = p.OffsetAt(i)
		default:
			return h.Type, h.Size, nil
		}
	}
	return gitobj.TypeNone, 0, fmt.Errorf("delta chain in %s is too deep", p.Path)
}

// HasPacked reports whether the object is in a pack. git treats that as proof
// the object is really there, even when a loose file of the same name is
// unreadable.
func (db *DB) HasPacked(oid gitobj.OID) bool {
	for _, p := range db.packs {
		if p.OpenErr != nil {
			continue
		}
		if _, ok := p.Find(oid); ok {
			return true
		}
	}
	return false
}

// FatalError is a condition git reports with "fatal:" and exit status 128. A
// packed object that will not decode is the one that matters here: git dies
// rather than carry on with a repository it cannot read.
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// corruptPacked builds the message git dies with when a packed object will not
// decode. It names the object and the pack, as git's unpack_entry() does.
func corruptPacked(p *Pack, oid gitobj.OID) error {
	return &FatalError{Msg: fmt.Sprintf("packed object %s (stored in %s) is corrupt", oid, p.Path)}
}
