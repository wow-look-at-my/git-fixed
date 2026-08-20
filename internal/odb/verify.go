package odb

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// VerifyOpts configures a full pack check.
type VerifyOpts struct {
	// Emit receives one diagnostic. oid is the zero value for a problem
	// with the pack as a whole rather than with one object.
	Emit func(oid gitobj.OID, text string)
	// Object receives every object the pack holds, once it is known to
	// decode and to hash to its recorded name. Several workers call it at
	// once, and data is only valid until it returns.
	Object func(oid gitobj.OID, typ gitobj.Type, size int64, data []byte)
	// Workers is the number of goroutines that decode objects.
	Workers int
	// BigFileThreshold matches core.bigFileThreshold: a larger undeltified
	// blob is hashed by streaming instead of being held in memory.
	BigFileThreshold int64
	// Progress is called with the count of objects finished so far.
	Progress func(done int)
}

// Verify checks a pack the way git's verify_pack() does, in parallel.
//
// see docs/pack-verification.md
func (p *Pack) Verify(o VerifyOpts) bool {
	ok := true
	fail := func(text string) {
		ok = false
		o.Emit(gitobj.OID{}, text)
	}
	if p.OpenErr != nil {
		fail(fmt.Sprintf("packfile %s index not opened", p.Path))
		return false
	}
	if !p.verifyIndexChecksum() {
		fail(fmt.Sprintf("Packfile index for %s hash mismatch", p.Path))
	}
	if inner, valid := p.validatePackHeader(); !valid {
		if inner != "" {
			o.Emit(gitobj.OID{}, inner)
		}
		fail(fmt.Sprintf("packfile %s cannot be accessed", p.Path))
		return false
	}
	rawsz := int64(p.Algo.RawSize)
	sigOff := p.dataSize - rawsz
	h := p.Algo.New()
	h.Write(p.data[:sigOff])
	if !bytes.Equal(h.Sum(nil), p.data[sigOff:]) {
		fail(fmt.Sprintf("%s pack checksum mismatch", p.Path))
	}
	if !bytes.Equal(p.idx[p.idxSize-2*rawsz:p.idxSize-rawsz], p.data[sigOff:]) {
		fail(fmt.Sprintf("%s pack checksum does not match its index", p.Path))
	}
	if !p.verifyObjects(o) {
		ok = false
	}
	return ok
}

// verifyIndexChecksum checks the index file's own trailing hash.
func (p *Pack) verifyIndexChecksum() bool {
	rawsz := int64(p.Algo.RawSize)
	if p.idxSize < rawsz {
		return false
	}
	h := p.Algo.New()
	h.Write(p.idx[:p.idxSize-rawsz])
	return bytes.Equal(h.Sum(nil), p.idx[p.idxSize-rawsz:])
}

// validatePackHeader repeats the checks git makes when it first opens a pack.
// It returns the specific complaint, and the caller adds git's generic one.
func (p *Pack) validatePackHeader() (string, bool) {
	rawsz := int64(p.Algo.RawSize)
	if p.dataSize < 12+rawsz {
		return fmt.Sprintf("file %s is far too short to be a packfile", p.Path), false
	}
	if string(p.data[0:4]) != "PACK" {
		return fmt.Sprintf("file %s is not a GIT packfile", p.Path), false
	}
	ver := binary.BigEndian.Uint32(p.data[4:8])
	if ver != 2 && ver != 3 {
		return fmt.Sprintf("packfile %s is version %d and not supported (try upgrading GIT to a newer version)", p.Path, ver), false
	}
	if n := binary.BigEndian.Uint32(p.data[8:12]); n != p.Num {
		return fmt.Sprintf("packfile %s claims to have %d objects while index indicates %d objects", p.Path, n, p.Num), false
	}
	if !bytes.Equal(p.data[p.dataSize-rawsz:], p.idx[p.idxSize-2*rawsz:p.idxSize-rawsz]) {
		return fmt.Sprintf("packfile %s does not match index", p.Path), false
	}
	return "", true
}

// cannotUnpack reports an entry that will not decode. Whatever stopped the read
// says so first, and only then does its caller add the line naming the entry.
// An entry whose own header is unreadable never reaches the decompressor.
func cannotUnpack(emit func(gitobj.OID, string), p *Pack, l *packLayout, oid gitobj.OID, e packEntry) {
	switch {
	case l.headerErr(e) != "":
		emit(oid, l.headerErr(e))
	default:
		if msg := p.InflateMessage(e.dataOff, e.size); msg != "" {
			emit(oid, msg)
		}
	}
	emit(oid, fmt.Sprintf("cannot unpack %s from %s at offset %d", oid, p.Path, e.off))
}

// packEntry is one object as the pack stores it, in offset order. It holds no
// pointer on purpose: there is one entry per object, so a pointer here puts
// every allocation and every sweep of the slice through the write barrier. The
// one string an entry could carry lives in packLayout.headerErrs instead.
type packEntry struct {
	off     int64
	dataOff int64
	size    int64
	end     int64
	idx     uint32 // position in index order
	// headerErr indexes packLayout.headerErrs, whose first element is the
	// empty string, so a zero entry means the header read fine.
	headerErr int32
	typ       gitobj.Type
}

// offIdx is one entry's offset and its position in index order. It exists so
// the offset sort moves no pointers.
type offIdx struct {
	off int64
	idx uint32
}

// packLayout is every entry in offset order, plus the base-to-children links
// that let each delta chain be decoded exactly once.
type packLayout struct {
	ents []packEntry
	// The children of entry i are childList[childStart[i]:childStart[i+1]].
	childStart []int32
	childList  []int32
	roots      []int32
	bad        []int32
	// headerErrs holds what stopped an entry header from being read, which
	// happens before anything decompresses. Element 0 is the empty string.
	headerErrs []string
}

// headerErr returns why an entry's header would not read, or the empty string.
func (l *packLayout) headerErr(e packEntry) string { return l.headerErrs[e.headerErr] }

func (l *packLayout) children(i int32) []int32 {
	return l.childList[l.childStart[i]:l.childStart[i+1]]
}

// buildLayout reads every entry header and links each delta to its base.
func (p *Pack) buildLayout() *packLayout {
	n := int(p.Num)
	// The sort moves a 16-byte pair, not the whole 48-byte packEntry, on a
	// quarter of a million entries. This sort runs on the main goroutine,
	// where a serial cost is the whole run's cost.
	order := make([]offIdx, n)
	for i := range order {
		order[i] = offIdx{off: p.OffsetAt(uint32(i)), idx: uint32(i)}
	}
	slices.SortFunc(order, func(a, b offIdx) int { return cmp.Compare(a.off, b.off) })

	l := &packLayout{ents: make([]packEntry, n), headerErrs: []string{""}}
	for i, o := range order {
		l.ents[i] = packEntry{off: o.off, idx: o.idx}
	}

	// posOf maps an index-order position to an offset-order position, so a
	// ref-delta's base is found without a map.
	posOf := make([]int32, n)
	for pos := range l.ents {
		posOf[l.ents[pos].idx] = int32(pos)
	}
	trailer := p.TrailerOffset()
	for i := range l.ents {
		if i+1 < n {
			l.ents[i].end = l.ents[i+1].off
		} else {
			l.ents[i].end = trailer
		}
	}

	parent := make([]int32, n)
	for i := range parent {
		parent[i] = -1
	}
	for i := range l.ents {
		h, err := p.ReadHeader(l.ents[i].off)
		if err != nil {
			l.bad = append(l.bad, int32(i))
			l.ents[i].typ = gitobj.TypeBad
			l.headerErrs = append(l.headerErrs, err.Error())
			l.ents[i].headerErr = int32(len(l.headerErrs) - 1)
			continue
		}
		l.ents[i].typ = h.Type
		l.ents[i].size = h.Size
		l.ents[i].dataOff = h.DataOff
		switch h.Type {
		case gitobj.TypeOfsDelta:
			pos := sort.Search(n, func(k int) bool { return l.ents[k].off >= h.BaseOff })
			if pos < n && l.ents[pos].off == h.BaseOff {
				parent[i] = int32(pos)
			} else {
				l.bad = append(l.bad, int32(i))
				l.ents[i].typ = gitobj.TypeBad
			}
		case gitobj.TypeRefDelta:
			if bi, ok := p.Find(h.BaseOID); ok {
				parent[i] = posOf[bi]
			}
			// A base outside this pack leaves this entry a root, and
			// the worker resolves it through the whole database.
		}
	}

	l.childStart = make([]int32, n+1)
	for i := range parent {
		if parent[i] >= 0 {
			l.childStart[parent[i]+1]++
		}
	}
	for i := 1; i <= n; i++ {
		l.childStart[i] += l.childStart[i-1]
	}
	fill := make([]int32, n)
	copy(fill, l.childStart[:n])
	l.childList = make([]int32, l.childStart[n])
	for i := range parent {
		if parent[i] >= 0 {
			l.childList[fill[parent[i]]] = int32(i)
			fill[parent[i]]++
		}
	}
	for i := range l.ents {
		if parent[i] < 0 {
			l.roots = append(l.roots, int32(i))
		}
	}
	return l
}

// verifyObjects decodes every object in the pack and hands it to the caller.
func (p *Pack) verifyObjects(o VerifyOpts) bool {
	workers := o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	l := p.buildLayout()

	var mu sync.Mutex
	good := true
	emit := func(oid gitobj.OID, text string) {
		mu.Lock()
		good = false
		o.Emit(oid, text)
		mu.Unlock()
	}
	// Object is called from every worker at once, without a lock: it is the
	// whole per-object check, so serializing it would leave one core doing
	// all the work while the rest decode ahead of it. Emit takes the lock
	// instead, because a pack that emits anything at all is broken and rare.
	object := o.Object

	for _, i := range l.bad {
		e := l.ents[i]
		oid := p.OIDAt(e.idx)
		cannotUnpack(emit, p, l, oid, e)
	}

	// The index records a CRC over each entry's raw bytes. Checking those is
	// a flat scan of the mapping, so it parallelizes on its own.
	if p.IdxVer > 1 {
		p.checkCRCs(l, workers, emit)
	}

	w := &walker{p: p, l: l, o: &o, emit: emit, object: object}
	var wg sync.WaitGroup
	var next int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := &Inflater{}
			for {
				ri := int(atomic.AddInt64(&next, 1)) - 1
				if ri >= len(l.roots) {
					return
				}
				w.walkChain(l.roots[ri], in)
			}
		}()
	}
	wg.Wait()
	if o.Progress != nil {
		o.Progress(int(atomic.LoadInt64(&w.done)))
	}
	return good
}

// checkCRCs verifies the index's CRC over every entry's compressed bytes.
func (p *Pack) checkCRCs(l *packLayout, workers int, emit func(gitobj.OID, string)) {
	var next int64
	var wg sync.WaitGroup
	const batch = 512
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				start := int(atomic.AddInt64(&next, batch)) - batch
				if start >= len(l.ents) {
					return
				}
				end := min(start+batch, len(l.ents))
				for i := start; i < end; i++ {
					e := l.ents[i]
					if e.off < 0 || e.end > int64(len(p.data)) || e.end < e.off {
						continue
					}
					if crc32.ChecksumIEEE(p.data[e.off:e.end]) != p.CRCAt(e.idx) {
						oid := p.OIDAt(e.idx)
						emit(oid, fmt.Sprintf("index CRC mismatch for object %s from %s at offset %d",
							oid, p.Path, e.off))
					}
				}
			}
		}()
	}
	wg.Wait()
}

// walker decodes delta chains for one pack.
type walker struct {
	p      *Pack
	l      *packLayout
	o      *VerifyOpts
	emit   func(gitobj.OID, string)
	object func(gitobj.OID, gitobj.Type, int64, []byte)
	done   int64
}

// frame is one level of an in-progress delta chain.
type frame struct {
	entry int32
	data  []byte
	next  int32 // position in childList of the next child to visit
}

// walkChain decodes one base object and every delta built on it, reusing the
// parent's buffer instead of decoding a chain again for each of its children.
func (w *walker) walkChain(root int32, in *Inflater) {
	l, p := w.l, w.p
	e := &l.ents[root]
	if e.typ == gitobj.TypeBad {
		return
	}
	if e.typ == gitobj.TypeBlob && w.o.BigFileThreshold > 0 && e.size >= w.o.BigFileThreshold &&
		len(l.children(root)) == 0 {
		w.finishStreamed(root)
		return
	}
	typ, data, err := w.materializeRoot(e, in)
	if err != nil {
		oid := p.OIDAt(e.idx)
		cannotUnpack(w.emit, p, l, oid, *e)
		return
	}
	w.finish(root, typ, data)
	if len(l.children(root)) == 0 {
		return
	}
	stack := []frame{{entry: root, data: data, next: l.childStart[root]}}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next >= l.childStart[top.entry+1] {
			stack = stack[:len(stack)-1]
			continue
		}
		child := l.childList[top.next]
		top.next++
		ce := &l.ents[child]
		if ce.typ == gitobj.TypeBad {
			continue
		}
		delta, err := in.Inflate(p, ce.dataOff, ce.size)
		var out []byte
		if err == nil {
			out, err = applyDelta(top.data, delta)
		}
		if err != nil {
			oid := p.OIDAt(ce.idx)
			cannotUnpack(w.emit, p, l, oid, *ce)
			continue
		}
		w.finish(child, typ, out)
		if len(l.children(child)) > 0 {
			stack = append(stack, frame{entry: child, data: out, next: l.childStart[child]})
		}
	}
}

// materializeRoot decodes a non-delta entry, or a ref-delta whose base lives
// outside this pack.
func (w *walker) materializeRoot(e *packEntry, in *Inflater) (gitobj.Type, []byte, error) {
	p := w.p
	switch e.typ {
	case gitobj.TypeRefDelta, gitobj.TypeOfsDelta:
		return gitobj.TypeBad, nil, fmt.Errorf("delta base is unresolved in %s", p.Path)
	}
	data, err := in.Inflate(p, e.dataOff, e.size)
	if err != nil {
		return gitobj.TypeBad, nil, err
	}
	return e.typ, data, nil
}

// finishStreamed hashes a blob past core.bigFileThreshold without holding it,
// which is what git's verify_packfile() does. It applies only to an entry
// nothing deltas against, because a base must be in memory to build a child
// from it. The caller sees a nil payload, and fsck reports a .gitmodules or
// .gitattributes blob this large as too large to parse, as git does.
func (w *walker) finishStreamed(i int32) {
	e := &w.l.ents[i]
	oid := w.p.OIDAt(e.idx)
	got, err := w.p.StreamHash(e.dataOff, e.size, e.typ)
	if err != nil {
		cannotUnpack(w.emit, w.p, w.l, oid, *e)
		return
	}
	if got != oid {
		w.emit(oid, fmt.Sprintf("packed %s from %s is corrupt", oid, w.p.Path))
		return
	}
	if w.object != nil {
		w.object(oid, e.typ, e.size, nil)
	}
	n := atomic.AddInt64(&w.done, 1)
	if w.o.Progress != nil && n&1023 == 0 {
		w.o.Progress(int(n))
	}
}

// finish hashes one decoded object and hands it to the caller.
func (w *walker) finish(i int32, typ gitobj.Type, data []byte) {
	e := &w.l.ents[i]
	oid := w.p.OIDAt(e.idx)
	if HashLiteral(w.p.Algo, typ.Name(), data) != oid {
		w.emit(oid, fmt.Sprintf("packed %s from %s is corrupt", oid, w.p.Path))
		return
	}
	if w.object != nil {
		w.object(oid, typ, int64(len(data)), data)
	}
	n := atomic.AddInt64(&w.done, 1)
	if w.o.Progress != nil && n&1023 == 0 {
		w.o.Progress(int(n))
	}
}

// StreamHash hashes a pack entry without holding its payload, for a blob past
// core.bigFileThreshold.
func (p *Pack) StreamHash(dataOff, size int64, typ gitobj.Type) (gitobj.OID, error) {
	in := &Inflater{}
	r, err := in.InflateStream(p, dataOff)
	if err != nil {
		return gitobj.OID{}, err
	}
	h := p.Algo.New()
	fmt.Fprintf(h, "%s %d", typ.Name(), size)
	h.Write([]byte{0})
	if _, err := io.Copy(h, io.LimitReader(r, size)); err != nil {
		return gitobj.OID{}, err
	}
	return gitobj.FromBytes(h.Sum(nil)), nil
}
