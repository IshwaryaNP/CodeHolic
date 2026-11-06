package src

import (
	"sync"
)

// Index provides a thread-safe in-memory mapping from keys to their on-disk byte offsets.
// All operations synchronize access via an internal sync.RWMutex.
type Index struct {
	mu sync.RWMutex
	m  map[string]int64
}

// NewIndex initializes an Index with an initial capacity hint.
func NewIndex(sizeHint int) *Index {
	if sizeHint <= 0 {
		sizeHint = 16
	}
	return &Index{
		m: make(map[string]int64, sizeHint),
	}
}

// Set associates the key with the given file offset.
func (idx *Index) Set(key string, off int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.m[key] = off
}

// Get retrieves the byte offset for key. Returns (offset, true) if found, (0, false) otherwise.
func (idx *Index) Get(key string) (int64, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	off, ok := idx.m[key]
	return off, ok
}

// Delete removes key from the in-memory index.
func (idx *Index) Delete(key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.m, key)
}

// Len returns the number of active keys in the index.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.m)
}

// Range calls fn sequentially for each key and offset present in the index under RLock.
// If fn returns false, iteration stops early.
//
// Deadlock Warning: fn must NOT call Index mutators (e.g. Set, Delete, Replace), as doing so
// would attempt to acquire a write lock while this goroutine holds an RLock, causing a deadlock.
func (idx *Index) Range(fn func(key string, off int64) bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for k, v := range idx.m {
		if !fn(k, v) {
			break
		}
	}
}

// Replace atomically replaces the entire in-memory map under a write lock.
// It creates a new internal map copying the entries from m.
func (idx *Index) Replace(m map[string]int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	newM := make(map[string]int64, len(m))
	for k, v := range m {
		newM[k] = v
	}
	idx.m = newM
}

