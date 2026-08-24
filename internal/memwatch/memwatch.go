// Package memwatch reports what a run costs the machine, as high-water marks.
//
// A run over a large repository is a climbing count and nothing else, and the
// count does not say how close the machine is to the end of its memory: a run
// about to be killed looks exactly like a run halfway through. These marks say
// it, and they are drawn on the meter as well as printed at the end, because a
// run the kernel kills never reaches the end.
//
// Resident memory has the kernel's own mark. VmHWM is the largest resident set
// the process has held, so it is exact whenever it is read, however seldom that
// is. The other two have no mark and are sampled.
//
// see docs/memory.md
package memwatch

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// refresh is how stale a mark may be before a caller's read opens the file again.
const refresh = 250 * time.Millisecond

// Marks are what a run has cost at its worst moment. Each one is its own mark,
// reached at its own moment, so they do not describe one instant and the
// smaller ones are not a part of the larger.
type Marks struct {
	// RSS is the largest resident set the process has held: VmHWM, which the kernel maintains.
	RSS uint64
	// Anon is the largest anonymous resident set seen: the heap, the object table and the buffers.
	Anon uint64
	// Swap is the largest swap use seen.
	Swap uint64
}

// Peak returns the process's marks, and false on a system that publishes none.
func Peak() (Marks, bool) { return std.peak() }

// std is the marks of this process, which has one memory footprint however many meters are drawing it.
var std = &watcher{path: "/proc/self/status", now: time.Now}

type watcher struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	last Marks
	// have says whether any read has succeeded, and read whether any has happened at all.
	have bool
	read bool
	at   time.Time
}

func (w *watcher) peak() (Marks, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.read || w.now().Sub(w.at) >= refresh {
		w.sample()
	}
	return w.last, w.have
}

// sample reads the file and moves the marks that have moved. A mark only ever
// goes up, so a later read that fails, or that catches a smaller number,
// leaves the mark where the worst moment put it.
func (w *watcher) sample() {
	w.at = w.now()
	w.read = true
	m, ok := readStatus(w.path)
	if !ok {
		return
	}
	w.have = true
	w.last.RSS = max(w.last.RSS, m.RSS)
	w.last.Anon = max(w.last.Anon, m.Anon)
	w.last.Swap = max(w.last.Swap, m.Swap)
}

func readStatus(path string) (Marks, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Marks{}, false
	}
	return parseStatus(string(data))
}

// parseStatus takes the three numbers out of a /proc/<pid>/status, which names
// them in kB:
//
//	VmHWM:	 81788928 kB
//	RssAnon:  12897484 kB
//	VmSwap:          0 kB
//
// VmHWM is the peak and the other two are current, which is what makes the
// other two sampled. A kernel built without an MMU publishes none of them, and
// a value in a unit this does not know is refused rather than guessed at: a
// wrong figure here would be read as a measurement.
func parseStatus(status string) (Marks, bool) {
	var m Marks
	var haveRSS bool
	for line := range strings.SplitSeq(status, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		var into *uint64
		switch key {
		case "VmHWM":
			into = &m.RSS
			haveRSS = true
		case "RssAnon":
			into = &m.Anon
		case "VmSwap":
			into = &m.Swap
		default:
			continue
		}
		kb, ok := kilobytes(value)
		if !ok {
			return Marks{}, false
		}
		*into = kb * 1024
	}
	// Without the resident mark there is nothing worth printing, whatever else the file held.
	return m, haveRSS
}

// kilobytes reads the "  81788928 kB" half of a line.
func kilobytes(value string) (uint64, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[1] != "kB" {
		return 0, false
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return kb, true
}

// Short renders the marks for a progress line.
func (m Marks) Short() string {
	if m.Swap == 0 {
		return Bytes(m.RSS)
	}
	return Bytes(m.RSS) + " +" + Bytes(m.Swap) + " swap"
}

// String renders them for the line a run ends with, where the anonymous mark earns its room.
func (m Marks) String() string {
	swap := "nothing swapped"
	if m.Swap > 0 {
		swap = Bytes(m.Swap) + " swapped"
	}
	return fmt.Sprintf("%s resident (%s of it this process's own), %s",
		Bytes(m.RSS), Bytes(m.Anon), swap)
}

// Bytes renders a size the way git renders one: a binary unit and two decimal
// places, as `git count-objects -H` prints "333.84 KiB", and whole bytes below
// a KiB.
func Bytes(n uint64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
