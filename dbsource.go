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
	snapDir string    // temp dir holding the copies; created lazily
	snapGen int       // generation counter; each copy gets its own subdirectory
	snapSub string    // current generation's subdirectory inside snapDir
	snapAt  time.Time // when snap was taken
	srcMod  time.Time // mtime of path when snap was taken
	walMod  time.Time // mtime of path+"-wal" when snap was taken; zero if absent
	walSize int64     // size of path+"-wal" when snap was taken; 0 if absent
}

// srcState fingerprints what the snapshot was made from. The -wal belongs in
// it because in WAL mode Thunderbird's writes land in the -wal first and the
// main file's mtime need not move until a checkpoint. Watching the main file
// alone lets a snapshot stay stale indefinitely while writes keep arriving.
// Size matters as much as mtime: WAL growth is how new data announces itself,
// and a same-second append would otherwise read as "unchanged".
type srcState struct {
	mod     time.Time
	walMod  time.Time
	walSize int64
}

// statSource fingerprints the main file and its -wal. A missing -wal is not an
// error: it disappears legitimately when Thunderbird checkpoints. Its absence
// is recorded as the zero state, which differs from any present -wal, so a
// checkpoint that empties it still reads as a change worth re-copying for.
func (s *dbSource) statSource() (srcState, error) {
	st, err := os.Stat(s.path)
	if err != nil {
		return srcState{}, err
	}
	out := srcState{mod: st.ModTime()}
	wst, err := os.Stat(s.path + "-wal")
	switch {
	case err == nil:
		out.walMod, out.walSize = wst.ModTime(), wst.Size()
	case !errors.Is(err, os.ErrNotExist):
		return srcState{}, err
	}
	return out, nil
}

// matches reports whether the snapshot was taken from exactly this state.
func (s *dbSource) matches(st srcState) bool {
	return st.mod.Equal(s.srcMod) && st.walMod.Equal(s.walMod) && st.walSize == s.walSize
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
	rows, err = s.snap.Query(q, args...)
	if err == nil || !isTorn(err) {
		return rows, err
	}
	// 4. The copy was taken mid-write and is unusable. Take it again - the
	// window that produced it has almost certainly closed. Retry exactly once:
	// a second torn copy is not a race, and its error is the honest answer.
	if err := s.makeSnapshotNow(); err != nil {
		return nil, err
	}
	return s.snap.Query(q, args...)
}

// makeSnapshotNow forces a fresh copy, bypassing the mtime shortcut in
// refreshSnapshot. That shortcut would be actively wrong here: the source
// mtime is unchanged since the torn copy, so it would hand back the very
// snapshot we just rejected.
func (s *dbSource) makeSnapshotNow() error {
	st, err := s.statSource()
	if err != nil {
		return s.snapshotErr(err)
	}
	// makeSnapshot allocates the next generation directory, so the retry
	// writes to a path of its own and never rewrites the torn file in place.
	return s.makeSnapshot(st)
}

// refreshSnapshot ensures s.snap is usable, re-copying only when the source
// has actually changed since the last copy.
func (s *dbSource) refreshSnapshot() error {
	st, err := s.statSource()
	if err != nil {
		return s.snapshotErr(err)
	}
	if s.snap != nil && s.matches(st) {
		s.snapAt = time.Now() // unchanged source; keep the copy, extend the window
		return nil
	}
	return s.makeSnapshot(st)
}

// makeSnapshot builds a new generation and swaps it in only once it is fully
// open. Nothing about the existing snapshot is touched until that succeeds, so
// a failed refresh degrades to "keep serving the older copy" rather than
// leaving the source with no snapshot at all. Every failure path also removes
// the generation directory it created: this is a long-lived server process, so
// a directory abandoned here would hold a partial copy of a database until
// exit, and a persistent failure (a full $TMPDIR, say) would leak one per tool
// call and keep the very condition causing it from ever clearing.
func (s *dbSource) makeSnapshot(st srcState) error {
	if s.snapDir == "" {
		dir, err := os.MkdirTemp("", "thunderbird-mcp-"+s.name+"-")
		if err != nil {
			return s.snapshotErr(err)
		}
		s.snapDir = dir
	}
	// Each generation gets its own directory. Closing the previous *sql.DB
	// later does not stop a caller still iterating *sql.Rows from an earlier
	// snapshot: its file descriptor points at the inode we would otherwise
	// truncate and rewrite, so that reader could observe a torn page mid-scan
	// (SQLITE_CORRUPT, or silently wrong rows). Writing to a fresh path
	// instead leaves the old inode untouched for as long as anyone holds it.
	prevSub, prevDB := s.snapSub, s.snap
	s.snapGen++
	sub := filepath.Join(s.snapDir, fmt.Sprintf("gen%d", s.snapGen))
	if err := os.Mkdir(sub, 0o700); err != nil {
		return s.snapshotErr(err)
	}
	dst := filepath.Join(sub, filepath.Base(s.path))
	// The -wal is mandatory: a copy of the main file alone cannot even see the
	// schema. The -shm is deliberately skipped - it adds nothing, and a stale
	// copy of a volatile index into the WAL is a liability.
	for _, suffix := range []string{"", "-wal"} {
		if err := s.copyFile(s.path+suffix, dst+suffix); err != nil {
			_ = os.RemoveAll(sub)
			return s.snapshotErr(err)
		}
	}
	// Read-write, not mode=ro: the copy was taken while Thunderbird may have
	// been mid-transaction, so its WAL can need recovery, and only a
	// read-write connection can run recovery. The copy is a private
	// throwaway, so write access costs nothing.
	db, err := sql.Open("sqlite", "file:"+dst)
	if err != nil {
		_ = os.RemoveAll(sub)
		return s.snapshotErr(err)
	}
	// The new generation is open, so the swap happens here and cannot fail
	// halfway. Everything above this line is either a fresh allocation or a
	// read, which is what lets every failure path return with s.snap, s.snapAt,
	// s.srcMod and the previous generation directory exactly as they were.
	s.snapSub = sub
	s.snap, s.snapAt = db, time.Now()
	s.srcMod, s.walMod, s.walSize = st.mod, st.walMod, st.walSize
	// Only now is the old generation expendable. On Linux and macOS an unlink
	// does not disturb a reader that still holds the file open, which is
	// exactly the semantics we want: an in-flight *sql.Rows from the previous
	// snapshot keeps reading its own inode until the caller is done with it.
	if prevDB != nil {
		_ = prevDB.Close()
	}
	if prevSub != "" {
		_ = os.RemoveAll(prevSub)
	}
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

// isTorn reports whether err indicates the snapshot was copied while
// Thunderbird was mid-write, leaving an unusable file.
//
// SQLITE_ERROR is in this set because a truncated header does not surface as a
// corruption code at all - the file simply has no schema, so the query fails
// with "no such table". That inclusion is only sound because isTorn is
// consulted in exactly one place: a query against a snapshot, never against
// the live file. Never move it to the live rung, or a genuine schema bug would
// be mistaken for a bad copy and silently answered from a stale copy instead.
//
// The snapshot in question is not always freshly copied: refreshSnapshot's
// unchanged-source shortcut hands back an older generation. What is
// guaranteed is only that misjudging such an error cannot hide it: the single
// retry re-copies and then returns that query's error unconditionally, so a
// real schema problem still reaches the caller, one extra copy later.
func isTorn(err error) bool {
	return sqliteCodeIn(err,
		11, // SQLITE_CORRUPT
		26, // SQLITE_NOTADB
		1,  // SQLITE_ERROR - a torn header reads as "no such table"
	)
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
