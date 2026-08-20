package main

// The diagnosis half of a run: git fsck's options, and the run they describe.

import (
	"io"
	"os"
	"runtime"
	"strconv"

	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/parseopt"
)

// fsckFlags are git fsck's own options, spelled the way git spells them and
// meaning what git means by them.
type fsckFlags struct {
	verbose      int
	unreachable  int
	dangling     int
	tags         int
	root         int
	cache        int
	reflogs      int
	full         int
	connectivity int
	references   int
	strict       int
	lostFound    int
	progress     int
	nameObjects  int
}

// newFsckFlags starts the flags where git starts them. Progress is -1 for "not
// said either way", which is a third state, not a default.
func newFsckFlags() *fsckFlags {
	return &fsckFlags{dangling: 1, reflogs: 1, full: 1, references: 1, progress: -1}
}

// table is the option list, in git fsck's order.
func (f *fsckFlags) table() []*parseopt.Bool {
	return []*parseopt.Bool{
		{Short: 'v', Long: "verbose", Help: "be verbose", Value: &f.verbose},
		{Long: "unreachable", Help: "show unreachable objects", Value: &f.unreachable},
		{Long: "dangling", Help: "show dangling objects", Value: &f.dangling},
		{Long: "tags", Help: "report tags", Value: &f.tags},
		{Long: "root", Help: "report root nodes", Value: &f.root},
		{Long: "cache", Help: "make index objects head nodes", Value: &f.cache},
		{Long: "reflogs", Help: "make reflogs head nodes (default)", Value: &f.reflogs},
		{Long: "full", Help: "also consider packs and alternate objects", Value: &f.full},
		{Long: "connectivity-only", Help: "check only connectivity", Value: &f.connectivity},
		{Long: "references", Help: "check reference database consistency", Value: &f.references},
		{Long: "strict", Help: "enable more strict checking", Value: &f.strict},
		{Long: "lost-found", Help: "write dangling objects in .git/lost-found", Value: &f.lostFound},
		{Long: "progress", Help: "show progress", Value: &f.progress},
		{Long: "name-objects", Help: "show verbose names for reachable objects", Value: &f.nameObjects},
	}
}

// options turns the flags into a run, resolving the defaults git resolves once
// it has read the whole command line.
func (f *fsckFlags) options(dir string, args []string, stdout, stderr io.Writer) *fsckcmd.Options {
	o := fsckcmd.DefaultOptions()
	o.Dir = dir
	o.Args = args
	o.Stdout = stdout
	o.Stderr = stderr
	o.Verbose = f.verbose != 0
	o.ShowUnreachable = f.unreachable != 0
	o.ShowDangling = f.dangling != 0
	o.ShowTags = f.tags != 0
	o.ShowRoot = f.root != 0
	o.KeepCacheObjects = f.cache != 0
	o.IncludeReflogs = f.reflogs != 0
	o.CheckFull = f.full != 0
	o.ConnectivityOnly = f.connectivity != 0
	o.CheckReferences = f.references != 0
	o.Strict = f.strict != 0
	o.WriteLostFound = f.lostFound != 0
	o.NameObjects = f.nameObjects != 0

	if f.progress == -1 {
		o.ShowProgress = isTerminal(stderr)
	} else {
		o.ShowProgress = f.progress != 0
	}
	if o.Verbose {
		o.ShowProgress = false
	}
	if o.WriteLostFound {
		o.CheckFull = true
		o.IncludeReflogs = false
	}
	if n, err := strconv.Atoi(os.Getenv("GIT_FIXED_THREADS")); err == nil && n > 0 {
		o.Workers = n
	} else {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	return o
}

// sameVerdict reports whether this run asks fsck the question the repair's own
// verification asks.
//
// Only then does its exit status mean "the repository is whole", and only then
// may the repair reuse it instead of reading everything again. --strict fails
// repositories git is happy with, --connectivity-only and --no-full pass ones
// it is not, a named object narrows the check to that object, and the rest
// below move the roots the walk starts from. Each answers something else.
func sameVerdict(o *fsckcmd.Options) bool {
	d := fsckcmd.DefaultOptions()
	return len(o.Args) == 0 &&
		o.Strict == d.Strict &&
		o.ConnectivityOnly == d.ConnectivityOnly &&
		o.KeepCacheObjects == d.KeepCacheObjects &&
		o.WriteLostFound == d.WriteLostFound &&
		o.CheckFull == d.CheckFull &&
		o.CheckReferences == d.CheckReferences &&
		o.IncludeReflogs == d.IncludeReflogs
}

// isTerminal reports whether progress would be going to a terminal, which is
// how git decides to show it when nobody said either way.
//
// It asks about the stream progress is actually written to, not about the
// process's own stderr. Those are the same thing for a real run and different
// for a test, which passes a buffer and must not have a progress meter written
// into the output it is checking.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
