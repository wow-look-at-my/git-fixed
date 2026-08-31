package repair

// The caches git rebuilds by itself, and how a scan decides a cache is broken.
//
// see docs/repair.md

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// derivedNames are the caches git rebuilds by itself. docs/repair.md says why
// .git/index is not among them.
var derivedNames = []string{
	"info/commit-graph",
	"info/packs",
}

// scanDerived finds the rebuildable caches that will not parse.
//
// A cache is checked by reading its magic and version, not by verifying what it
// claims, because a cache that disagrees with the objects is displaced by the
// same rule: git rebuilds it either way, and it costs nothing to be wrong.
func (s *scanner) scanDerived(d *Damage) {
	objects := s.repo.ObjectsDir
	for _, name := range derivedNames {
		path := filepath.Join(objects, filepath.FromSlash(name))
		if broken, ok := checkDerived(path); ok && broken {
			d.Derived = append(d.Derived, path)
		}
	}
	// A commit-graph chain names its parts in a file of hashes.
	chainDir := filepath.Join(objects, "info", "commit-graphs")
	if entries, err := os.ReadDir(chainDir); err == nil {
		for _, e := range entries {
			path := filepath.Join(chainDir, e.Name())
			if broken, ok := checkDerived(path); ok && broken {
				d.Derived = append(d.Derived, path)
			}
		}
	}
	packDir := filepath.Join(objects, "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "multi-pack-index",
			strings.HasSuffix(name, ".rev"),
			strings.HasSuffix(name, ".bitmap"):
			path := filepath.Join(packDir, name)
			if broken, ok := checkDerived(path); ok && broken {
				d.Derived = append(d.Derived, path)
			}
		}
	}
	sort.Strings(d.Derived)
}

// derivedMagic is the signature each cache file starts with.
var derivedMagic = map[string][]byte{
	"commit-graph":      []byte("CGPH"),
	"multi-pack-index":  []byte("MIDX"),
	".rev":              []byte("RIDX"),
	".bitmap":           []byte("BITM"),
	"commit-graph-part": []byte("CGPH"),
}

// checkDerived reports whether a cache file is broken. ok is false when there
// is nothing to judge, which is the usual case: the file is not there.
func checkDerived(path string) (broken, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	base := filepath.Base(path)
	var magic []byte
	switch {
	case base == "multi-pack-index":
		magic = derivedMagic["multi-pack-index"]
	case base == "commit-graph":
		magic = derivedMagic["commit-graph"]
	case strings.HasSuffix(base, ".rev"):
		magic = derivedMagic[".rev"]
	case strings.HasSuffix(base, ".bitmap"):
		magic = derivedMagic[".bitmap"]
	case base == "packs":
		// objects/info/packs is a text list of pack names.
		return packsListStale(path, data), true
	default:
		// A commit-graph chain part is named by its hash, and the chain file
		// itself is a list of those names.
		if filepath.Base(filepath.Dir(path)) == "commit-graphs" {
			if base == "commit-graph-chain" {
				return false, false
			}
			magic = derivedMagic["commit-graph-part"]
		}
	}
	if magic == nil {
		return false, false
	}
	if len(data) < len(magic)+4 {
		return true, true
	}
	return string(data[:len(magic)]) != string(magic), true
}

// packsListStale reports whether objects/info/packs names a pack that is gone.
func packsListStale(path string, data []byte) bool {
	dir := filepath.Dir(filepath.Dir(path))
	for _, line := range strings.Split(string(data), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "P ")
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "pack", name)); err != nil {
			return true
		}
	}
	return false
}
