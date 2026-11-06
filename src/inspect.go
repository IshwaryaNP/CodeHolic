package src

import "os"

// Snapshot returns a copy of the current index as a map[string]int64.
// Safe to call concurrently; holds RLock while copying.
func (db *DB) Snapshot() map[string]int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make(map[string]int64, len(db.index.m))
	for k, v := range db.index.m {
		out[k] = v
	}
	return out
}

// WriteOffset returns the current append position in the file.
func (db *DB) WriteOffset() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.writeOffset
}

// DBPath returns the file path of the database.
func (db *DB) DBPath() string {
	return db.path
}

// FileSize returns the actual on-disk size of the database file.
func (db *DB) FileSize() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	fi, err := os.Stat(db.path)
	if err != nil {
		return db.writeOffset
	}
	return fi.Size()
}

// IsBroken returns true if the DB is in a broken state.
func (db *DB) IsBroken() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.broken
}
