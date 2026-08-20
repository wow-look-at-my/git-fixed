// Package fsckcmd implements git-fsck.
//
// The phases follow builtin/fsck.c, but each one runs its work in parallel and
// puts the output back in order before printing it.
//
// see docs/architecture.md
package fsckcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
		IncludeReflogs: true,
		CheckFull:      true,
		ShowDangling:   true,
		Workers:        runtime.GOMAXPROCS(0),
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
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
	db, err := odb.Open(repo.ObjectsDir, repo.Algo, !o.ConnectivityOnly)
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

	if o.ConnectivityOnly {
		r.markForConnectivity()
	} else {
		r.checkObjectDirs()
		r.finishDeferredBlobs()
	}
	rep.Flush()

	r.handleArgs()
	if len(o.Args) == 0 {
		r.defaultHeads()
		o.KeepCacheObjects = true
	}
	rep.Flush()

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

	r.verifyGraphFiles()
	rep.Flush()

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

// fsckError renders one finding the way builtin/fsck.c's callback does.
func (r *run) fsckError(o *fsck.Options, oid gitobj.OID, objType gitobj.Type, sev fsck.Severity, _ fsck.MsgID, message string) int {
	key := sortKey{phase: r.currentPhase(oid), oid: oid}
	if sev == fsck.SevWarn {
		r.rep.Errf(key, "warning in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
		return 0
	}
	r.rep.Errf(key, "error in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
	r.fail(ErrorObject)
	return 1
}

// keyFor remembers where an object was found, so its findings sort with the
// rest of that pass.
var _ = sort.Strings

func (r *run) currentPhase(oid gitobj.OID) int {
	if k, ok := r.keys.Load(oid); ok {
		return k.(sortKey).phase
	}
	return phaseObjects
}

// printableType is git's printable_type(): the spelling of an object's type, or
// "unknown" when nothing has told us what it is.
func (r *run) printableType(oid gitobj.OID, typ gitobj.Type) string {
	if typ == gitobj.TypeNone {
		if e := r.objs.Get(oid); e != nil {
			typ = e.Type()
		}
	}
	if name := typ.Name(); name != "" && typ != gitobj.TypeNone {
		return name
	}
	return "unknown"
}

// describePath is only used for messages that name a file rather than an object.
func describePath(dir, name string) string { return filepath.Join(dir, name) }
