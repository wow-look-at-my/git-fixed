package odb

import (
	"container/list"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// deltaCacheLimit is how much reconstructed pack data the cache holds.
const deltaCacheLimit = 96 << 20

// deltaCacheShards splits the cache so several workers rarely wait on the same lock.
const deltaCacheShards = 64

// deltaKey names one entry in a pack.
type deltaKey struct {
	pack *Pack
	off  int64
}

// deltaCache remembers objects that deltas were built from. see docs/pack-verification.md
type deltaCache struct {
	shards [deltaCacheShards]deltaShard
}

type deltaShard struct {
	mu   sync.Mutex
	m    map[deltaKey]*list.Element
	lru  *list.List
	size int64
}

// deltaValue is one cached object. The bytes are shared and never written to.
type deltaValue struct {
	key  deltaKey
	typ  gitobj.Type
	data []byte
}

func newDeltaCache() *deltaCache {
	c := &deltaCache{}
	for i := range c.shards {
		c.shards[i].m = make(map[deltaKey]*list.Element)
		c.shards[i].lru = list.New()
	}
	return c
}

func (c *deltaCache) shard(off int64) *deltaShard {
	// Offsets grow by whole objects, so the low bits alone would land neighbours in one shard.
	h := uint64(off)
	h ^= h >> 17
	return &c.shards[h%deltaCacheShards]
}

// get returns a cached object. The caller must only read the bytes.
func (c *deltaCache) get(p *Pack, off int64) (gitobj.Type, []byte, bool) {
	s := c.shard(off)
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.m[deltaKey{p, off}]
	if !ok {
		return gitobj.TypeNone, nil, false
	}
	s.lru.MoveToFront(el)
	v := el.Value.(*deltaValue)
	return v.typ, v.data, true
}

// put records an object, dropping the least recently used entries to stay
// inside the limit.
func (c *deltaCache) put(p *Pack, off int64, typ gitobj.Type, data []byte) {
	if int64(len(data)) > deltaCacheLimit/deltaCacheShards {
		// One object that fills a whole shard would evict everything else for a single hit.
		return
	}
	key := deltaKey{p, off}
	s := c.shard(off)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.m[key]; ok {
		s.lru.MoveToFront(el)
		return
	}
	s.m[key] = s.lru.PushFront(&deltaValue{key: key, typ: typ, data: data})
	s.size += int64(len(data))
	for s.size > deltaCacheLimit/deltaCacheShards {
		back := s.lru.Back()
		if back == nil {
			break
		}
		v := s.lru.Remove(back).(*deltaValue)
		delete(s.m, v.key)
		s.size -= int64(len(v.data))
	}
}
