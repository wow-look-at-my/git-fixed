package fsckcmd

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Phases of a run, in the order git produces their output.
const (
	phaseRefs = iota
	phaseObjects
	phaseHeads
	phaseIndex
	phaseIndexFiles
	phaseConnectivity
	phaseGraphs
)

// sortKey orders one message. Work runs in parallel, so a message carries where
// it came from and the output is put back in that order before it is printed.
type sortKey struct {
	phase int
	group int   // object directory, or pack number
	pos   int64 // offset in a pack
	oid   gitobj.OID
	seq   int64 // keeps messages about one object in the order they were made
}

func (a sortKey) less(b sortKey) bool {
	switch {
	case a.phase != b.phase:
		return a.phase < b.phase
	case a.group != b.group:
		return a.group < b.group
	case a.pos != b.pos:
		return a.pos < b.pos
	}
	if c := a.oid.Compare(b.oid); c != 0 {
		return c < 0
	}
	return a.seq < b.seq
}

// reporter collects every line, then prints each phase in order.
//
// see docs/output-ordering.md
type reporter struct {
	mu     sync.Mutex
	msgs   []message
	seq    int64
	stdout io.Writer
	stderr io.Writer
	// stream sends verbose lines straight out instead of holding them, so a
	// run over a large repository does not keep one line per object.
	stream bool
}

type message struct {
	key  sortKey
	out  bool // true for stdout, false for stderr
	text string
}

func newReporter(stdout, stderr io.Writer) *reporter {
	return &reporter{stdout: stdout, stderr: stderr}
}

func (r *reporter) add(key sortKey, toStdout bool, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	key.seq = r.seq
	r.msgs = append(r.msgs, message{key: key, out: toStdout, text: text})
}

// Errf queues a line for stderr.
func (r *reporter) Errf(key sortKey, format string, args ...any) {
	r.add(key, false, fmt.Sprintf(format, args...))
}

// Outf queues a line for stdout.
func (r *reporter) Outf(key sortKey, format string, args ...any) {
	r.add(key, true, fmt.Sprintf(format, args...))
}

// Verbosef writes a progress-style line immediately. Verbose output is one line
// per object, so holding it would cost memory in proportion to the repository.
func (r *reporter) Verbosef(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.stderr, format+"\n", args...)
}

// Flush prints everything collected so far, in order, and forgets it.
func (r *reporter) Flush() {
	r.mu.Lock()
	msgs := r.msgs
	r.msgs = nil
	r.mu.Unlock()
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].key.less(msgs[j].key) })
	for _, m := range msgs {
		w := r.stderr
		if m.out {
			w = r.stdout
		}
		fmt.Fprintln(w, m.text)
	}
}
