package fsckcmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// checkPackRevIndexes verifies each pack's reverse index, if one is on disk.
// git splits this in two: a load, which refuses a file it cannot read at all,
// and a verify, which reads the contents. The two report differently.
func (r *run) checkPackRevIndexes() {
	key := sortKey{phase: phaseIndexFiles}
	for _, p := range r.db.Packs() {
		if p.OpenErr != nil {
			continue
		}
		shown := strings.TrimSuffix(p.Path, ".pack") + ".rev"
		data, err := os.ReadFile(strings.TrimSuffix(p.File, ".pack") + ".rev")
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			r.rep.Errf(key, "error: unable to load rev-index for pack '%s'", p.Path)
			r.fail(ErrorPackRevIndex)
			continue
		}
		if msg := loadRevIndex(data, p, r.repo.Algo); msg != "" {
			r.rep.Errf(key, "error: reverse-index file %s %s", shown, msg)
			r.rep.Errf(key, "error: unable to load rev-index for pack '%s'", p.Path)
			r.fail(ErrorPackRevIndex)
			continue
		}
		if msgs := verifyRevIndex(data, p, r.repo.Algo); len(msgs) > 0 {
			for _, msg := range msgs {
				r.rep.Errf(key, "error: %s", msg)
			}
			r.rep.Errf(key, "error: invalid rev-index for pack '%s'", p.Path)
			r.fail(ErrorPackRevIndex)
		}
	}
}

// revIndexSize is git's revindex_size(): a header, one index position per
// object, and the pack checksum next to the file's own.
func revIndexSize(num uint32, rawsz int) int { return 12 + int(num)*4 + 2*rawsz }

// loadRevIndex is git's load_revindex_from_disk(). It returns the tail of the
// "reverse-index file %s ..." message, or "" when the file loads.
func loadRevIndex(data []byte, p *odb.Pack, algo *gitobj.Algo) string {
	want := revIndexSize(p.Num, algo.RawSize)
	switch {
	case len(data) < want:
		return "is too small"
	case len(data) != want:
		return "is corrupt"
	}
	if binary.BigEndian.Uint32(data[0:4]) != 0x52494458 {
		return "has unknown signature"
	}
	if v := binary.BigEndian.Uint32(data[4:8]); v != 1 {
		return fmt.Sprintf("has unsupported version %d", v)
	}
	// git accepts either hash here without comparing it to the repository's
	// own, so this does too.
	if h := binary.BigEndian.Uint32(data[8:12]); h != 1 && h != 2 {
		return fmt.Sprintf("has unsupported hash id %d", h)
	}
	return ""
}

// verifyRevIndex is git's verify_pack_revindex(): the file's checksum, then the
// stored order against the order the index itself implies.
func verifyRevIndex(data []byte, p *odb.Pack, algo *gitobj.Algo) []string {
	var msgs []string
	rawsz := algo.RawSize
	h := algo.New()
	h.Write(data[:len(data)-rawsz])
	if !bytes.Equal(h.Sum(nil), data[len(data)-rawsz:]) {
		msgs = append(msgs, "invalid checksum")
	}
	// The in-memory reverse index is the pack's objects in offset order,
	// each carrying the position it holds in the index.
	order := make([]uint32, p.Num)
	for i := range order {
		order[i] = uint32(i)
	}
	sort.Slice(order, func(a, b int) bool { return p.OffsetAt(order[a]) < p.OffsetAt(order[b]) })
	for i, nr := range order {
		stored := binary.BigEndian.Uint32(data[12+i*4:])
		if nr != stored {
			msgs = append(msgs, fmt.Sprintf("invalid rev-index position at %d: %d != %d", i, nr, stored))
		}
	}
	return msgs
}

// verifyBitmapFiles checks the checksum of every bitmap file, which is all git's
// verify_bitmap_files() does.
func (r *run) verifyBitmapFiles() {
	key := sortKey{phase: phaseIndexFiles, group: 1}
	// Each bitmap has the path this process opens and the path git prints.
	var files [][2]string
	for _, dir := range r.db.Dirs {
		if midx := filepath.Join(dir.Path, "pack", "multi-pack-index"); fileExists(midx) {
			if name, ok := midxChecksumName(midx, r.repo.Algo); ok {
				files = append(files, [2]string{
					filepath.Join(dir.Path, "pack", name),
					filepath.Join(dir.Display, "pack", name),
				})
			}
		}
	}
	for _, p := range r.db.Packs() {
		files = append(files, [2]string{
			strings.TrimSuffix(p.File, ".pack") + ".bitmap",
			strings.TrimSuffix(p.Path, ".pack") + ".bitmap",
		})
	}
	for _, f := range files {
		data, err := os.ReadFile(f[0])
		if err != nil {
			continue // it is fine not to have one
		}
		rawsz := r.repo.Algo.RawSize
		if len(data) >= rawsz {
			h := r.repo.Algo.New()
			h.Write(data[:len(data)-rawsz])
			if bytes.Equal(h.Sum(nil), data[len(data)-rawsz:]) {
				continue
			}
		}
		r.rep.Errf(key, "error: bitmap file '%s' has invalid checksum", f[1])
		r.fail(ErrorBitmap)
	}
}

// midxChecksumName builds the file name of a multi-pack-index's bitmap, which
// carries the index's own checksum.
func midxChecksumName(midxPath string, algo *gitobj.Algo) (string, bool) {
	data, err := os.ReadFile(midxPath)
	if err != nil || len(data) < algo.RawSize {
		return "", false
	}
	sum := data[len(data)-algo.RawSize:]
	return "multi-pack-index-" + gitobj.FromBytes(sum).String() + ".bitmap", true
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
			if !r.verifyCommitGraphs(dir) {
				r.fail(ErrorCommitGraph)
			}
		}
	}
	if r.repo.Config.Bool("core.multipackindex", true) {
		for _, dir := range r.db.Dirs {
			if !r.verifyMultiPackIndex(dir) {
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
