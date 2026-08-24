package odb

// Writing loose objects, which is how a recovered object gets back into the
// repository.

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// WriteLoose writes content as a loose object under objectsDir and returns the
// name it was stored under.
//
// The caller says what the object's name must be. WriteLoose hashes the content
// and refuses to write anything else, because a recovered object is only the
// original object if it hashes to the name that went missing. Pass an invalid
// OID to store content whose name is not known in advance.
//
// An object that is already there is left alone. Two names cannot collide on
// different content, so a file that exists holds this content already.
func WriteLoose(objectsDir string, algo *gitobj.Algo, t gitobj.Type, content []byte, want gitobj.OID) (gitobj.OID, error) {
	oid := Hash(algo, t, content)
	if want.Valid() && oid.Compare(want) != 0 {
		return gitobj.OID{}, fmt.Errorf("content hashes to %s, not %s", oid, want)
	}

	name := oid.String()
	dir := filepath.Join(objectsDir, name[:2])
	path := filepath.Join(dir, name[2:])
	if _, err := os.Stat(path); err == nil {
		return oid, nil
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return gitobj.OID{}, err
	}

	body, err := deflateObject(t, content)
	if err != nil {
		return gitobj.OID{}, err
	}

	// Write beside the target and rename, so a reader never sees a partial object.
	tmp, err := os.CreateTemp(dir, "tmp_obj_*")
	if err != nil {
		return gitobj.OID{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return gitobj.OID{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return gitobj.OID{}, err
	}
	// A loose object is read-only once written, the same as git's own.
	if err := os.Chmod(tmpName, 0o444); err != nil {
		os.Remove(tmpName)
		return gitobj.OID{}, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return gitobj.OID{}, err
	}
	return oid, nil
}

// deflateObject builds the on-disk body: the header, a NUL, the content, all
// zlib-compressed.
func deflateObject(t gitobj.Type, content []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := fmt.Fprintf(zw, "%s %d", t.Name(), len(content)); err != nil {
		return nil, err
	}
	if _, err := zw.Write([]byte{0}); err != nil {
		return nil, err
	}
	if _, err := zw.Write(content); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
