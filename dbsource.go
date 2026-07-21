package main

import (
	"database/sql"
	"errors"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

// snapshotTTL bounds how often a contended database is re-copied. Gloda's
// mtime churns constantly while Thunderbird indexes, so without this debounce
// an active indexer would mean a fresh copy on nearly every tool call.
const snapshotTTL = 60 * time.Second

// dbSource owns one SQLite file in the Thunderbird profile. Reads go to the
// live file; when Thunderbird holds a lock, reads transparently fall back to a
// private snapshot copy. See specs/2026-07-21-live-profile-db-access-design.md.
type dbSource struct {
	path string // the file inside the profile
	name string // "gloda" | "contacts" | "calendar", used in error messages

	mu      sync.Mutex
	live    *sql.DB
	snap    *sql.DB   // nil until the first contention
	snapDir string    // temp dir holding the copy; created lazily
	snapAt  time.Time // when snap was taken
	srcMod  time.Time // mtime of path when snap was taken
}

func openDBSource(path, name string) (*dbSource, error) {
	live, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// sql.Open is lazy, so without this probe a missing or corrupt file is
	// only discovered on the first tool call, long after the "unavailable"
	// log line would have been useful. A locked file is not a failure here:
	// it just means the first real query will snapshot.
	var n int
	if err := live.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil && !isBusy(err) {
		live.Close()
		return nil, err
	}
	return &dbSource{path: path, name: name, live: live}, nil
}

func (s *dbSource) Query(q string, args ...any) (*sql.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live.Query(q, args...)
}

func (s *dbSource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.live != nil {
		errs = append(errs, s.live.Close())
	}
	return errors.Join(errs...)
}

// isBusy reports whether err is SQLite's "the file is locked by someone else"
// signal, the only condition that justifies falling back to a snapshot.
func isBusy(err error) bool {
	return sqliteCodeIn(err, 5 /* SQLITE_BUSY */, 6 /* SQLITE_LOCKED */)
}

// sqliteCodeIn matches against primary result codes; extended codes are
// (primary | n<<8), so the low byte is what identifies the class.
func sqliteCodeIn(err error, codes ...int) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Code()&0xff == c {
			return true
		}
	}
	return false
}
