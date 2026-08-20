package fsckcmd

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// checkPackRevIndexes verifies each pack's reverse index, if one is on disk.
func (r *run) checkPackRevIndexes() {
	key := sortKey{phase: phaseIndexFiles}
	for _, p := range r.db.Packs() {
		if p.OpenErr != nil {
			continue
		}
		path := strings.TrimSuffix(p.Path, ".pack") + ".rev"
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			r.rep.Errf(key, "error: unable to load rev-index for pack '%s'", p.Path)
			r.fail(ErrorPackRevIndex)
			continue
		}
		if msg := verifyRevIndex(data, p, r.repo.Algo); msg != "" {
			r.rep.Errf(key, "error: %s", msg)
			r.rep.Errf(key, "error: invalid rev-index for pack '%s'", p.Path)
			r.fail(ErrorPackRevIndex)
		}
	}
}

// verifyRevIndex checks a .rev file's header, checksum, and that it really is
// the pack's objects in offset order.
func verifyRevIndex(data []byte, p *odb.Pack, algo *gitobj.Algo) string {
	rawsz := algo.RawSize
	want := 12 + int(p.Num)*4 + 2*rawsz
	if len(data) < want {
		return "invalid rev-index size"
	}
	if binary.BigEndian.Uint32(data[0:4]) != 0x52494458 {
		return "invalid rev-index signature"
	}
	if binary.BigEndian.Uint32(data[4:8]) != 1 {
		return "invalid rev-index version"
	}
	if got := binary.BigEndian.Uint32(data[8:12]); got != uint32(algo.Format) {
		return "invalid rev-index hash version"
	}
	h := algo.New()
	h.Write(data[:len(data)-rawsz])
	if !bytes.Equal(h.Sum(nil), data[len(data)-rawsz:]) {
		return "invalid checksum"
	}
	seen := make([]bool, p.Num)
	prev := int64(-1)
	for i := uint32(0); i < p.Num; i++ {
		nr := binary.BigEndian.Uint32(data[12+int(i)*4:])
		if nr >= p.Num {
			return "invalid rev-index position"
		}
		if seen[nr] {
			return "duplicate rev-index position"
		}
		seen[nr] = true
		off := p.OffsetAt(nr)
		if off <= prev {
			return "rev-index is not in pack order"
		}
		prev = off
	}
	return ""
}

// verifyBitmapFiles checks the checksum of every bitmap file, which is all git's
// verify_bitmap_files() does.
func (r *run) verifyBitmapFiles() {
	key := sortKey{phase: phaseIndexFiles, group: 1}
	var names []string
	for _, dir := range r.db.Dirs {
		if midx := filepath.Join(dir.Path, "pack", "multi-pack-index"); fileExists(midx) {
			if hash, ok := midxChecksumName(midx, r.repo.Algo); ok {
				names = append(names, hash)
			}
		}
	}
	for _, p := range r.db.Packs() {
		names = append(names, strings.TrimSuffix(p.Path, ".pack")+".bitmap")
	}
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			continue // it is fine not to have one
		}
		rawsz := r.repo.Algo.RawSize
		if len(data) < rawsz {
			r.rep.Errf(key, "error: bitmap file '%s' has invalid checksum", name)
			r.fail(ErrorBitmap)
			continue
		}
		h := r.repo.Algo.New()
		h.Write(data[:len(data)-rawsz])
		if !bytes.Equal(h.Sum(nil), data[len(data)-rawsz:]) {
			r.rep.Errf(key, "error: bitmap file '%s' has invalid checksum", name)
			r.fail(ErrorBitmap)
		}
	}
}

// midxChecksumName builds the name of a multi-pack-index's bitmap, which
// carries the index's own checksum in its file name.
func midxChecksumName(midxPath string, algo *gitobj.Algo) (string, bool) {
	data, err := os.ReadFile(midxPath)
	if err != nil || len(data) < algo.RawSize {
		return "", false
	}
	sum := data[len(data)-algo.RawSize:]
	return filepath.Join(filepath.Dir(midxPath), "multi-pack-index-"+gitobj.FromBytes(sum).String()+".bitmap"), true
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// verifyGraphFiles checks the commit-graph and the multi-pack-index of every
// object directory, which git does by running its own subcommands.
func (r *run) verifyGraphFiles() {
	if r.repo.Config.Bool("core.commitgraph", true) {
		for _, dir := range r.db.Dirs {
			if !r.verifyCommitGraphs(dir.Path) {
				r.fail(ErrorCommitGraph)
			}
		}
	}
	if r.repo.Config.Bool("core.multipackindex", true) {
		for _, dir := range r.db.Dirs {
			if !r.verifyMultiPackIndex(dir.Path) {
				r.fail(ErrorMultiPackIndex)
			}
		}
	}
}

// commitGraphFiles lists the graph files of one object directory: either the
// single info/commit-graph, or every layer of a commit-graph chain.
func commitGraphFiles(objectDir string) []string {
	single := filepath.Join(objectDir, "info", "commit-graph")
	if fileExists(single) {
		return []string{single}
	}
	chain := filepath.Join(objectDir, "info", "commit-graphs", "commit-graph-chain")
	data, err := os.ReadFile(chain)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, filepath.Join(objectDir, "info", "commit-graphs", "graph-"+line+".graph"))
	}
	return out
}
