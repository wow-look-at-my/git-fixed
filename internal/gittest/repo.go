// Package gittest builds repositories for tests, including ones that are
// deliberately broken in ways git's own porcelain refuses to produce.
package gittest

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// Repo is a repository under a test's temporary directory.
type Repo struct {
	t    *testing.T
	Dir  string
	Algo *gitobj.Algo
}

// New creates an empty repository with a predictable configuration.
func New(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	r := &Repo{t: t, Dir: dir, Algo: gitobj.SHA1}
	r.Git("init", "-q", "--template=", ".")
	r.Git("config", "user.name", "A U Thor")
	r.Git("config", "user.email", "author@example.com")
	return r
}

// NewSHA256 creates an empty repository that names objects with SHA-256.
func NewSHA256(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	r := &Repo{t: t, Dir: dir, Algo: gitobj.SHA256}
	r.Git("init", "-q", "--template=", "--object-format=sha256", ".")
	r.Git("config", "user.name", "A U Thor")
	r.Git("config", "user.email", "author@example.com")
	return r
}

// Env is the fixed identity and date every test commit uses, so object names
// stay the same from run to run.
func Env() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=A U Thor",
		"GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=C O Mitter",
		"GIT_COMMITTER_EMAIL=committer@example.com",
		"GIT_COMMITTER_DATE=1700000000 +0000",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+os.TempDir(),
		"XDG_CONFIG_HOME="+os.TempDir(),
	)
}

// Git runs a git command in the repository and fails the test if it errors.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	out, err := r.TryGit(args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TryGit runs a git command and hands back its combined output and error.
func (r *Repo) TryGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = Env()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// GitIn runs git with input on its standard input.
func (r *Repo) GitIn(stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = Env()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errBuf.String())
	}
	return strings.TrimRight(out.String(), "\n")
}

// GitDir is the repository's control directory.
func (r *Repo) GitDir() string { return filepath.Join(r.Dir, ".git") }

// WriteRaw stores a loose object with the type name and content given, without
// asking git whether either makes sense. It returns the object's name.
func (r *Repo) WriteRaw(typeName string, content []byte) gitobj.OID {
	r.t.Helper()
	oid := odb.HashLiteral(r.Algo, typeName, content)
	var body bytes.Buffer
	fmt.Fprintf(&body, "%s %d", typeName, len(content))
	body.WriteByte(0)
	body.Write(content)
	r.writeLoose(oid, body.Bytes())
	return oid
}

// WriteLooseBytes stores exactly these bytes as the object's file, which lets a
// test build a file whose content does not match its name.
func (r *Repo) WriteLooseBytes(oid gitobj.OID, raw []byte) {
	r.t.Helper()
	r.writeLoose(oid, raw)
}

func (r *Repo) writeLoose(oid gitobj.OID, uncompressed []byte) {
	r.t.Helper()
	hex := oid.String()
	dir := filepath.Join(r.GitDir(), "objects", hex[:2])
	if err := os.MkdirAll(dir, 0o777); err != nil {
		r.t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zlib.NewWriter(&out)
	if _, err := zw.Write(uncompressed); err != nil {
		r.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		r.t.Fatal(err)
	}
	path := filepath.Join(dir, hex[2:])
	_ = os.Chmod(path, 0o666)
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		r.t.Fatal(err)
	}
}

// ObjectPath is where a loose object's file lives.
func (r *Repo) ObjectPath(oid gitobj.OID) string {
	hex := oid.String()
	return filepath.Join(r.GitDir(), "objects", hex[:2], hex[2:])
}

// Overwrite replaces a loose object's file with raw bytes, which is how a test
// makes an object that cannot be read at all.
func (r *Repo) Overwrite(oid gitobj.OID, raw []byte) {
	r.t.Helper()
	path := r.ObjectPath(oid)
	_ = os.Chmod(path, 0o666)
	if err := os.WriteFile(path, raw, 0o444); err != nil {
		r.t.Fatal(err)
	}
}

// Delete removes a loose object.
func (r *Repo) Delete(oid gitobj.OID) {
	r.t.Helper()
	path := r.ObjectPath(oid)
	_ = os.Chmod(path, 0o666)
	if err := os.Remove(path); err != nil {
		r.t.Fatal(err)
	}
}

// TreeEntry is one line of a tree, with the mode written out exactly as given
// so a test can store a zero-padded or otherwise unusual mode.
type TreeEntry struct {
	Mode string
	Name string
	OID  gitobj.OID
}

// Tree assembles tree object bytes from entries, in the order given.
func Tree(entries ...TreeEntry) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(e.OID.Raw())
	}
	return buf.Bytes()
}

// Blob stores a blob and returns its name.
func (r *Repo) Blob(content string) gitobj.OID {
	r.t.Helper()
	return r.WriteRaw("blob", []byte(content))
}

// Commit stores a well-formed commit and returns its name.
func (r *Repo) Commit(tree gitobj.OID, parents []gitobj.OID, message string) gitobj.OID {
	r.t.Helper()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	buf.WriteString("author A U Thor <author@example.com> 1700000000 +0000\n")
	buf.WriteString("committer C O Mitter <committer@example.com> 1700000000 +0000\n\n")
	buf.WriteString(message)
	buf.WriteString("\n")
	return r.WriteRaw("commit", buf.Bytes())
}

// Tag stores a well-formed annotated tag and returns its name.
func (r *Repo) Tag(target gitobj.OID, targetType, name, message string) gitobj.OID {
	r.t.Helper()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "object %s\ntype %s\ntag %s\n", target, targetType, name)
	buf.WriteString("tagger C O Mitter <committer@example.com> 1700000000 +0000\n\n")
	buf.WriteString(message)
	buf.WriteString("\n")
	return r.WriteRaw("tag", buf.Bytes())
}

// UpdateRef points a reference at an object.
func (r *Repo) UpdateRef(name string, oid gitobj.OID) {
	r.t.Helper()
	path := filepath.Join(r.GitDir(), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(oid.String()+"\n"), 0o666); err != nil {
		r.t.Fatal(err)
	}
}

// SetHEAD points HEAD at a reference.
func (r *Repo) SetHEAD(ref string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.GitDir(), "HEAD"), []byte("ref: "+ref+"\n"), 0o666); err != nil {
		r.t.Fatal(err)
	}
}

// Write puts a file in the working tree.
func (r *Repo) Write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.Dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
		r.t.Fatal(err)
	}
}

// SimpleHistory builds one commit on master with one file, and returns the
// names of the blob, the tree, and the commit.
func (r *Repo) SimpleHistory() (blob, tree, commit gitobj.OID) {
	r.t.Helper()
	blob = r.Blob("hello\n")
	tree = r.WriteRaw("tree", Tree(TreeEntry{Mode: "100644", Name: "f", OID: blob}))
	commit = r.Commit(tree, nil, "one")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	return blob, tree, commit
}
