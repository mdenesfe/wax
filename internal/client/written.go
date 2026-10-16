package client

import "sync"

// maxWritten bounds a writtenCache; it is reset when full, which costs at
// most one redundant write per entry afterwards.
const maxWritten = 10000

// writtenCache remembers values already written to the database (LID
// mappings, push names), so the hot message path skips upserts that would
// not change anything. It only suppresses writes; a miss writes as before.
type writtenCache struct {
	mu sync.Mutex
	m  map[string]string
}

// unchanged reports whether key was last written with value, and records
// value as written otherwise. Callers write when it returns false and
// call forget if that write fails.
func (w *writtenCache) unchanged(key, value string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.m[key]; ok && v == value {
		return true
	}
	if w.m == nil || len(w.m) >= maxWritten {
		w.m = make(map[string]string)
	}
	w.m[key] = value
	return false
}

func (w *writtenCache) forget(key string) {
	w.mu.Lock()
	delete(w.m, key)
	w.mu.Unlock()
}
