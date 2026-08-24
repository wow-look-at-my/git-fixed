// Package fsck holds git's object consistency checks.
//
// Every check, every message id, and every message string matches git's fsck.c,
// so a report from this package reads exactly like git's own.
package fsck

import (
	"fmt"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/go-containers/set"
)

// Severity is git's enum fsck_msg_type, in the same order.
type Severity int

// The severities.
const (
	SevIgnore Severity = iota
	SevInfo
	SevFatal
	SevError
	SevWarn
)

// String renders the severity as the fsck.<msgid> configuration value spells it.
func (s Severity) String() string {
	switch s {
	case SevIgnore:
		return "ignore"
	case SevWarn:
		return "warn"
	case SevError:
		return "error"
	case SevInfo:
		return "info"
	case SevFatal:
		return "fatal"
	}
	return "unknown"
}

// ParseSeverity accepts the three values git's configuration allows.
func ParseSeverity(s string) (Severity, bool) {
	switch s {
	case "error":
		return SevError, true
	case "warn":
		return SevWarn, true
	case "ignore":
		return SevIgnore, true
	}
	return 0, false
}

// ErrorFunc receives one finished message.
type ErrorFunc func(o *Options, ctx any, oid gitobj.OID, objType gitobj.Type, sev Severity, id MsgID, message string) int

// Options carries the severity table, the skip list, and the deferred work that
// a whole fsck run shares. Several goroutines report through one Options, so
// every mutable field is behind the mutex.
type Options struct {
	Strict bool
	Algo   *gitobj.Algo

	// MaxTreeEntryLen is git's max_tree_entry_len, which fsck.largePathname can raise or lower.
	MaxTreeEntryLen int

	// Error is called for every message that survives the severity table and the skip list.
	Error ErrorFunc

	mu       sync.Mutex
	msgType  []Severity
	skiplist set.Set[gitobj.OID]

	// A .gitmodules or .gitattributes blob named by a tree is checked once its content is read.
	gitmodulesFound    set.Set[gitobj.OID]
	gitmodulesDone     set.Set[gitobj.OID]
	gitattributesFound set.Set[gitobj.OID]
	gitattributesDone  set.Set[gitobj.OID]

	names map[gitobj.OID]string
}

// NewOptions returns options with git's defaults.
func NewOptions(algo *gitobj.Algo) *Options {
	return &Options{
		Algo:               algo,
		MaxTreeEntryLen:    4096,
		skiplist:           set.New[gitobj.OID](),
		gitmodulesFound:    set.New[gitobj.OID](),
		gitmodulesDone:     set.New[gitobj.OID](),
		gitattributesFound: set.New[gitobj.OID](),
		gitattributesDone:  set.New[gitobj.OID](),
		Error:              DefaultErrorFunc,
	}
}

// EnableObjectNames turns on the readable names --name-objects prints.
func (o *Options) EnableObjectNames() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.names == nil {
		o.names = make(map[gitobj.OID]string)
	}
}

// PutObjectName records a name for an object, keeping the first one seen.
func (o *Options) PutObjectName(oid gitobj.OID, format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.names == nil {
		return
	}
	if _, ok := o.names[oid]; ok {
		return
	}
	o.names[oid] = fmt.Sprintf(format, args...)
}

// ObjectName returns the recorded name, or "" when there is none.
func (o *Options) ObjectName(oid gitobj.OID) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.names[oid]
}

// Describe renders an object name, with its readable name when one is known.
func (o *Options) Describe(oid gitobj.OID) string {
	if name := o.ObjectName(oid); name != "" {
		return oid.String() + " (" + name + ")"
	}
	return oid.String()
}

// severity resolves one message id against the table and the strict flag.
func (o *Options) severity(id MsgID) Severity {
	if o.msgType == nil {
		sev := msgInfos[id].Severity
		if o.Strict && sev == SevWarn {
			sev = SevError
		}
		return sev
	}
	return o.msgType[id]
}

// SetSeverity overrides one check's severity. The table it materializes freezes
// the strict flag as it stands now, which is what git does.
func (o *Options) SetSeverity(id MsgID, sev Severity) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.msgType == nil {
		table := make([]Severity, msgIDCount)
		for i := range table {
			table[i] = o.severity(MsgID(i))
		}
		o.msgType = table
	}
	o.msgType[id] = sev
}

// Severity reports the severity in force for one check.
func (o *Options) Severity(id MsgID) Severity {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.severity(id)
}

// AddSkip adds an object the run must not report on.
func (o *Options) AddSkip(oid gitobj.OID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skiplist.Add(oid)
}

// Skipped reports whether the object is on the skip list.
func (o *Options) Skipped(oid gitobj.OID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.skiplist.Contains(oid)
}

// report is git's report(): it applies the severity table and the skip list,
// prefixes the camel-cased message id, and hands the line to the callback.
func (o *Options) report(ctx any, oid gitobj.OID, objType gitobj.Type, id MsgID, format string, args ...any) int {
	sev := o.Severity(id)
	if sev == SevIgnore {
		return 0
	}
	if oid.Valid() && o.Skipped(oid) {
		return 0
	}
	switch sev {
	case SevFatal:
		sev = SevError
	case SevInfo:
		sev = SevWarn
	}
	msg := msgInfos[id].Camel + ": " + fmt.Sprintf(format, args...)
	return o.Error(o, ctx, oid, objType, sev, id, msg)
}

// DefaultErrorFunc is git's fsck_error_function, used by callers that do not
// install one of their own.
func DefaultErrorFunc(o *Options, _ any, oid gitobj.OID, _ gitobj.Type, sev Severity, _ MsgID, message string) int {
	if sev == SevWarn {
		fmt.Printf("warning: object %s: %s\n", o.Describe(oid), message)
		return 0
	}
	fmt.Printf("error: object %s: %s\n", o.Describe(oid), message)
	return 1
}

// MsgIDByName looks up a check by its fsck.<msgid> configuration spelling. The
// name is matched with underscores removed and in lower case, as git does.
func MsgIDByName(name string) (MsgID, bool) {
	lower := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower = append(lower, c)
	}
	for i := range msgInfos {
		if msgInfos[i].Lower == string(lower) {
			return MsgID(i), true
		}
	}
	return 0, false
}

// Name returns the camel-cased spelling of a check.
func (id MsgID) Name() string { return msgInfos[id].Camel }

// DefaultSeverity returns the severity a check has before any configuration.
func (id MsgID) DefaultSeverity() Severity { return msgInfos[id].Severity }
