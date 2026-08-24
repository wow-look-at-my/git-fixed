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
	// ReleaseEvery is how much of the pack is read before its pages go back. see releaseEvery.
	ReleaseEvery int64
}

// DefaultChainBudget is how many bytes of decoded delta bases one pack's walk may hold at a time.
const DefaultChainBudget = 256 << 20

// releaseEvery is how much of a pack a pass reads before it hands the pages back. see docs/memory.md
const releaseEvery = 256 << 20

// releaser hands a pack's pages back as a pass reads past them. A pack can be
// larger than the machine, so a sweep at the end of one is too late.
type releaser struct {
	p     *Pack
	every int64
	// read is how many of the pack's bytes have been read since the last sweep.
	read atomic.Int64
}

// newReleaser builds one for a pack, at the caller's interval or the default.
func newReleaser(p *Pack, every int64) *releaser {
	if every <= 0 {
		every = releaseEvery
	}
	return &releaser{p: p, every: every}
}

// spent counts bytes as read, and sweeps once enough of them have been.
func (r *releaser) spent(n int64) {
	if r.read.Add(n) >= r.every && r.read.Swap(0) >= r.every {
		r.p.Release()
	}
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
	// The pack's own hash is one thread reading every byte of the pack.
	sums := make(chan []string, 1)
	go func() { sums <- p.checksumComplaints() }()
	objectsOK := p.verifyObjects(o)
	for _, text := range <-sums {
		fail(text)
	}
	// This pack has been read end to end. What a later phase wants from it, it faults back.
	p.Release()
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
func cannotUnpack(emit func(gitobj.OID, string), p *Pack, l *packLayout, oid gitobj.OID, i int32) {
	e := l.ents[i]
	switch msg := l.headerErr(i); {
	case msg != "":
		emit(oid, msg)
	default:
		if msg := p.InflateMessage(e.dataOff(), e.size); msg != "" {
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
		cannotUnpack(emit, p, l, p.OIDAt(l.ents[i].idx), i)
		stack = append(stack, l.children(i)...)
	}
}

// Flags a layout keeps on an entry rather than in a list of its own.
const (
	// entRoot marks an entry no other entry in this pack deltas against.
	entRoot uint8 = 1 << 0
)

// maxHeader is the longest header ReadHeader makes: a size varint, then a base name or an offset varint.
const maxHeader = 1 + 9 + gitobj.MaxRawSize

// packEntry is one object as the pack stores it, in offset order. It is 24
// bytes: everything derivable is derived. see docs/pack-verification.md
type packEntry struct {
	off  int64
	size int64
	idx  uint32 // position in index order
	// hdr is the length of the entry header, which is under fifty bytes.
	hdr   uint8
	typ   int8
	flags uint8
}

// objType is the entry's type as the rest of the package spells it.
func (e packEntry) objType() gitobj.Type { return gitobj.Type(e.typ) }

// dataOff is where the entry's zlib stream starts.
func (e packEntry) dataOff() int64 { return e.off + int64(e.hdr) }

// packLayout is every entry in offset order, plus the base-to-children links
// that let each delta chain be decoded exactly once.
type packLayout struct {
	ents []packEntry
	// The children of entry i are childList[childStart[i]:childStart[i+1]].
	childStart []int32
	childList  []int32
	bad        []int32
	// headerErrs holds what stopped an entry header from being read, which happens before anything decompresses.
	headerErrs map[int32]string
	// trailer is where the last entry ends.
	trailer int64
}

// headerErr returns why an entry's header would not read, or the empty string.
func (l *packLayout) headerErr(i int32) string { return l.headerErrs[i] }

// end is where an entry's bytes stop, which is where the next entry starts.
func (l *packLayout) end(i int32) int64 {
	if int(i)+1 < len(l.ents) {
		return l.ents[i+1].off
	}
	return l.trailer
}

func (l *packLayout) children(i int32) []int32 {
	return l.childList[l.childStart[i]:l.childStart[i+1]]
}

// setType records a type that has to survive in one byte.
func (e *packEntry) setType(t gitobj.Type) { e.typ = int8(t) }

// buildLayout reads every entry header and links each delta to its base.
//
// Nothing here holds a second structure per entry that outlives it: the offsets
// are sorted in place, the parent links are spent on the child lists and
// dropped, and what is left is the entries and the two child arrays.
func (p *Pack) buildLayout(pages *releaser) *packLayout {
	n := int(p.Num)
	l := &packLayout{ents: make([]packEntry, n), headerErrs: map[int32]string{}, trailer: p.TrailerOffset()}
	for i := range l.ents {
		l.ents[i] = packEntry{off: p.OffsetAt(uint32(i)), idx: uint32(i)}
	}
	slices.SortFunc(l.ents, func(a, b packEntry) int { return cmp.Compare(a.off, b.off) })

	// posAt finds an entry by its offset, which is what the entries are sorted by.
	posAt := func(off int64) int32 {
		pos := sort.Search(n, func(k int) bool { return l.ents[k].off >= off })
		if pos < n && l.ents[pos].off == off {
			return int32(pos)
		}
		return -1
	}

	parent := make([]int32, n)
	for i := range parent {
		parent[i] = -1
	}
	for i := range l.ents {
		e := &l.ents[i]
		// One header is one page of the pack, and there is an entry every kilobyte or so: this pass reads all of it.
		pages.spent(l.end(int32(i)) - e.off)
		h, err := p.ReadHeader(e.off)
		if err != nil {
			l.bad = append(l.bad, int32(i))
			e.setType(gitobj.TypeBad)
			l.headerErrs[int32(i)] = err.Error()
			continue
		}
		if h.DataOff-e.off > maxHeader {
			// The entry holds this as a length in one byte. see maxHeader.
			l.bad = append(l.bad, int32(i))
			e.setType(gitobj.TypeBad)
			l.headerErrs[int32(i)] = fmt.Sprintf("object header at %d in %s is malformed", e.off, p.Path)
			continue
		}
		e.setType(h.Type)
		e.size = h.Size
		e.hdr = uint8(h.DataOff - e.off)
		switch h.Type {
		case gitobj.TypeOfsDelta:
			if pos := posAt(h.BaseOff); pos >= 0 {
				parent[i] = pos
			} else {
				l.bad = append(l.bad, int32(i))
				e.setType(gitobj.TypeBad)
			}
		case gitobj.TypeRefDelta:
			// git's get_delta_base() looks in this pack and nowhere else.
			if bi, ok := p.Find(h.BaseOID); ok {
				if pos := posAt(p.OffsetAt(bi)); pos >= 0 {
					parent[i] = pos
					break
				}
			}
			l.bad = append(l.bad, int32(i))
			e.setType(gitobj.TypeBad)
			l.headerErrs[int32(i)] = badDeltaBase(h.DataOff, p.Path).Error()
		}
	}

	l.childStart = make([]int32, n+1)
	for i := range parent {
		if parent[i] >= 0 {
			l.childStart[parent[i]+1]++
		} else {
			l.ents[i].flags |= entRoot
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
	return l
}

// parentOf reads an entry's base back off the pack, for the one path that walks
// up a chain rather than down it. A layout that kept the link would hold four
// bytes per object in the repository for a step taken on a handful of them.
func (l *packLayout) parentOf(p *Pack, i int32) int32 {
	e := l.ents[i]
	h, err := p.ReadHeader(e.off)
	if err != nil {
		return -1
	}
	pos := func(off int64) int32 {
		k := sort.Search(len(l.ents), func(k int) bool { return l.ents[k].off >= off })
		if k < len(l.ents) && l.ents[k].off == off {
			return int32(k)
		}
		return -1
	}
	switch h.Type {
	case gitobj.TypeOfsDelta:
		return pos(h.BaseOff)
	case gitobj.TypeRefDelta:
		if bi, ok := p.Find(h.BaseOID); ok {
			return pos(p.OffsetAt(bi))
		}
	}
	return -1
}

// verifyObjects decodes every object in the pack and hands it to the caller.
func (p *Pack) verifyObjects(o VerifyOpts) bool {
	workers := o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	// One releaser over every pass, so a sweep counts what all of them read.
	pages := newReleaser(p, o.ReleaseEvery)
	l := p.buildLayout(pages)

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
		oid := p.OIDAt(l.ents[i].idx)
		cannotUnpack(emit, p, l, oid, i)
		failSubtree(emit, p, l, i)
	}

	// The index records a CRC over each entry's raw bytes. Checking those is a
	// flat scan of the mapping, so it parallelizes on its own.
	if p.IdxVer > 1 {
		p.checkCRCs(l, workers, emit, pages)
		p.Release()
	}

	w := &walker{p: p, l: l, o: &o, emit: emit, object: object, pages: pages}
	budget := o.ChainBudget
	if budget <= 0 {
		budget = DefaultChainBudget
	}
	w.budget.Store(budget)
	// The workers claim entries and walk the ones nothing deltas against: a list of those is four bytes an object.
	var wg sync.WaitGroup
	var next int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := &Inflater{}
			for {
				ri := int(atomic.AddInt64(&next, 1)) - 1
				if ri >= len(l.ents) {
					return
				}
				if l.ents[ri].flags&entRoot != 0 {
					w.walkChain(int32(ri), in)
				}
			}
		}()
	}
	wg.Wait()
	return good
}

// checkCRCs verifies the index's CRC over every entry's compressed bytes.
func (p *Pack) checkCRCs(l *packLayout, workers int, emit func(gitobj.OID, string), pages *releaser) {
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
				last := min(start+batch, len(l.ents))
				for i := start; i < last; i++ {
					e := l.ents[i]
					end := l.end(int32(i))
					if e.off < 0 || end > int64(len(p.data)) || end < e.off {
						continue
					}
					pages.spent(end - e.off)
					if crc32.ChecksumIEEE(p.data[e.off:end]) != p.CRCAt(e.idx) {
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
