package fsckcmd

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
)

// checkRefs is the reference-database check git runs as "git refs verify",
// before it looks at a single object. It reads the files themselves rather than
// the references they name: a ref file with a stray byte, a name no ref may
// have, or a symref pointing outside the ref namespace is a defect even when
// the object it names is fine.
//
// see docs/ref-consistency.md
func (r *run) checkRefs() {
	if r.o.Verbose {
		r.rep.Verbosef("Checking ref database")
	}
	// git measures this phase as a single step, because it hands the whole of it to "git refs verify" and cannot see.
	m := r.meterOn("Checking ref database", 1)
	defer func() {
		m.Advance(1)
		m.Finish()
	}()
	for _, wt := range r.repo.Worktrees() {
		dir := wt.Dir
		prefix := ""
		if !wt.IsMain {
			prefix = "worktrees/" + wt.ID + "/"
		}
		r.checkRefsDir(filepath.Join(dir, "refs"), prefix)
		r.checkRootRefs(dir, prefix)
		if wt.IsMain {
			r.checkPackedRefs(filepath.Join(dir, "packed-refs"))
		}
	}
}

// refReport prints a finding about a reference. git prints the path, the name
// of the check, and the complaint, and only an error counts toward the status.
func (r *run) refReport(path string, id fsck.MsgID, format string, args ...any) {
	sev := r.fsck.Severity(id)
	if sev == fsck.SevIgnore {
		return
	}
	// A ref check has no fatal level, and an informational finding prints
	// as a warning, exactly as fsck's own reporting does.
	if sev == fsck.SevInfo {
		sev = fsck.SevWarn
	}
	key := sortKey{phase: phaseRefs}
	text := fmt.Sprintf("%s: %s: %s", path, id.Name(), fmt.Sprintf(format, args...))
	if sev == fsck.SevWarn {
		r.rep.Errf(key, "warning: %s", text)
		return
	}
	r.rep.Errf(key, "error: %s", text)
	r.fail(ErrorRefs)
}

// checkRefsDir walks a worktree's refs directory. Every file under it is a
// reference, whatever its depth.
func (r *run) checkRefsDir(root, prefix string) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return
	}
	// The walk order follows the directory, so sort to keep the report reproducible.
	sort.Strings(paths)
	for _, path := range paths {
		name := filepath.Base(path)
		// A lock file is not a reference. A name that starts with a dot
		// is not exempt: a ref may not begin with a dot, and the name
		// check has to see it.
		if name[0] != '.' && strings.HasSuffix(name, ".lock") {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		r.checkOneRef(prefix+"refs/"+filepath.ToSlash(rel), path)
	}
}

// checkRootRefs checks the references that live beside refs/ rather than under
// it, such as HEAD and MERGE_HEAD.
func (r *run) checkRootRefs(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name[0] == '.' || strings.HasSuffix(name, ".lock") {
			continue
		}
		if !e.Type().IsRegular() || !fsck.IsRootRef(name) {
			continue
		}
		r.checkOneRef(prefix+name, filepath.Join(dir, name))
	}
}

// checkOneRef is git's files_fsck_ref(): the file's type, then its name, then
// its content.
func (r *run) checkOneRef(refname, path string) {
	if r.o.Verbose {
		r.rep.Verbosef("Checking %s", refname)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return
	}
	mode := st.Mode()
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		r.refReport(refname, fsck.MsgBadRefFiletype, "unexpected file type")
		return
	}
	r.checkRefName(refname)
	r.checkRefContent(refname, path, mode)
}

// checkRefName rejects a name no reference may carry. A root reference is
// exempt, because its name is a single component in capitals.
func (r *run) checkRefName(refname string) {
	if fsck.IsRootRef(refname) {
		return
	}
	if !fsck.CheckRefnameFormat(refname, 0) {
		r.refReport(refname, fsck.MsgBadRefName, "invalid refname format")
	}
}

// checkRefContent reads a ref file and judges what is in it.
func (r *run) checkRefContent(refname, path string, mode os.FileMode) {
	if mode&os.ModeSymlink != 0 {
		r.refReport(refname, fsck.MsgSymlinkRef, "use deprecated symbolic link for symref")
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return
		}
		gitdir, err := filepath.Abs(r.repo.CommonDir)
		if err != nil {
			return
		}
		referent := target
		if rel, err := filepath.Rel(gitdir, target); err == nil && !strings.HasPrefix(rel, "..") {
			referent = rel
		}
		// A symlink carries no trailing byte to complain about, so the newline checks are skipped for it.
		r.checkSymrefTarget(refname, filepath.ToSlash(referent))
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if rest, ok := bytes.CutPrefix(data, []byte("ref:")); ok {
		referent := string(bytes.TrimLeft(rest, " \t\n\v\f\r"))
		r.checkSymrefContent(refname, referent)
		return
	}
	oid, trailing, ok := r.repo.Algo.ParsePrefix(string(data))
	if !ok {
		r.refReport(refname, fsck.MsgBadRefContent, "%s", strings.TrimRight(string(data), " \t\n\v\f\r"))
		return
	}
	if trailing == "" {
		r.refReport(refname, fsck.MsgRefMissingNewline, "misses LF at the end")
		return
	}
	if trailing != "\n" {
		r.refReport(refname, fsck.MsgTrailingRefContent, "has trailing garbage: '%s'", trailing)
		return
	}
	if oid.IsNull() {
		r.refReport(refname, fsck.MsgBadRefOid, "points to invalid object ID '%s'", oid)
	}
}

// checkSymrefContent judges the target line of a symbolic reference stored as a
// file, including the whitespace around it.
func (r *run) checkSymrefContent(refname, referent string) {
	trimmed := strings.TrimRight(referent, " \t\n\v\f\r")
	lastByte := byte(0)
	if referent != "" {
		lastByte = referent[len(referent)-1]
	}
	if len(trimmed) == len(referent) || (len(trimmed) < len(referent) && lastByte != '\n') {
		r.refReport(refname, fsck.MsgRefMissingNewline, "misses LF at the end")
	}
	if len(trimmed) != len(referent) && len(trimmed) != len(referent)-1 {
		r.refReport(refname, fsck.MsgTrailingRefContent, "has trailing whitespaces or newlines")
	}
	r.checkSymrefTarget(refname, trimmed)
}

// checkSymrefTarget judges where a symbolic reference points.
func (r *run) checkSymrefTarget(refname, target string) {
	stripped := refname
	if rest, ok := strings.CutPrefix(refname, "worktrees/"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			stripped = rest[i+1:]
		}
	}
	if stripped == "HEAD" && !strings.HasPrefix(target, "refs/heads/") {
		r.refReport(refname, fsck.MsgBadHeadTarget, "HEAD points to non-branch '%s'", target)
		return
	}
	if fsck.IsRootRef(target) {
		return
	}
	if !fsck.CheckRefnameFormat(target, 0) {
		r.refReport(refname, fsck.MsgBadReferentName, "points to invalid refname '%s'", target)
		return
	}
	if !strings.HasPrefix(target, "refs/") && !strings.HasPrefix(target, "worktrees/") {
		r.refReport(refname, fsck.MsgSymrefTargetIsNotARef, "points to non-ref target '%s'", target)
	}
}

// checkPackedRefs checks the packed-refs file line by line. Its name in every
// message is the bare "packed-refs", or the line the problem is on.
func (r *run) checkPackedRefs(path string) {
	st, err := os.Lstat(path)
	if err != nil {
		return // not having a packed-refs file is normal
	}
	if st.Mode()&os.ModeSymlink != 0 {
		r.refReport("packed-refs", fsck.MsgBadRefFiletype, "not a regular file but a symlink")
		return
	}
	if !st.Mode().IsRegular() {
		r.refReport("packed-refs", fsck.MsgBadRefFiletype, "not a regular file")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if len(data) == 0 {
		r.refReport("packed-refs", fsck.MsgEmptyPackedRefsFile, "file is empty")
		return
	}
	sorted := r.checkPackedRefsContent(data)
	if sorted {
		r.checkPackedRefsSorted(data)
	}
}

// packedLine returns the line starting at off, and reports whether it ended in
// a newline. git reports an unterminated line and then reads to the end anyway.
func (r *run) packedLine(data []byte, off int, line int) ([]byte, int) {
	i := bytes.IndexByte(data[off:], '\n')
	if i < 0 {
		r.refReport(fmt.Sprintf("packed-refs line %d", line), fsck.MsgPackedRefEntryNotTerminated,
			"'%s' is not terminated with a newline", data[off:])
		return data[off:], len(data)
	}
	return data[off : off+i], off + i + 1
}

// checkPackedRefsContent walks the file and reports whether its header claims
// the entries are sorted.
func (r *run) checkPackedRefsContent(data []byte) bool {
	line := 1
	off := 0
	sorted := false
	// git looks at the opening line before it knows whether the file has a header.
	first, next := r.packedLine(data, off, line)
	if len(data) > 0 && data[0] == '#' {
		sorted = r.checkPackedRefsHeader(first)
		off, line = next, line+1
	}
	for off < len(data) {
		text, next := r.packedLine(data, off, line)
		r.checkPackedRefsEntry(line, text)
		off, line = next, line+1
		if off < len(data) && data[off] == '^' {
			text, next := r.packedLine(data, off, line)
			r.checkPackedRefsPeeled(line, text)
			off, line = next, line+1
		}
	}
	return sorted
}

// checkPackedRefsHeader judges the opening line, and reports whether it claims the
// file is sorted.
func (r *run) checkPackedRefsHeader(text []byte) bool {
	traits, ok := bytes.CutPrefix(text, []byte("# pack-refs with: "))
	if !ok {
		r.refReport("packed-refs.header", fsck.MsgBadPackedRefHeader,
			"'%s' does not start with '# pack-refs with: '", text)
		return false
	}
	for _, t := range bytes.Split(traits, []byte(" ")) {
		if string(t) == "sorted" {
			return true
		}
	}
	return false
}

// checkPackedRefsEntry judges an "<oid> <refname>" line.
func (r *run) checkPackedRefsEntry(line int, text []byte) {
	where := fmt.Sprintf("packed-refs line %d", line)
	oid, rest, ok := r.repo.Algo.ParsePrefix(string(text))
	if !ok {
		r.refReport(where, fsck.MsgBadPackedRefEntry, "'%s' has invalid oid", text)
		return
	}
	if rest == "" || !isSpaceByte(rest[0]) {
		r.refReport(where, fsck.MsgBadPackedRefEntry,
			"has no space after oid '%s' but with '%s'", oid, rest)
		return
	}
	refname := rest[1:]
	if strings.IndexByte(refname, 0) >= 0 {
		r.refReport(where, fsck.MsgBadPackedRefEntry, "refname '%s' contains NULL binaries", refname)
	}
	if !fsck.CheckRefnameFormat(refname, 0) {
		r.refReport(where, fsck.MsgBadRefName, "has bad refname '%s'", refname)
	}
}

// checkPackedRefsPeeled judges a "^<oid>" line, which carries what the tag above
// it points at.
func (r *run) checkPackedRefsPeeled(line int, text []byte) {
	where := fmt.Sprintf("packed-refs line %d", line)
	body := text[1:]
	_, rest, ok := r.repo.Algo.ParsePrefix(string(body))
	if !ok {
		r.refReport(where, fsck.MsgBadPackedRefEntry, "'%s' has invalid peeled oid", body)
		return
	}
	if rest != "" {
		r.refReport(where, fsck.MsgBadPackedRefEntry,
			"has trailing garbage after peeled oid '%s'", rest)
	}
}

// checkPackedRefsSorted checks the claim the header makes. git compares the
// names as they sit in the file, so a name sorts by its bytes up to the newline.
func (r *run) checkPackedRefsSorted(data []byte) {
	hexsz := r.repo.Algo.HexSize
	line := 1
	off := 0
	if len(data) > 0 && data[0] == '#' {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			off = i + 1
			line++
		}
	}
	prev := ""
	for off < len(data) {
		end := bytes.IndexByte(data[off:], '\n')
		if end < 0 {
			end = len(data) - off
		}
		text := string(data[off : off+end])
		off += end + 1
		if strings.HasPrefix(text, "^") {
			line++
			continue
		}
		if len(text) <= hexsz {
			line++
			continue
		}
		cur := text[hexsz+1:]
		if prev != "" && prev >= cur {
			r.refReport(fmt.Sprintf("packed-refs line %d", line), fsck.MsgPackedRefUnsorted,
				"refname '%s' is less than previous refname '%s'", cur, prev)
			return
		}
		prev = cur
		line++
	}
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
