package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	// copyFile is a seam so tests can simulate a torn copy. Defaults to the
	// package-level copyFile.
	copyFile func(src, dst string) error

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
	return &dbSource{path: path, name: name, live: live, copyFile: copyFile}, nil
}

func (s *dbSource) Query(q string, args ...any) (*sql.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. A warm snapshot wins outright: contention is sticky for the TTL, so
	// we do not re-probe the live file on every call.
	if s.snap != nil && time.Since(s.snapAt) < snapshotTTL {
		return s.snap.Query(q, args...)
	}

	// 2. The live file. This is the common case, including while Thunderbird
	// runs: plain WAL readers coexist with an active writer just fine.
	rows, err := s.live.Query(q, args...)
	if err == nil {
		return rows, nil
	}
	// 3. Only a lock justifies a copy. Missing tables, bad SQL and corruption
	// are real errors and must reach the caller unchanged.
	if !isBusy(err) {
		return nil, err
	}
	if err := s.refreshSnapshot(); err != nil {
		return nil, err
	}
	return s.snap.Query(q, args...)
}

// refreshSnapshot ensures s.snap is usable, re-copying only when the source
// has actually changed since the last copy.
func (s *dbSource) refreshSnapshot() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return s.snapshotErr(err)
	}
	if s.snap != nil && st.ModTime().Equal(s.srcMod) {
		s.snapAt = time.Now() // unchanged source; keep the copy, extend the window
		return nil
	}
	return s.makeSnapshot(st.ModTime())
}

func (s *dbSource) makeSnapshot(mod time.Time) error {
	if s.snap != nil {
		s.snap.Close()
		s.snap = nil
	}
	if s.snapDir == "" {
		dir, err := os.MkdirTemp("", "thunderbird-mcp-"+s.name+"-")
		if err != nil {
			return s.snapshotErr(err)
		}
		s.snapDir = dir
	}
	dst := filepath.Join(s.snapDir, filepath.Base(s.path))
	// The -wal is mandatory: a copy of the main file alone cannot even see the
	// schema. The -shm is deliberately skipped - it adds nothing, and a stale
	// copy of a volatile index into the WAL is a liability.
	for _, suffix := range []string{"", "-wal"} {
		if err := s.copyFile(s.path+suffix, dst+suffix); err != nil {
			return s.snapshotErr(err)
		}
	}
	// Read-write, not mode=ro: the copy was taken while Thunderbird may have
	// been mid-transaction, so its WAL can need recovery, and only a
	// read-write connection can run recovery. The copy is a private
	// throwaway, so write access costs nothing.
	db, err := sql.Open("sqlite", "file:"+dst)
	if err != nil {
		return s.snapshotErr(err)
	}
	s.snap, s.snapAt, s.srcMod = db, time.Now(), mod
	return nil
}

// snapshotErr explains the real cause and the fix. Surfacing a bare
// "database is locked" here would tell the user nothing actionable.
func (s *dbSource) snapshotErr(err error) error {
	return fmt.Errorf("%s: Thunderbird is holding %s and it could not be copied: %w; free space in $TMPDIR or close Thunderbird",
		s.name, filepath.Base(s.path), err)
}

// copyFile copies src to dst. A missing source is not an error - the -wal may
// legitimately be absent - but any stale dst is removed so the snapshot never
// pairs a fresh main file with a leftover WAL.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		if rmErr := os.Remove(dst); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
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
	if s.snap != nil {
		errs = append(errs, s.snap.Close())
	}
	if s.snapDir != "" {
		errs = append(errs, os.RemoveAll(s.snapDir))
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
