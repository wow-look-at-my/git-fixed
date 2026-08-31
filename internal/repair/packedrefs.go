package repair

// Repairing a packed-refs file git's own reader refuses.
//
// see docs/repair.md

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// BadPackedRefs is a packed-refs file that will not parse.
type BadPackedRefs struct {
	// Path is the file, absolute.
	Path string
	// Why is what git's reader says about it.
	Why string
}

// RepairedPackedRefs is what a packed-refs rewrite came to.
type RepairedPackedRefs struct {
	// Kept is how many references were carried over unchanged.
	Kept int
	// Restored are references whose line was unreadable and whose value came back from a reflog.
	Restored []RepairedRef
	// Dropped is how many lines could not be read as a reference at all.
	Dropped []string
	// Why is what was wrong with the old file.
	Why string
}

// packedRefsHeader is the trait line git writes.
const packedRefsHeader = "# pack-refs with: peeled fully-peeled sorted \n"

// scanPackedRefs records a packed-refs file git will not read.
func (s *scanner) scanPackedRefs(d *Damage) {
	if s.repo.PackedRefsFatal == "" {
		return
	}
	path := filepath.Join(s.repo.CommonDir, "packed-refs")
	if _, err := os.Stat(path); err != nil {
		return
	}
	d.PackedRefs = &BadPackedRefs{Path: path, Why: s.repo.PackedRefsFatal}
}

// repairPackedRefs rewrites packed-refs from the lines that still read.
//
// git's reader stops at the earliest line it refuses, so a bad line hides every
// reference below it. Rewriting the file in valid grammar puts those back. The
// lines that will not read are a different matter: a dropped line may be a
// branch, and a branch that quietly disappears is exactly the loss this tool
// exists to prevent. So each dropped line is carried in the result, the reflog
// is asked whether it knows the reference, and anything still unaccounted for
// is reported with the original file waiting in quarantine.
func repairPackedRefs(repo *gitrepo.Repo, db *odb.DB, q *Quarantine, bad *BadPackedRefs) (RepairedPackedRefs, error) {
	out := RepairedPackedRefs{Why: bad.Why}
	data, err := os.ReadFile(bad.Path)
	if err != nil {
		return out, err
	}

	refs, dropped := readPackedLines(repo.Algo, data)
	out.Dropped = dropped

	// A dropped line may name a reference the loose refs and the reflogs
	// still know. Asking costs nothing and turns a reported loss into a
	// restored reference.
	for _, name := range namesInDroppedLines(dropped) {
		if _, ok := refs[name]; ok {
			continue
		}
		oid, from, ok := lastGoodValue(repo, db, name)
		if !ok {
			continue
		}
		refs[name] = oid
		out.Restored = append(out.Restored, RepairedRef{Name: name, OID: oid, From: from})
	}
	out.Kept = len(refs) - len(out.Restored)

	body, err := renderPackedRefs(repo, db, refs)
	if err != nil {
		return out, err
	}
	if err := q.Take(bad.Path, "a packed-refs file git's reader refuses"); err != nil {
		return out, err
	}
	if err := writeFileAtomic(bad.Path, body); err != nil {
		return out, err
	}
	return out, nil
}

// readPackedLines pulls every reference out of a packed-refs file, and returns
// the lines it could not read.
//
// This is deliberately more forgiving than git's reader, which is allowed to
// refuse the file outright. Here every line that holds a name and a hash is a
// reference somebody made, whatever is wrong with the line above it.
func readPackedLines(algo *gitobj.Algo, data []byte) (map[string]gitobj.OID, []string) {
	refs := map[string]gitobj.OID{}
	var dropped []string
	for i, raw := range bytes.Split(data, []byte("\n")) {
		line := string(bytes.TrimRight(raw, "\r"))
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			// A comment is legal only at the top of the file; a comment
			// line anywhere else is a line git refuses. It carries no
			// reference either way.
			if i != 0 {
				dropped = append(dropped, line)
			}
			continue
		case strings.HasPrefix(line, "^"):
			// A peel line records what the tag above points at.
			if _, ok := algo.Parse(strings.TrimSpace(line[1:])); !ok {
				dropped = append(dropped, line)
			}
			continue
		}
		hex, name, ok := strings.Cut(line, " ")
		oid, parsed := algo.Parse(hex)
		if !ok || !parsed || name == "" || !fsck.CheckRefnameFormat(name, 0) {
			dropped = append(dropped, line)
			continue
		}
		refs[name] = oid
	}
	return refs, dropped
}

// namesInDroppedLines guesses which references a set of unreadable lines was about.
func namesInDroppedLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "refs/") && fsck.CheckRefnameFormat(field, 0) {
				out = append(out, field)
			}
		}
	}
	sort.Strings(out)
	return out
}

// lastGoodValue finds the newest value a reference held whose object is still
// here, checking the loose ref file before falling back to the reflog.
func lastGoodValue(repo *gitrepo.Repo, db *odb.DB, name string) (gitobj.OID, string, bool) {
	worktreeDir, refName := splitRefName(repo, name)
	loose := filepath.Join(repo.CommonDir, filepath.FromSlash(refName))
	if data, err := os.ReadFile(loose); err == nil {
		if oid, ok := repo.Algo.Parse(strings.TrimSpace(string(data))); ok && db.Has(oid) {
			return oid, "its loose ref file", true
		}
	}
	entries := repo.Reflog(worktreeDir, refName)
	for i := len(entries) - 1; i >= 0; i-- {
		oid := entries[i].New
		if !oid.Valid() || !db.Has(oid) {
			continue
		}
		if _, _, err := db.Read(oid); err != nil {
			continue
		}
		return oid, "its reflog", true
	}
	return gitobj.OID{}, "", false
}

// renderPackedRefs builds a packed-refs file git will read: the trait header,
// then every reference sorted by name, with a peel line under each tag that
// points at something other than a commit-less object.
func renderPackedRefs(repo *gitrepo.Repo, db *odb.DB, refs map[string]gitobj.OID) ([]byte, error) {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	// "sorted" in the header is a claim git's reader relies on to binary search.
	sort.Strings(names)

	var buf bytes.Buffer
	buf.WriteString(packedRefsHeader)
	for _, name := range names {
		oid := refs[name]
		fmt.Fprintf(&buf, "%s %s\n", oid, name)
		if peeled, ok := peel(db, oid); ok {
			fmt.Fprintf(&buf, "^%s\n", peeled)
		}
	}
	return buf.Bytes(), nil
}

// peel follows a tag chain to the object underneath, which is what a "^" line
// records. It reports false for anything that is not a tag.
func peel(db *odb.DB, oid gitobj.OID) (gitobj.OID, bool) {
	cur := oid
	for depth := 0; depth < 10; depth++ {
		typ, data, err := db.Read(cur)
		if err != nil || typ != gitobj.TypeTag {
			if depth == 0 {
				return gitobj.OID{}, false
			}
			return cur, true
		}
		next, ok := taggedObject(db, data)
		if !ok {
			return gitobj.OID{}, false
		}
		cur = next
	}
	return gitobj.OID{}, false
}

// taggedObject reads the object a tag names.
func taggedObject(db *odb.DB, data []byte) (gitobj.OID, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if hex, ok := strings.CutPrefix(line, "object "); ok {
			return db.Algo.Parse(strings.TrimSpace(hex))
		}
		if line == "" {
			break
		}
	}
	return gitobj.OID{}, false
}

// writeFileAtomic replaces a file without a reader ever seeing a partial write.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"_tmp_*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
