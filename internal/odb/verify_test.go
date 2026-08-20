package odb_test

// The pack walk under a memory cap.
//
// The walk decodes a chain from the top down and holds each level while the
// deltas below it are built. Past its budget it stops holding one and rebuilds
// it later from its own chain instead, which is a second way of arriving at the
// same object and must arrive at the same bytes.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// branchingPack is a delta tree that branches at every level, so that a walk
// with no room to spare has to defer and rebuild rather than descending.
//
//	0                  whole
//	├── 1              delta on 0
//	│   ├── 3          delta on 1
//	│   └── 4          delta on 1
//	├── 2              delta on 0
//	│   └── 5          delta on 2
//	└── 6              delta on 0
func branchingPack() []gittest.PackObject {
	body := func(tag string) []byte {
		return []byte(strings.Repeat("a line that deltas well\n", 400) + tag + "\n")
	}
	return []gittest.PackObject{
		{Type: gitobj.TypeBlob, Data: body("root"), Base: -1},
		{Type: gitobj.TypeBlob, Data: body("child one"), Base: 0},
		{Type: gitobj.TypeBlob, Data: body("child two"), Base: 0},
		{Type: gitobj.TypeBlob, Data: body("grandchild one"), Base: 1},
		{Type: gitobj.TypeBlob, Data: body("grandchild two"), Base: 1},
		{Type: gitobj.TypeBlob, Data: body("grandchild three"), Base: 2},
		{Type: gitobj.TypeBlob, Data: body("child three"), Base: 0},
	}
}

// linearPack is one chain with no branch in it, which is what a large file
// rewritten many times looks like.
func linearPack(n int) []gittest.PackObject {
	objs := make([]gittest.PackObject, n)
	for i := range objs {
		body := strings.Repeat("a line that deltas well\n", 400) + fmt.Sprintf("revision %d\n", i)
		objs[i] = gittest.PackObject{Type: gitobj.TypeBlob, Data: []byte(body), Base: i - 1}
	}
	objs[0].Base = -1
	return objs
}

// verified is what one walk of a pack produced: every object it handed back,
// and every complaint it made.
type verified struct {
	objects map[gitobj.OID]string
	errors  []string
	ok      bool
}

// walkPack verifies a pack at one budget and records everything it said.
func walkPack(t *testing.T, path string, workers int, budget int64) verified {
	t.Helper()
	p, err := odb.OpenPack(strings.TrimSuffix(path, ".pack")+".idx",
		strings.TrimSuffix(path, ".pack")+".idx", gitobj.SHA1, true)
	require.NoError(t, err)
	defer p.Close()

	got := verified{objects: map[gitobj.OID]string{}}
	got.ok = p.Verify(odb.VerifyOpts{
		Workers:     workers,
		ChainBudget: budget,
		Emit: func(oid gitobj.OID, text string) {
			got.errors = append(got.errors, text)
		},
		Object: func(oid gitobj.OID, typ gitobj.Type, size int64, data []byte) {
			// The payload is only borrowed, so what is kept is a digest of
			// it. Comparing those is what proves the two walks agree.
			got.objects[oid] = fmt.Sprintf("%s %d %s", typ.Name(), size,
				odb.HashLiteral(gitobj.SHA1, typ.Name(), data))
		},
	})
	return got
}

func TestAChainBudgetChangesNothingButMemory(t *testing.T) {
	gittest.RequireGit(t)
	for _, c := range []struct {
		name string
		objs []gittest.PackObject
	}{
		{"branching", branchingPack()},
		{"linear", linearPack(12)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := gittest.New(t)
			path, _ := r.WritePack("test", c.objs)
			want := walkPack(t, path, 1, 0)
			require.True(t, want.ok, "the pack must verify: %v", want.errors)
			require.Len(t, want.objects, len(c.objs))

			for _, budget := range []int64{1, 64, 4096} {
				for _, workers := range []int{1, 4} {
					got := walkPack(t, path, workers, budget)
					assert.True(t, got.ok, "budget %d: %v", budget, got.errors)
					assert.Equal(t, want.objects, got.objects,
						"budget %d with %d workers decoded different objects",
						budget, workers)
				}
			}
		})
	}
}

// TestABudgetOfOneStillDecodesADeferredSubtree pins the case the budget exists
// for.
//
// The root of branchingPack has three children and the first of them has
// children of its own. A walk holding one byte cannot descend into a child that
// is not its parent's last, so that subtree is deferred and comes back through
// rebuild. The objects below it exist in the result only if that path decoded
// them, and they have to be the same bytes as the walk that had room.
func TestABudgetOfOneStillDecodesADeferredSubtree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	objs := branchingPack()
	path, _ := r.WritePack("test", objs)

	loose := walkPack(t, path, 1, 0)
	tight := walkPack(t, path, 1, 1)
	require.True(t, tight.ok, "%v", tight.errors)
	assert.Equal(t, loose.objects, tight.objects)
	assert.Len(t, tight.objects, len(objs))
}

// TestACorruptDeltaIsReportedWhateverTheBudget keeps the cap from swallowing a
// finding. A deferred entry is decoded a second way, and the second way has to
// refuse the same object the first way refused.
func TestACorruptDeltaIsReportedWhateverTheBudget(t *testing.T) {
	gittest.RequireGit(t)
	objs := branchingPack()
	// Object 3 hangs below the child the tight budget defers, so it is
	// reached through rebuild there and through the descent otherwise.
	const broken = 3
	for _, budget := range []int64{0, 1} {
		r := gittest.New(t)
		path, offsets := r.WritePack("test", objs)
		// The index is built before this, so it still describes the pack
		// as it was: a good index over a bad entry, which is the shape of
		// the damage this tool exists for.
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[(offsets[broken]+offsets[broken+1])/2] ^= 0xff
		gittest.WriteOver(t, path, data)

		got := walkPack(t, path, 1, budget)
		assert.False(t, got.ok, "budget %d: a corrupt entry must fail the pack", budget)
		assert.NotEmpty(t, got.errors, "budget %d: it must say what is wrong", budget)
		assert.NotContains(t, got.objects, hashOf(objs[broken]),
			"budget %d: a corrupt entry must not be handed back", budget)
	}
}

// hashOf is the name the object in a pack entry would have.
func hashOf(o gittest.PackObject) gitobj.OID {
	return odb.HashLiteral(gitobj.SHA1, o.Type.Name(), o.Data)
}

// TestAnEntryCannotAskForMoreThanThePackHolds is about the number in an entry's
// header rather than the bytes after it.
//
// That number says how big the object will be once it is inflated, and a walk
// that takes it at its word reserves it before reading a byte of the payload.
// Four wrong bytes there ask for thirty-two terabytes, and the run dies with
// the runtime's own out-of-memory message, naming no object and no pack. No
// stream of the length actually available can inflate that far, so the entry is
// refused and reported like any other that will not decode.
func TestAnEntryCannotAskForMoreThanThePackHolds(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	objs := branchingPack()
	path, offsets := r.WritePack("test", objs)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// The header is rewritten in place to claim 32 TB. It runs over the
	// start of the entry's own stream, which is what an entry with a wrong
	// size looks like from here anyway.
	copy(data[offsets[1]:], packHeaderClaiming(gitobj.TypeBlob, 1<<45))
	gittest.WriteOver(t, path, data)

	got := walkPack(t, path, 1, 0)
	assert.False(t, got.ok, "an entry that cannot be read must fail the pack")
	assert.NotEmpty(t, got.errors, "and it must say which one")
	assert.NotContains(t, got.objects, hashOf(objs[1]))
}

// packHeaderClaiming builds the type and size a pack entry starts with, in the
// encoding git's unpack_object_header_buffer reads back.
func packHeaderClaiming(typ gitobj.Type, size int64) []byte {
	out := []byte{byte(typ)<<4 | byte(size&0xf)}
	for size >>= 4; size > 0; size >>= 7 {
		out[len(out)-1] |= 0x80
		out = append(out, byte(size&0x7f))
	}
	return out
}
