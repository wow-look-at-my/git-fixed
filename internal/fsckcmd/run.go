// Package fsckcmd implements the fsck half of git-fixed: git fsck's own checks,
// its output and its exit status.
//
// The phases follow builtin/fsck.c, but each one runs its work in parallel and
// puts the output back in order before printing it.
//
// see docs/architecture.md
package fsckcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// Exit status bits, the same ones builtin/fsck.c returns.
const (
	ErrorObject         = 001
	ErrorReachable      = 002
	ErrorPack           = 004
	ErrorRefs           = 010
	ErrorCommitGraph    = 020
	ErrorMultiPackIndex = 040
	ErrorPackRevIndex   = 0100
	ErrorBitmap         = 0200
)

// Options are the command's settings, one field per git fsck option.
type Options struct {
	ShowRoot         bool
	ShowTags         bool
	ShowUnreachable  bool
	IncludeReflogs   bool
	CheckFull        bool
	CheckReferences  bool
	ConnectivityOnly bool
	Strict           bool
	KeepCacheObjects bool
	WriteLostFound   bool
	Verbose          bool
	ShowDangling     bool
	NameObjects      bool
	ShowProgress     bool
	Args             []string

	// Workers is how many goroutines decode and check objects at once.
	Workers int

	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// DefaultOptions returns the settings git starts from.
func DefaultOptions() *Options {
	return &Options{
		IncludeReflogs:  true,
		CheckFull:       true,
		CheckReferences: true,
		ShowDangling:    true,
		Workers:         runtime.GOMAXPROCS(0),
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
	}
}

// run holds one execution's state.
type run struct {
	o      *Options
	repo   *gitrepo.Repo
	db     *odb.DB
	objs   *objTable
	rep    *reporter
	fsck   *fsck.Options
	errors atomic.Uint32

	defaultRefs atomic.Int64

	fatalMu  sync.Mutex
	fatalMsg string
	// fatalPre is the line git prints just before it dies, which is its
	// decompressor speaking for itself.
	fatalPre string

	pendingMu sync.Mutex
	pending   []*objEntry
}

func (r *run) fail(bits uint32) { r.errors.Or(bits) }

// Run performs the whole check and returns the status git would exit with.
func Run(o *Options) int {
	rep := newReporter(o.Stdout, o.Stderr)
	repo, err := gitrepo.Open(o.Dir)
	if err != nil {
		fmt.Fprintf(o.Stderr, "fatal: %s\n", err)
		return 128
	}
	db, err := odb.Open(repo.ObjectsDir, repo.DisplayObjectsDir, repo.Algo, !o.ConnectivityOnly)
	if err != nil {
		fmt.Fprintf(o.Stderr, "fatal: %s\n", err)
		return 128
	}
	defer db.Close()
	db.BigFileThreshold = repo.Config.Int("core.bigfilethreshold", 512*1024*1024)

	r := &run{o: o, repo: repo, db: db, objs: newObjTable(), rep: rep}
	r.fsck = fsck.NewOptions(repo.Algo)
	r.fsck.Strict = o.Strict
	if o.NameObjects {
		r.fsck.EnableObjectNames()
	}
	r.fsck.Error = r.fsckError
	if code := r.applyConfig(); code != 0 {
		return code
	}

	// die reports the one condition git exits 128 for, after printing what
	// the run has already found.
	die := func() int {
		rep.Flush()
		msg, pre := r.dying()
		if pre != "" {
			fmt.Fprintf(o.Stderr, "error: %s\n", pre)
		}
		fmt.Fprintf(o.Stderr, "fatal: %s\n", msg)
		return 128
	}

	if o.CheckReferences {
		r.checkRefs()
	}
	rep.Flush()

	r.snapshotRefs()
	if r.died() != "" {
		return die()
	}

	if o.ConnectivityOnly {
		r.markForConnectivity()
	} else {
		r.checkObjectDirs()
		r.finishDeferredBlobs()
	}
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	r.handleArgs()
	if len(o.Args) == 0 {
		r.defaultHeads()
		o.KeepCacheObjects = true
	}
	r.noteNoHeads()
	if repo.PackedRefsFatal != "" {
		r.noteFatalMsg(repo.PackedRefsFatal)
	}
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	if o.KeepCacheObjects {
		if code := r.checkIndexes(); code != 0 {
			rep.Flush()
			return code
		}
	}
	rep.Flush()

	r.checkPackRevIndexes()
	r.verifyBitmapFiles()
	rep.Flush()

	r.checkConnectivity()
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	r.verifyGraphFiles()
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	return int(r.errors.Load())
}

// applyConfig reads the fsck.* settings, which decide how loud each check is.
func (r *run) applyConfig() int {
	for _, e := range r.repo.Config.Entries() {
		name, ok := strings.CutPrefix(strings.ToLower(e.Key), "fsck.")
		if !ok || e.Value == nil {
			continue
		}
		if name == "skiplist" {
			if err := r.loadSkiplist(*e.Value); err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: %s\n", err)
				return 128
			}
			continue
		}
		if code := r.setMsgType(name, *e.Value); code != 0 {
			return code
		}
	}
	return 0
}

// setMsgType applies one fsck.<msgid> setting.
func (r *run) setMsgType(name, value string) int {
	id, ok := fsck.MsgIDByName(name)
	if !ok {
		fmt.Fprintf(r.o.Stderr, "fatal: Unhandled message id: %s\n", name)
		return 128
	}
	if id == fsck.MsgLargePathname {
		// This one may carry its own limit, as in "warn:1024".
		if colon := strings.IndexByte(value, ':'); colon >= 0 {
			limit, err := parseSize(value[colon+1:])
			if err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: unable to parse max tree entry len: %s\n", value[colon+1:])
				return 128
			}
			r.fsck.MaxTreeEntryLen = limit
			value = value[:colon]
		}
	}
	sev, ok := fsck.ParseSeverity(value)
	if !ok {
		fmt.Fprintf(r.o.Stderr, "fatal: Unknown fsck message type: '%s'\n", value)
		return 128
	}
	if sev != fsck.SevError && id.DefaultSeverity() == fsck.SevFatal {
		fmt.Fprintf(r.o.Stderr, "fatal: Cannot demote %s to %s\n", name, value)
		return 128
	}
	r.fsck.SetSeverity(id, sev)
	return 0
}

func parseSize(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// loadSkiplist reads the file of object names fsck must stay quiet about.
func (r *run) loadSkiplist(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not open '%s'", path)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		oid, ok := r.repo.Algo.Parse(line)
		if !ok {
			return fmt.Errorf("invalid object name: %s", line)
		}
		r.fsck.AddSkip(oid)
	}
	return nil
}

// fsckError renders one finding the way builtin/fsck.c's callback does. ctx is
// the sort key of whatever pass produced the finding.
func (r *run) fsckError(o *fsck.Options, ctx any, oid gitobj.OID, objType gitobj.Type, sev fsck.Severity, _ fsck.MsgID, message string) int {
	key, _ := ctx.(sortKey)
	key.oid = oid
	if sev == fsck.SevWarn {
		r.rep.Errf(key, "warning in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
		return 0
	}
	r.rep.Errf(key, "error in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
	r.fail(ErrorObject)
	return 1
}

// ensureType fills in an object's type when nothing has said what it is yet.
// Only --connectivity-only leaves types unknown, because it never reads the
// objects; git resolves them the same way, when a reference or a walk first
// needs to know.
func (r *run) ensureType(e *objEntry) gitobj.Type {
	if t := e.Type(); t != gitobj.TypeNone {
		return t
	}
	t, _, err := r.db.Info(e.OID)
	if err != nil {
		return gitobj.TypeNone
	}
	e.SetType(t)
	return t
}

// readObject reads an object and notices the one failure git treats as fatal.
func (r *run) readObject(oid gitobj.OID) (gitobj.Type, []byte, error) {
	typ, buf, err := r.db.Read(oid)
	r.noteFatal(err)
	return typ, buf, err
}

// noteFatal remembers the first condition git would die on. Work already under
// way finishes, and the run stops at the end of the phase, which is as close as
// a parallel implementation gets to git's immediate exit.
func (r *run) noteFatal(err error) {
	var fatal *odb.FatalError
	if err == nil || !errors.As(err, &fatal) {
		return
	}
	r.fatalMu.Lock()
	if r.fatalMsg == "" {
		r.fatalMsg = fatal.Msg
		r.fatalPre = fatal.Inflate
	}
	r.fatalMu.Unlock()
}

// noteFatalMsg records a fatal condition the caller found itself, rather than
// one that came back from the object database.
func (r *run) noteFatalMsg(msg string) {
	r.fatalMu.Lock()
	if r.fatalMsg == "" {
		r.fatalMsg = msg
	}
	r.fatalMu.Unlock()
}

// died reports whether the run has hit a fatal condition.
func (r *run) died() string {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalMsg
}

// dying returns the message the run stops with, and the line its decompressor
// printed first.
func (r *run) dying() (msg, pre string) {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalMsg, r.fatalPre
}
