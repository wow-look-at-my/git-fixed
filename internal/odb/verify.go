package odb

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// VerifyOpts configures a full pack check.
type VerifyOpts struct {
	// Emit receives one diagnostic.
	Emit func(oid gitobj.OID, text string)
	// Object receives every object the pack holds, once it is known to decode and to hash to its recorded name.
	Object func(oid gitobj.OID, typ gitobj.Type, size int64, data []byte)
	// Workers is the number of goroutines that decode objects.
	Workers int
	// BigFileThreshold matches core.bigFileThreshold.
	BigFileThreshold int64
	// Progress is called once per object finished, from every worker.
	Progress func()
	// ChainBudget bounds the decoded delta bases the walk holds at once.
	ChainBudget int64
}

// DefaultChainBudget is how many bytes of decoded delta bases one pack's walk may hold at a time.
const DefaultChainBudget = 256 << 20

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
	// The pack's own hash is one thread reading every byte of the pack.
	sums := make(chan []string, 1)
	go func() { sums <- p.checksumComplaints() }()
	objectsOK := p.verifyObjects(o)
	for _, text := range <-sums {
		fail(text)
	}
	if !objectsOK {
		ok = false
	}
	return ok
}

// checksumComplaints hashes the whole pack and compares it with the two copies of that hash the repository.
func (p *Pack) checksumComplaints() []string {
	var out []string
	rawsz := int64(p.Algo.RawSize)
	sigOff := p.dataSize - rawsz
	sum, err := p.hashThrough(sigOff)
	if err != nil {
		// The pack is mapped, so it opened once already.
		return []string{fmt.Sprintf("%s cannot be read: %s", p.Path, errnoText(err))}
	}
	if !bytes.Equal(sum, p.data[sigOff:]) {
		out = append(out, fmt.Sprintf("%s pack checksum mismatch", p.Path))
	}
	if !bytes.Equal(p.idx[p.idxSize-2*rawsz:p.idxSize-rawsz], p.data[sigOff:]) {
		out = append(out, fmt.Sprintf("%s pack checksum does not match its index", p.Path))
	}
	return out
}

// hashBuf is what makes this pass one read instead of a fault storm.
const hashBuf = 1 << 20

// hashThrough hashes the first n bytes of the pack file.

func (p *Pack) hashThrough(n int64) ([]byte, error) {
	f, err := os.Open(p.File)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := p.Algo.New()
	if _, err := io.CopyBuffer(h, io.LimitReader(f, n), make([]byte, hashBuf)); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
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

// failSubtree reports every delta standing on an entry that could not be produced.
func failSubtree(emit func(gitobj.OID, string), p *Pack, l *packLayout, root int32) {
	stack := append([]int32(nil), l.children(root)...)
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		e := l.ents[i]
		cannotUnpack(emit, p, l, p.OIDAt(e.idx), e)
		stack = append(stack, l.children(i)...)
	}
}

// packEntry is one object as the pack stores it, in offset order.
type packEntry struct {
	off     int64
	dataOff int64
	size    int64
	end     int64
	idx     uint32 // position in index order
	// headerErr indexes packLayout.headerErrs, whose first element is the empty string.
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
	// parents is the other direction, which is how a delta the walk could not afford to hold gets rebuilt later.
	parents []int32
	roots   []int32
	bad     []int32
	// headerErrs holds what stopped an entry header from being read, which happens before anything decompresses.
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
	// The sort moves a 16-byte pair, not the whole 48-byte packEntry, on a quarter of a million entries.
	order := make([]offIdx, n)
	for i := range order {
		order[i] = offIdx{off: p.OffsetAt(uint32(i)), idx: uint32(i)}
	}
	slices.SortFunc(order, func(a, b offIdx) int { return cmp.Compare(a.off, b.off) })

	l := &packLayout{ents: make([]packEntry, n), headerErrs: []string{""}}
	for i, o := range order {
		l.ents[i] = packEntry{off: o.off, idx: o.idx}
	}

	// posOf maps an index-order position to an offset-order position.
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
				break
			}
			// git's get_delta_base() looks in this pack and nowhere else.
			l.bad = append(l.bad, int32(i))
			l.ents[i].typ = gitobj.TypeBad
			l.headerErrs = append(l.headerErrs, badDeltaBase(h.DataOff, p.Path).Error())
			l.ents[i].headerErr = int32(len(l.headerErrs) - 1)
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
	l.parents = parent
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
	// Object is called from every worker at once, without a lock: it is the whole per-object check.
	object := o.Object

	for _, i := range l.bad {
		e := l.ents[i]
		oid := p.OIDAt(e.idx)
		cannotUnpack(emit, p, l, oid, e)
		failSubtree(emit, p, l, i)
	}

	// The index records a CRC over each entry's raw bytes. Checking those is
	// a flat scan of the mapping, so it parallelizes on its own.
	if p.IdxVer > 1 {
		p.checkCRCs(l, workers, emit)
	}

	w := &walker{p: p, l: l, o: &o, emit: emit, object: object}
	budget := o.ChainBudget
	if budget <= 0 {
		budget = DefaultChainBudget
	}
	w.budget.Store(budget)
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
	// budget is the decoded base data every worker on this pack may still hold between them. see DefaultChainBudget.
	budget atomic.Int64
}

// frame is one level of an in-progress delta chain.
type frame struct {
	entry int32
	data  []byte
	next  int32 // position in childList of the next child to visit
}

// take reserves room to hold one decoded base. The bottom of a worker's stack
// is always allowed: a worker that could hold nothing would decode nothing.
func (w *walker) take(depth int, n int64) bool {
	if depth == 0 {
		return true
	}
	for {
		cur := w.budget.Load()
		if cur < n {
			return false
		}
		if w.budget.CompareAndSwap(cur, cur-n) {
			return true
		}
	}
}

// give hands a reservation back.
func (w *walker) give(depth int, n int64) {
	if depth == 0 {
		return
	}
	w.budget.Add(n)
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
		failSubtree(w.emit, p, l, root)
		return
	}
	w.finish(root, typ, data)
	if len(l.children(root)) == 0 {
		return
	}
	// Anything the spread could not afford to hold comes back here, already checked.
	deferred := w.spread(root, typ, data, in, nil)
	for len(deferred) > 0 {
		d := deferred[len(deferred)-1]
		deferred = deferred[:len(deferred)-1]
		data, err := w.rebuild(d, in)
		if err != nil {
			cannotUnpack(w.emit, p, l, p.OIDAt(l.ents[d].idx), l.ents[d])
			failSubtree(w.emit, p, l, d)
			continue
		}
		deferred = w.spread(d, typ, data, in, deferred)
	}
}

// spread builds every delta standing on base, and every delta standing on those,
// in one pass down the chain. base is spread's to drop.
//
// It holds one decoded object per level, and releases a level as soon as
// nothing below will read it again: descending into a node's last child hands
// the buffer over rather than stacking a second one on top of it, which is the
// whole of a chain that never branches. Past the budget a child is returned for
// the caller to rebuild later instead of being held here.
func (w *walker) spread(base int32, typ gitobj.Type, data []byte, in *Inflater, deferred []int32) []int32 {
	l, p := w.l, w.p
	stack := []frame{{entry: base, data: data, next: l.childStart[base]}}
	for len(stack) > 0 {
		i := len(stack) - 1
		top := &stack[i]
		end := l.childStart[top.entry+1]
		if top.next >= end {
			w.give(i, int64(len(top.data)))
			stack = stack[:i]
			continue
		}
		child := l.childList[top.next]
		top.next++
		last := top.next >= end
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
			cannotUnpack(w.emit, p, l, p.OIDAt(ce.idx), *ce)
			failSubtree(w.emit, p, l, child)
			continue
		}
		w.finish(child, typ, out)
		if l.childStart[child+1] == l.childStart[child] {
			continue
		}
		if last {
			// The parent has no more children, so nothing will read it again.
			w.give(i, int64(len(top.data)))
			if !w.take(i, int64(len(out))) {
				deferred = append(deferred, child)
				stack = stack[:i]
				continue
			}
			stack[i] = frame{entry: child, data: out, next: l.childStart[child]}
			continue
		}
		if !w.take(i+1, int64(len(out))) {
			deferred = append(deferred, child)
			continue
		}
		stack = append(stack, frame{entry: child, data: out, next: l.childStart[child]})
	}
	return deferred
}

// rebuild decodes one entry from the bottom of its own chain, for a delta the
// walk could not afford to keep when it first passed it. It holds two objects
// at a time rather than the chain.
func (w *walker) rebuild(i int32, in *Inflater) ([]byte, error) {
	l := w.l
	var path []int32
	for j := i; ; {
		path = append(path, j)
		if l.parents[j] < 0 {
			break
		}
		j = l.parents[j]
	}
	_, data, err := w.materializeRoot(&l.ents[path[len(path)-1]], in)
	if err != nil {
		return nil, err
	}
	for k := len(path) - 2; k >= 0; k-- {
		e := &l.ents[path[k]]
		delta, err := in.Inflate(w.p, e.dataOff, e.size)
		if err != nil {
			return nil, err
		}
		if data, err = applyDelta(data, delta); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// materializeRoot decodes a non-delta entry, or a ref-delta whose base lives
// outside this pack.
func (w *walker) materializeRoot(e *packEntry, in *Inflater) (gitobj.Type, []byte, error) {
	p := w.p
	switch e.typ {
	case gitobj.TypeRefDelta, gitobj.TypeOfsDelta:
		// buildLayout marks an unresolved delta bad, and walkChain drops a bad entry before it reaches here.
		return gitobj.TypeBad, nil, badDeltaBase(e.dataOff, p.Path)
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
	if w.o.Progress != nil {
		w.o.Progress()
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
	if w.o.Progress != nil {
		w.o.Progress()
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
