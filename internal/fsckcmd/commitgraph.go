package fsckcmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Values the commit-graph format fixes.
const (
	cgSignature    = "CGPH"
	cgVersion      = 1
	cgParentNone   = 0x70000000
	cgExtraEdges   = 0x80000000
	cgEdgeLast     = 0x7fffffff
	cgGenV1Max     = 0x3FFFFFFF
	cgDateOverflow = 0x80000000
)

// commitGraph is one parsed commit-graph layer.
type commitGraph struct {
	path       string
	algo       *gitobj.Algo
	data       []byte
	numCommits uint32
	fanout     []byte // 256 * 4 bytes
	lookup     []byte // numCommits * rawsz
	commitData []byte // numCommits * (rawsz + 16)
	extraEdges []byte
	genData    []byte
	// base is every layer below this one in a chain, oldest first.
	base []*commitGraph
	// lexBase is how many commits the layers below hold, which is where
	// this layer's own positions start.
	lexBase uint32
}

// verifyCommitGraphs checks the commit-graph of one object directory. It
// reports the same lines git's "commit-graph verify" does.
//
// see docs/commit-graph.md
func (r *run) verifyCommitGraphs(objectDir string) bool {
	files := commitGraphFiles(objectDir)
	if len(files) == 0 {
		return true
	}
	key := sortKey{phase: phaseGraphs}
	var layers []*commitGraph
	ok := true
	for _, path := range files {
		g, msg := loadCommitGraph(path, r.repo.Algo)
		if msg != "" {
			r.rep.Errf(key, "%s", msg)
			return false
		}
		g.base = append([]*commitGraph(nil), layers...)
		for _, b := range layers {
			g.lexBase += b.numCommits
		}
		layers = append(layers, g)
	}
	for _, g := range layers {
		if !r.verifyOneCommitGraph(key, g) {
			ok = false
		}
	}
	return ok
}

// loadCommitGraph reads and structurally validates one graph file. The message
// it returns on failure is the one git's parser prints.
func loadCommitGraph(path string, algo *gitobj.Algo) (*commitGraph, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("error: Could not open commit-graph '%s'", path)
	}
	g := &commitGraph{path: path, algo: algo, data: data}
	if len(data) < 8 {
		return nil, "error: commit-graph file is too small"
	}
	if string(data[0:4]) != cgSignature {
		return nil, fmt.Sprintf("error: commit-graph signature %X does not match signature %X",
			binary.BigEndian.Uint32(data[0:4]), 0x43475048)
	}
	if v := data[4]; v != cgVersion {
		return nil, fmt.Sprintf("error: commit-graph version %X does not match version %X", v, cgVersion)
	}
	hashVersion := data[5]
	if uint32(hashVersion) != algo.Format {
		return nil, fmt.Sprintf("error: commit-graph hash version %X does not match version %X",
			hashVersion, algo.Format)
	}
	numChunks := int(data[6])
	rawsz := algo.RawSize
	tableLen := (numChunks + 1) * 12
	if len(data) < 8+tableLen+rawsz {
		return nil, fmt.Sprintf("error: commit-graph file is too small to hold %d chunks", numChunks)
	}
	chunk := func(id string) []byte {
		for i := 0; i < numChunks; i++ {
			off := 8 + i*12
			if string(data[off:off+4]) != id {
				continue
			}
			start := binary.BigEndian.Uint64(data[off+4 : off+12])
			end := binary.BigEndian.Uint64(data[off+16 : off+24])
			if start > end || end > uint64(len(data)) {
				return nil
			}
			return data[start:end]
		}
		return nil
	}
	g.fanout = chunk("OIDF")
	g.lookup = chunk("OIDL")
	g.commitData = chunk("CDAT")
	g.extraEdges = chunk("EDGE")
	g.genData = chunk("GDA2")
	if len(g.fanout) < 256*4 {
		return nil, "error: commit-graph is missing the OID Fanout chunk"
	}
	if g.lookup == nil {
		return nil, "error: commit-graph is missing the OID Lookup chunk"
	}
	if g.commitData == nil {
		return nil, "error: commit-graph is missing the Commit Data chunk"
	}
	g.numCommits = binary.BigEndian.Uint32(g.fanout[255*4:])
	if uint64(g.numCommits)*uint64(rawsz) > uint64(len(g.lookup)) {
		return nil, "error: commit-graph oid table and fanout disagree on size"
	}
	if uint64(g.numCommits)*uint64(rawsz+16) > uint64(len(g.commitData)) {
		return nil, "error: commit-graph is missing the Commit Data chunk"
	}
	return g, ""
}

// verifyOneCommitGraph is git's verify_one_commit_graph().
func (r *run) verifyOneCommitGraph(key sortKey, g *commitGraph) bool {
	ok := true
	report := func(format string, args ...any) {
		ok = false
		r.rep.Errf(key, format, args...)
	}
	// The lite checks come first, because everything after them assumes the
	// tables are the size the header claims.
	for i := 0; i < 255; i++ {
		a := binary.BigEndian.Uint32(g.fanout[i*4:])
		b := binary.BigEndian.Uint32(g.fanout[(i+1)*4:])
		if a > b {
			r.rep.Errf(key, "error: commit-graph fanout values out of order")
			return false
		}
	}
	rawsz := g.algo.RawSize
	if !g.checksumValid() {
		report("the commit-graph file has incorrect checksum and is likely corrupt")
	}
	var prev gitobj.OID
	fanoutPos := 0
	for i := uint32(0); i < g.numCommits; i++ {
		cur := g.algo.FromRaw(g.lookup[int(i)*rawsz:])
		if i > 0 && prev.Compare(cur) >= 0 {
			report("commit-graph has incorrect OID order: %s then %s", prev, cur)
		}
		prev = cur
		for int(cur.H[0]) > fanoutPos {
			value := binary.BigEndian.Uint32(g.fanout[fanoutPos*4:])
			if value != i {
				report("commit-graph has incorrect fanout value: fanout[%d] = %d != %d", fanoutPos, value, i)
			}
			fanoutPos++
		}
	}
	for fanoutPos < 256 {
		value := binary.BigEndian.Uint32(g.fanout[fanoutPos*4:])
		if value != g.numCommits {
			report("commit-graph has incorrect fanout value: fanout[%d] = %d != %d", fanoutPos, value, g.numCommits)
		}
		fanoutPos++
	}
	if !ok {
		// git stops here when the tables themselves disagree, because
		// every check below reads through them.
		return false
	}

	var seenGenZero, seenGenNonZero gitobj.OID
	for i := uint32(0); i < g.numCommits; i++ {
		cur := g.algo.FromRaw(g.lookup[int(i)*rawsz:])
		typ, buf, err := r.readObject(cur)
		if err != nil || typ != gitobj.TypeCommit {
			report("failed to parse commit %s from object database for commit-graph", cur)
			continue
		}
		graphTree, graphParents, generation, date, gok := g.commitAt(i)
		if !gok {
			report("failed to parse commit %s from commit-graph", cur)
			continue
		}
		odbTree, odbParents, odbDate := parseCommitFields(buf, g.algo)
		if graphTree != odbTree {
			report("root tree OID for commit %s in commit-graph is %s != %s", cur, graphTree, odbTree)
		}
		var maxGeneration uint64
		for j, gp := range graphParents {
			if j >= len(odbParents) {
				report("commit-graph parent list for commit %s is too long", cur)
				break
			}
			if gp.oid != odbParents[j] {
				report("commit-graph parent for %s is %s != %s", cur, gp.oid, odbParents[j])
			}
			if gp.generation > maxGeneration {
				maxGeneration = gp.generation
			}
		}
		if len(odbParents) > len(graphParents) {
			report("commit-graph parent list for commit %s terminates early", cur)
		}
		if generation != 0 {
			seenGenNonZero = cur
		} else {
			seenGenZero = cur
		}
		if seenGenZero.Valid() {
			continue
		}
		if g.genData == nil && maxGeneration == cgGenV1Max {
			maxGeneration--
		}
		if generation < maxGeneration+1 {
			report("commit-graph generation for commit %s is %d < %d", cur, generation, maxGeneration+1)
		}
		if date != odbDate {
			report("commit date for commit %s in commit-graph is %d != %d", cur, date, odbDate)
		}
	}
	if seenGenZero.Valid() && seenGenNonZero.Valid() {
		report("commit-graph has both zero and non-zero generations (e.g., commits '%s' and '%s')",
			seenGenZero, seenGenNonZero)
	}
	return ok
}

func (g *commitGraph) checksumValid() bool {
	rawsz := g.algo.RawSize
	if len(g.data) < rawsz {
		return false
	}
	h := g.algo.New()
	h.Write(g.data[:len(g.data)-rawsz])
	return bytes.Equal(h.Sum(nil), g.data[len(g.data)-rawsz:])
}

// graphParent is one parent as the graph records it.
type graphParent struct {
	oid        gitobj.OID
	generation uint64
}

// commitAt decodes one commit's record: its root tree, its parents, its
// generation number, and its date.
func (g *commitGraph) commitAt(i uint32) (tree gitobj.OID, parents []graphParent, generation uint64, date uint64, ok bool) {
	rawsz := g.algo.RawSize
	rec := g.commitData[int(i)*(rawsz+16):]
	tree = g.algo.FromRaw(rec)
	edge0 := binary.BigEndian.Uint32(rec[rawsz:])
	edge1 := binary.BigEndian.Uint32(rec[rawsz+4:])
	packed := binary.BigEndian.Uint32(rec[rawsz+8:])
	dateLow := binary.BigEndian.Uint32(rec[rawsz+12:])
	date = uint64(packed&0x3)<<32 | uint64(dateLow)
	generation = uint64(packed >> 2)
	if g.genData != nil && int(i)*4+4 <= len(g.genData) {
		offset := uint64(binary.BigEndian.Uint32(g.genData[int(i)*4:]))
		if offset&cgDateOverflow != 0 {
			// An offset this large is stored in the overflow chunk,
			// which this reader does not need for verification.
			return tree, nil, generation, date, false
		}
		generation = date + offset
	}
	add := func(pos uint32) bool {
		p, found := g.commitByPosition(pos)
		if !found {
			return false
		}
		parents = append(parents, p)
		return true
	}
	if edge0 == cgParentNone {
		return tree, parents, generation, date, true
	}
	if !add(edge0) {
		return tree, nil, generation, date, false
	}
	if edge1 == cgParentNone {
		return tree, parents, generation, date, true
	}
	if edge1&cgExtraEdges == 0 {
		if !add(edge1) {
			return tree, nil, generation, date, false
		}
		return tree, parents, generation, date, true
	}
	// Three or more parents continue in the extra edges chunk.
	pos := int(edge1&cgEdgeLast) * 4
	for {
		if pos+4 > len(g.extraEdges) {
			return tree, nil, generation, date, false
		}
		edge := binary.BigEndian.Uint32(g.extraEdges[pos:])
		pos += 4
		if !add(edge & cgEdgeLast) {
			return tree, nil, generation, date, false
		}
		if edge&cgExtraEdges != 0 {
			break
		}
	}
	return tree, parents, generation, date, true
}

// commitByPosition resolves a graph position, which counts through the layers
// of a chain before reaching this one.
func (g *commitGraph) commitByPosition(pos uint32) (graphParent, bool) {
	if pos < g.lexBase {
		for _, b := range g.base {
			if pos < b.lexBase+b.numCommits && pos >= b.lexBase {
				local := pos - b.lexBase
				oid := b.algo.FromRaw(b.lookup[int(local)*b.algo.RawSize:])
				_, _, gen, _, ok := b.commitAt(local)
				return graphParent{oid: oid, generation: gen}, ok
			}
		}
		return graphParent{}, false
	}
	local := pos - g.lexBase
	if local >= g.numCommits {
		return graphParent{}, false
	}
	oid := g.algo.FromRaw(g.lookup[int(local)*g.algo.RawSize:])
	_, _, gen, _, ok := g.commitAt(local)
	return graphParent{oid: oid, generation: gen}, ok
}

// parseCommitFields pulls the root tree, the parents, and the commit date out of
// a commit object.
func parseCommitFields(buf []byte, algo *gitobj.Algo) (tree gitobj.OID, parents []gitobj.OID, date uint64) {
	hexsz := algo.HexSize
	if len(buf) > hexsz+5 && string(buf[:5]) == "tree " {
		tree, _ = algo.ParseHexBytes(buf[5:])
		buf = buf[6+hexsz:]
	}
	for len(buf) > hexsz+7 && string(buf[:7]) == "parent " {
		p, ok := algo.ParseHexBytes(buf[7:])
		if !ok {
			break
		}
		parents = append(parents, p)
		buf = buf[8+hexsz:]
	}
	// The committer line carries the date git records in the graph.
	for len(buf) > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := buf[:nl]
		if bytes.HasPrefix(line, []byte("committer ")) {
			if gt := bytes.LastIndexByte(line, '>'); gt > 0 {
				fields := bytes.Fields(line[gt+1:])
				if len(fields) >= 1 {
					var v uint64
					for _, c := range fields[0] {
						if c < '0' || c > '9' {
							v = 0
							break
						}
						v = v*10 + uint64(c-'0')
					}
					date = v
				}
			}
			break
		}
		if nl == 0 {
			break
		}
		buf = buf[nl+1:]
	}
	return tree, parents, date
}
