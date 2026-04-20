package search

import (
	"math"
	"sort"
	"sync"
)

// Entry is one row of the in-memory vector index: a game identifier and
// its embedding. Small value type — copied by the top-K sort without
// allocation surprises.
type Entry struct {
	GamePath string
	System   string
	Vector   []float32
}

// Hit is a single search result emitted by Top. GamePath+System
// uniquely identifies the game row (matches the IGDB cache's PK),
// Score is normalized cosine similarity in [-1, 1]; higher is better.
type Hit struct {
	GamePath string  `json:"game_path"`
	System   string  `json:"system"`
	Score    float32 `json:"score"`
}

// Index is a flat vector store. Thread-safe; readers and writers take
// an RWMutex. For library sizes < ~10 000 a linear cosine scan is
// <5 ms and beats the overhead of any ANN structure at this scale.
type Index struct {
	mu      sync.RWMutex
	entries []Entry
	byKey   map[string]int // path|system → position in entries
}

// NewIndex returns an empty index. Populate via Upsert or LoadAll.
func NewIndex() *Index {
	return &Index{byKey: make(map[string]int)}
}

// Upsert inserts or replaces the entry for (gamePath, system). Copies
// the caller's vector so later mutations on the original are safe.
func (x *Index) Upsert(e Entry) {
	key := e.GamePath + "|" + e.System
	vec := make([]float32, len(e.Vector))
	copy(vec, e.Vector)
	e.Vector = vec
	x.mu.Lock()
	defer x.mu.Unlock()
	if i, ok := x.byKey[key]; ok {
		x.entries[i] = e
		return
	}
	x.byKey[key] = len(x.entries)
	x.entries = append(x.entries, e)
}

// Delete removes the entry for (gamePath, system). No-op if absent.
func (x *Index) Delete(gamePath, system string) {
	key := gamePath + "|" + system
	x.mu.Lock()
	defer x.mu.Unlock()
	i, ok := x.byKey[key]
	if !ok {
		return
	}
	// Swap-with-last so the slice stays contiguous without shifting.
	last := len(x.entries) - 1
	if i != last {
		x.entries[i] = x.entries[last]
		moved := x.entries[i]
		x.byKey[moved.GamePath+"|"+moved.System] = i
	}
	x.entries = x.entries[:last]
	delete(x.byKey, key)
}

// Size returns the number of indexed entries.
func (x *Index) Size() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.entries)
}

// Top computes cosine similarity between the query vector and every
// indexed entry, then returns the K best hits. Returns fewer if the
// index holds <K entries or the query is zero-length.
//
// Assumes both sides are L2-normalized; Qwen3-Embedding-0.6B's
// pooling output is NOT normalized by default, so we normalize the
// query on entry. Indexed vectors are normalized at insert via
// NormalizeInPlace — keeps the hot loop a bare dot product.
func (x *Index) Top(query []float32, k int) []Hit {
	if k <= 0 || len(query) == 0 {
		return nil
	}
	q := make([]float32, len(query))
	copy(q, query)
	NormalizeInPlace(q)

	x.mu.RLock()
	defer x.mu.RUnlock()
	scored := make([]Hit, 0, len(x.entries))
	for _, e := range x.entries {
		if len(e.Vector) != len(q) {
			continue // dimension mismatch — skip rather than crash
		}
		var dot float32
		for i, qv := range q {
			dot += qv * e.Vector[i]
		}
		scored = append(scored, Hit{GamePath: e.GamePath, System: e.System, Score: dot})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

// NormalizeInPlace scales v to unit length. Zero vectors stay zero.
// Exported so callers can normalize batches before Upsert.
func NormalizeInPlace(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}
