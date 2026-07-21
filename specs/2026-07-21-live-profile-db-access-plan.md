# Live Profile DB Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make gloda, contacts, and calendar reads succeed while Thunderbird is running, by reading the live profile file first and falling back to a WAL-inclusive snapshot copy when SQLite reports contention.

**Architecture:** One new type, `dbSource`, owns a single profile SQLite file and replaces the `*sql.DB` field in `Gloda`, `Contacts`, and `Calendar`. All three use only `Query` and `Close`, so this is a type change at the call sites, not a restructure. `Query` serves a warm snapshot if one exists inside the TTL, otherwise reads the live file, and falls back to snapshotting only on `SQLITE_BUSY`/`SQLITE_LOCKED`. Every other error propagates untouched.

**Tech Stack:** Go 1.25, `modernc.org/sqlite v1.54.0`, Ginkgo v2 + Gomega for tests.

**Spec:** `specs/2026-07-21-live-profile-db-access-design.md`

## Global Constraints

- Snapshots copy the main file **and `-wal`**. A snapshot of the main file alone cannot see the schema. Do **not** copy `-shm`.
- Snapshot connections are opened **read-write** (plain `file:<path>`, no `mode=ro`), because a copied WAL may need recovery and only a read-write connection can run it.
- `immutable=1` must not appear in any DSN after Task 5. On a WAL database it hides everything since the last checkpoint, including the schema.
- Live reads use exactly `file:<path>?mode=ro&_pragma=busy_timeout(5000)`.
- `snapshotTTL` is a package constant of `60 * time.Second`. Not configurable.
- Only `SQLITE_BUSY` (5) and `SQLITE_LOCKED` (6) trigger the snapshot fallback. Compare with `se.Code() & 0xff` so extended result codes match.
- Tests are Ginkgo specs in `package main`, registered through the existing `TestThunderbird` runner in `thunderbird_suite_test.go`. Do not add a second `go test` entrypoint.
- Run the full suite with `go test ./...` before every commit.

---

### Task 1: `dbSource` skeleton — live reads and lifecycle

Establishes the type and the uncontended read path. No snapshot logic yet.

**Files:**
- Create: `dbsource.go`
- Create: `dbsource_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const snapshotTTL = 60 * time.Second`
  - `func openDBSource(path, name string) (*dbSource, error)`
  - `func (s *dbSource) Query(q string, args ...any) (*sql.Rows, error)`
  - `func (s *dbSource) Close() error`
  - test fixture `walFixture` with `newWALFixture(dir string) *walFixture`, `(*walFixture).lock()`, `(*walFixture).close()`, and field `path`.

**Fixture note — this is load-bearing.** `newWALFixture` **keeps its writing connection open**. Closing it would checkpoint the WAL and delete the `-wal` file, moving every row into the main database. A fixture in that state cannot detect the bug being fixed: `immutable=1` reads a checkpointed database perfectly well, so the Task 5 regression test would pass against the broken code. Verified: after closing the writer, `-wal` is gone and `immutable=1` returns all 3 rows. Do not "simplify" this fixture by closing the writer.

- [ ] **Step 1: Write the failing test**

Create `dbsource_test.go`:

```go
package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	_ "modernc.org/sqlite"
)

// walFixture is a WAL-mode database standing in for a live Thunderbird
// profile. Its writing connection stays OPEN for the fixture's lifetime, so
// the committed rows remain un-checkpointed in the -wal. That is the whole
// point: closing the writer would checkpoint the WAL away and the resulting
// database would be readable even by the broken immutable=1 path.
type walFixture struct {
	path string
	db   *sql.DB
	conn *sql.Conn
	tx   *sql.Tx
}

// newWALFixture creates the database with three committed rows in the WAL.
func newWALFixture(dir string) *walFixture {
	path := filepath.Join(dir, "fixture.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	Expect(err).NotTo(HaveOccurred())
	db.SetMaxOpenConns(1) // one connection, so lock() acts on the writer itself

	var jm string
	Expect(db.QueryRow("PRAGMA journal_mode=WAL").Scan(&jm)).To(Succeed())
	Expect(jm).To(Equal("wal"))
	_, err = db.Exec("CREATE TABLE t(x TEXT); INSERT INTO t VALUES('a'),('b'),('c')")
	Expect(err).NotTo(HaveOccurred())

	// Guard the premise: if the -wal is missing, the rows were checkpointed
	// and every test built on this fixture is silently weaker than it looks.
	_, err = os.Stat(path + "-wal")
	Expect(err).NotTo(HaveOccurred(), "fixture must leave rows un-checkpointed in the -wal")

	return &walFixture{path: path, db: db}
}

// lock makes the fixture reject readers, the way a running Thunderbird does:
// an exclusive lock plus an open, uncommitted write on the same connection.
func (f *walFixture) lock() {
	ctx := context.Background()
	conn, err := f.db.Conn(ctx)
	Expect(err).NotTo(HaveOccurred())
	_, err = conn.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE")
	Expect(err).NotTo(HaveOccurred())
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.ExecContext(ctx, "INSERT INTO t VALUES('uncommitted')")
	Expect(err).NotTo(HaveOccurred())
	f.conn, f.tx = conn, tx
}

func (f *walFixture) close() {
	if f.tx != nil {
		_ = f.tx.Rollback()
	}
	if f.conn != nil {
		_ = f.conn.Close()
	}
	_ = f.db.Close()
}

func countVia(s *dbSource) (int, error) {
	rows, err := s.Query("SELECT count(*) FROM t")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if !rows.Next() {
		return 0, rows.Err()
	}
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}

var _ = Describe("dbSource", func() {
	var dir string
	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("reads an uncontended database", func() {
		// A plain reader coexists with an active WAL writer; only an
		// exclusive lock causes contention.
		f := newWALFixture(dir)
		defer f.close()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		n, err := countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))
	})

	It("reports a file that is not a database", func() {
		bad := filepath.Join(dir, "bad.sqlite")
		Expect(os.WriteFile(bad, []byte("this is not a database"), 0o600)).To(Succeed())

		_, err := openDBSource(bad, "fixture")
		Expect(err).To(HaveOccurred())
	})

	It("is nil-safe on Close", func() {
		var s *dbSource
		Expect(s.Close()).To(Succeed())
	})
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestThunderbird 2>&1 | tail -20`
Expected: compile failure — `undefined: openDBSource`, `undefined: dbSource`.

- [ ] **Step 3: Write minimal implementation**

Create `dbsource.go`:

```go
package main

import (
	"database/sql"
	"errors"
	"sync"
	"time"
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
```

Add the non-blank import for the error type — the import block becomes:

```go
import (
	"database/sql"
	"errors"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS — the three new specs plus every pre-existing spec.

- [ ] **Step 5: Commit**

```bash
git add dbsource.go dbsource_test.go
git commit -m "feat: add dbSource wrapper for profile SQLite files"
```

---

### Task 2: Snapshot fallback on contention

The heart of the fix. A locked database is read through a copy that includes the WAL.

**Files:**
- Modify: `dbsource.go`
- Modify: `dbsource_test.go`

**Interfaces:**
- Consumes: `dbSource`, `openDBSource`, `isBusy`, `sqliteCodeIn`, `walFixture`, `newWALFixture`, `countVia` from Task 1.
- Produces:
  - `func (s *dbSource) refreshSnapshot() error`
  - `func (s *dbSource) makeSnapshot(mod time.Time) error`
  - `func copyFile(src, dst string) error`
  - field `copyFile func(src, dst string) error` on `dbSource` (test seam, defaults to the package-level `copyFile`)

- [ ] **Step 1: Write the failing test**

Append to `dbsource_test.go`, and add `"time"` to its imports:

```go
var _ = Describe("dbSource under contention", func() {
	var dir string
	var f *walFixture
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		f = newWALFixture(dir)
	})
	AfterEach(func() { f.close() })

	It("falls back to a snapshot when the live file is locked", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		n, err := countVia(s)
		Expect(err).NotTo(HaveOccurred())
		// 3 proves two things at once: the read survived the lock, and the
		// copy included the -wal (all three rows live there un-checkpointed).
		// The uncommitted 4th row must not appear.
		Expect(n).To(Equal(3))
		Expect(s.snap).NotTo(BeNil())
	})

	It("cannot read the locked file live", func() {
		f.lock()

		// Guards the premise of the test above: if this ever stops returning
		// BUSY, the fallback spec would pass without exercising the fallback.
		live, err := sql.Open("sqlite", "file:"+f.path+"?mode=ro&_pragma=busy_timeout(200)")
		Expect(err).NotTo(HaveOccurred())
		defer live.Close()
		var n int
		err = live.QueryRow("SELECT count(*) FROM t").Scan(&n)
		Expect(err).To(HaveOccurred())
		Expect(isBusy(err)).To(BeTrue())
	})

	It("does not snapshot when the query itself is bad", func() {
		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		_, err = s.Query("SELECT * FROM no_such_table")
		Expect(err).To(HaveOccurred())
		Expect(s.snap).To(BeNil()) // a real bug must not be masked by a copy
	})

	It("removes the snapshot directory on Close", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		_, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())

		snapDir := s.snapDir
		Expect(snapDir).NotTo(BeEmpty())
		Expect(s.Close()).To(Succeed())
		_, statErr := os.Stat(snapDir)
		Expect(os.IsNotExist(statErr)).To(BeTrue())
	})
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... 2>&1 | tail -30`
Expected: the contention specs FAIL with `database is locked (5) (SQLITE_BUSY)`, and compile errors for the unknown fields `s.snap` / `s.snapDir` if they are not yet referenced.

- [ ] **Step 3: Write minimal implementation**

In `dbsource.go`, add `"fmt"`, `"io"`, `"os"`, `"path/filepath"` to the imports, add the seam field to the struct, and set it in `openDBSource`:

```go
type dbSource struct {
	path string
	name string

	// copyFile is a seam so tests can simulate a torn copy. Defaults to the
	// package-level copyFile.
	copyFile func(src, dst string) error

	mu      sync.Mutex
	live    *sql.DB
	snap    *sql.DB
	snapDir string
	snapAt  time.Time
	srcMod  time.Time
}
```

In `openDBSource`, replace the final return with:

```go
	return &dbSource{path: path, name: name, live: live, copyFile: copyFile}, nil
```

Replace `Query` with the full ladder:

```go
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
```

Add the snapshot machinery:

```go
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
```

Extend `Close` to tear down the snapshot:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS. The fallback spec proves both halves at once — the read survives the lock, and because the fixture's writer is still open, all three rows are un-checkpointed WAL content, so `rows=3` also proves the copy included the `-wal`.

- [ ] **Step 5: Commit**

```bash
git add dbsource.go dbsource_test.go
git commit -m "feat: fall back to a WAL-inclusive snapshot when the profile db is locked"
```

---

### Task 3: TTL and mtime refresh policy

Bounds the copying so an active indexer cannot cause a copy per call.

**Files:**
- Modify: `dbsource_test.go`

**Interfaces:**
- Consumes: everything from Task 2. No production changes — `refreshSnapshot` already implements the policy; this task proves it and pins it against regression.
- Produces: no new symbols.

- [ ] **Step 1: Write the failing test**

Append to `dbsource_test.go`:

```go
var _ = Describe("dbSource snapshot refresh policy", func() {
	var dir string
	var f *walFixture
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		f = newWALFixture(dir)
	})
	AfterEach(func() { f.close() })

	It("reuses a warm snapshot without copying again", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		var copies int
		real := s.copyFile
		s.copyFile = func(src, dst string) error { copies++; return real(src, dst) }

		_, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(copies).To(Equal(2)) // main + -wal, one copy each

		firstAt := s.snapAt
		for i := 0; i < 5; i++ {
			_, err = countVia(s)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(copies).To(Equal(2))       // still warm, nothing re-copied
		Expect(s.snapAt).To(Equal(firstAt)) // and the window was not restarted
	})

	It("keeps the existing copy when the TTL lapses but the source has not changed", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		_, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())

		var copies int
		real := s.copyFile
		s.copyFile = func(src, dst string) error { copies++; return real(src, dst) }

		s.snapAt = time.Now().Add(-2 * snapshotTTL) // force the TTL to have lapsed

		n, err := countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))
		Expect(copies).To(Equal(0)) // mtime unchanged, so no new copy
	})

	It("re-copies once the source has changed", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		_, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())

		var copies int
		real := s.copyFile
		s.copyFile = func(src, dst string) error { copies++; return real(src, dst) }

		s.snapAt = time.Now().Add(-2 * snapshotTTL)
		s.srcMod = s.srcMod.Add(-time.Hour) // pretend the source moved on

		_, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(copies).To(Equal(2))
	})
})
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS immediately — Task 2 already implemented this policy. If any spec fails, the bug is in `refreshSnapshot`, not the test; fix `refreshSnapshot`.

Note this is the one place the plan deviates from write-failing-test-first: the behavior was implemented in Task 2 because `Query` cannot be written without it. These specs pin it.

- [ ] **Step 3: Commit**

```bash
git add dbsource_test.go
git commit -m "test: pin snapshot TTL and mtime refresh policy"
```

---

### Task 4: Torn-copy retry and actionable snapshot errors

A copy taken mid-write can be unusable. Retry once, then fail with something the user can act on.

**Files:**
- Modify: `dbsource.go`
- Modify: `dbsource_test.go`

**Interfaces:**
- Consumes: everything from Task 2.
- Produces: `func isTorn(err error) bool`

- [ ] **Step 1: Write the failing test**

Append to `dbsource_test.go`:

```go
var _ = Describe("dbSource snapshot failures", func() {
	var dir string
	var f *walFixture
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		f = newWALFixture(dir)
	})
	AfterEach(func() { f.close() })

	It("re-copies once when the first snapshot came out torn", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		// Simulate a copy taken mid-write: the first attempt writes garbage,
		// every later attempt copies for real.
		var attempts int
		real := s.copyFile
		s.copyFile = func(src, dst string) error {
			attempts++
			if attempts <= 2 { // the main file and its -wal, first pass
				return os.WriteFile(dst, []byte("torn garbage"), 0o600)
			}
			return real(src, dst)
		}

		n, err := countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))
		Expect(attempts).To(Equal(4)) // one torn pass, one good pass
	})

	It("gives an actionable error when the snapshot cannot be written", func() {
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		s.copyFile = func(src, dst string) error {
			return &os.PathError{Op: "write", Path: dst, Err: errors.New("no space left on device")}
		}

		_, err = countVia(s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Thunderbird is holding"))
		Expect(err.Error()).To(ContainSubstring("no space left on device"))
		Expect(err.Error()).To(ContainSubstring("close Thunderbird"))
		Expect(err.Error()).NotTo(ContainSubstring("database is locked"))
	})
})
```

Add `"errors"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... 2>&1 | tail -30`
Expected: the torn-copy spec FAILS with `file is not a database` or `no such table: t` — there is no retry yet. The actionable-error spec should already pass.

- [ ] **Step 3: Write minimal implementation**

In `dbsource.go`, add `isTorn` beside `isBusy`:

```go
// isTorn reports whether err indicates the snapshot was copied while
// Thunderbird was mid-write, leaving an unusable file.
func isTorn(err error) bool {
	return sqliteCodeIn(err,
		11, // SQLITE_CORRUPT
		26, // SQLITE_NOTADB
		1,  // SQLITE_ERROR - a torn header reads as "no such table"
	)
}
```

Replace the tail of `Query` (everything after the `isBusy` guard) with:

```go
	if err := s.refreshSnapshot(); err != nil {
		return nil, err
	}
	rows, err = s.snap.Query(q, args...)
	if err == nil || !isTorn(err) {
		return rows, err
	}
	// The copy was taken mid-write. Take it again; a second failure is real.
	if err := s.makeSnapshotNow(); err != nil {
		return nil, err
	}
	return s.snap.Query(q, args...)
}

// makeSnapshotNow forces a fresh copy, ignoring the mtime shortcut.
func (s *dbSource) makeSnapshotNow() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return s.snapshotErr(err)
	}
	return s.makeSnapshot(st.ModTime())
}
```

Note `isTorn` includes `SQLITE_ERROR` (1) because a truncated header surfaces as `no such table`, not as a corruption code. That is only consulted for a query against a *freshly made snapshot*, never against the live file, so it cannot mask a genuine schema error from the caller.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add dbsource.go dbsource_test.go
git commit -m "feat: retry a torn snapshot once and report copy failures actionably"
```

---

### Task 5: Wire gloda, contacts and calendar; delete `immutable=1`

Switches all three sources onto `dbSource` and removes the unsafe DSNs.

**Files:**
- Modify: `gloda.go:14`, `gloda.go:26-33`, `gloda.go:35-40`, `gloda.go:102`
- Modify: `contacts.go:11`, `contacts.go:21-27`, `contacts.go:29-34`
- Modify: `calendar.go:12-15`, `calendar.go:29-35`, `calendar.go:37-42`
- Modify: `contacts_test.go`, `calendar_test.go` (only if a compile error appears)

**Interfaces:**
- Consumes: `openDBSource(path, name string) (*dbSource, error)`, `(*dbSource).Query`, `(*dbSource).Close` from Tasks 1–4.
- Produces: `Gloda.db`, `Contacts.db`, `Calendar.db` are all `*dbSource`. Constructor signatures are unchanged: `openGlodaAt(path string) (*Gloda, error)`, `openContactsAt(path string) (*Contacts, error)`, `openCalendarAt(path string, names map[string]string) (*Calendar, error)`.

- [ ] **Step 1: Write the failing test**

Append to `dbsource_test.go` — a regression test proving the three sources survive a locked profile:

```go
// seededLiveDB writes seedFile into a WAL database and returns it in the state
// a running Thunderbird leaves its profile: the writing connection still open
// (so the seeded rows are un-checkpointed WAL content), holding an exclusive
// lock and an uncommitted write. Caller must call the returned release func.
//
// The connection must stay open. Closing it checkpoints the WAL into the main
// file, and a checkpointed database reads fine through immutable=1 - which
// would make this test pass against the very bug it exists to catch.
func seededLiveDB(dir, name, seedFile, blockingWrite string) (string, func()) {
	ctx := context.Background()
	path := filepath.Join(dir, name)

	seed, err := os.ReadFile(seedFile)
	Expect(err).NotTo(HaveOccurred())

	db, err := sql.Open("sqlite", "file:"+path)
	Expect(err).NotTo(HaveOccurred())
	db.SetMaxOpenConns(1)

	var jm string
	Expect(db.QueryRow("PRAGMA journal_mode=WAL").Scan(&jm)).To(Succeed())
	Expect(jm).To(Equal("wal"))
	_, err = db.Exec(string(seed))
	Expect(err).NotTo(HaveOccurred())

	_, err = os.Stat(path + "-wal")
	Expect(err).NotTo(HaveOccurred(), "seed must stay un-checkpointed in the -wal")

	conn, err := db.Conn(ctx)
	Expect(err).NotTo(HaveOccurred())
	_, err = conn.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE")
	Expect(err).NotTo(HaveOccurred())
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.ExecContext(ctx, blockingWrite)
	Expect(err).NotTo(HaveOccurred())

	return path, func() {
		_ = tx.Rollback()
		_ = conn.Close()
		_ = db.Close()
	}
}

var _ = Describe("profile sources under a locked profile", func() {
	It("serves contacts while the address book is locked", func() {
		path, release := seededLiveDB(GinkgoT().TempDir(), "abook.sqlite",
			"testdata/abook_seed.sql", "DELETE FROM properties")
		defer release()

		c, err := openContactsAt(path)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()

		res, err := c.Search("bob", 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(HaveLen(1))
		Expect(res[0].Email).To(Equal("bob@example.com"))
	})

	It("serves calendar events while the calendar is locked", func() {
		path, release := seededLiveDB(GinkgoT().TempDir(), "local.sqlite",
			"testdata/calendar_seed.sql", "DELETE FROM cal_events")
		defer release()

		cal, err := openCalendarAt(path, map[string]string{})
		Expect(err).NotTo(HaveOccurred())
		defer cal.Close()

		// Widest possible window, so this asserts "reads succeed under a lock"
		// rather than re-testing the date filtering calendar_test.go covers.
		evs, err := cal.Events(time.UnixMicro(0), time.Now().AddDate(100, 0, 0), 50)
		Expect(err).NotTo(HaveOccurred())
		Expect(evs).NotTo(BeEmpty())
	})
})
```

If `calendar_seed.sql` seeds events outside that window, or the table names differ, adjust the assertion to match the fixture rather than editing the fixture. Check with: `grep -n "INSERT INTO" testdata/calendar_seed.sql`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... 2>&1 | tail -30`
Expected: **FAIL** for both new specs with `no such table: properties` / `no such table: cal_events`. This is the bug itself: `immutable=1` ignores the `-wal`, where the seeded schema and rows live, so it reports the tables as absent rather than erroring on the lock.

If either spec *passes* at this step, stop — the fixture checkpointed its WAL and the test is not exercising the bug. Do not proceed to Step 3 until both fail for the stated reason.

- [ ] **Step 3: Write the implementation**

`gloda.go` — change the struct, the constructor, and the query call:

```go
type Gloda struct{ db *dbSource }
```

```go
func openGlodaAt(path string) (*Gloda, error) {
	src, err := openDBSource(path, "gloda")
	if err != nil {
		return nil, err
	}
	return &Gloda{db: src}, nil
}
```

`Close` and `queryRows` need no edit — `dbSource` exposes the same `Close()` and `Query()` shape. Delete the now-stale comment on line 27 (`// mode=ro + a busy timeout ...`), since the DSN moved into `dbSource`.

`contacts.go`:

```go
type Contacts struct{ db *dbSource }
```

```go
func openContactsAt(path string) (*Contacts, error) {
	src, err := openDBSource(path, "contacts")
	if err != nil {
		return nil, err
	}
	return &Contacts{db: src}, nil
}
```

`calendar.go`:

```go
type Calendar struct {
	db    *dbSource
	names map[string]string // cal_id -> display name
}
```

```go
func openCalendarAt(path string, names map[string]string) (*Calendar, error) {
	src, err := openDBSource(path, "calendar")
	if err != nil {
		return nil, err
	}
	return &Calendar{db: src, names: names}, nil
}
```

Then fix the imports in the three files:

- `contacts.go` still needs `"database/sql"` for `sql.NullString` and `*sql.Rows`. Keep it.
- `calendar.go` still needs `"database/sql"` for `sql.NullString`. Keep it.
- `gloda.go` no longer references the `sql` package at all. Remove `"database/sql"`.
- The `_ "modernc.org/sqlite"` blank imports in all three files can go: `dbsource.go` imports the same package non-blank, and its `init` registers the `sqlite` driver for the whole binary. Removing them is optional; leaving them is harmless. Do not remove the blank import from `secrets.go`, which opens `key4.db` on its own and is out of scope for this plan.

Run `gofmt -l .` after the edits and fix anything it lists.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS, including every pre-existing gloda, contacts and calendar spec.

- [ ] **Step 5: Verify no unsafe DSN survives**

Run: `grep -rn "immutable=1" --include="*.go" .`
Expected: no output. If anything matches outside a test asserting the old behavior, it is a miss — fix it.

Run: `go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add gloda.go contacts.go calendar.go dbsource_test.go
git commit -m "fix: read gloda, contacts and calendar through dbSource

Removes the immutable=1 DSNs, which on a WAL-mode profile hide everything
written since the last checkpoint - including the schema - and silently
return empty results instead of erroring."
```

---

### Task 6: Document the behavior

**Files:**
- Modify: `docs/configuration.md`
- Modify: `README.md` (only the troubleshooting section, if one exists; otherwise skip the README edit)

**Interfaces:**
- Consumes: the finished behavior from Task 5.
- Produces: no code symbols.

- [ ] **Step 1: Read the current docs to match their voice and structure**

Run: `sed -n 1,60p docs/configuration.md && grep -n "Troubleshoot\|troubleshoot" README.md docs/*.md`

- [ ] **Step 2: Add the section**

Add to `docs/configuration.md`, matching the surrounding heading level and tone:

```markdown
## Running alongside Thunderbird

The server reads the profile while Thunderbird is open. Reads normally go
straight to the profile's SQLite files; when Thunderbird holds a database
lock, the server transparently copies that database (and its write-ahead log)
to a temporary directory and reads the copy instead.

A copy is reused for up to 60 seconds and is only re-made when the underlying
file has actually changed, so an actively indexing Thunderbird does not cause
a copy per request. Copies live in `$TMPDIR` and are deleted when the server
exits. If a database is locked and the copy cannot be written — most often
because `$TMPDIR` is full — the request fails with an error naming the cause
rather than returning incomplete results.
```

- [ ] **Step 3: Verify the docs build**

Run: `mkdocs build --strict 2>&1 | tail -5`
Expected: build succeeds. If `mkdocs` is not installed, skip this step rather than installing it.

- [ ] **Step 4: Commit**

```bash
git add docs/configuration.md
git commit -m "docs: explain snapshot fallback when Thunderbird holds a lock"
```

---

## Final verification

- [ ] Run `go test ./...` — all specs pass.
- [ ] Run `go vet ./...` — clean.
- [ ] Run `gofmt -l .` — no output.
- [ ] Run `grep -rn "immutable=1" --include="*.go" .` — no matches.
- [ ] Confirm `git log --oneline` shows one commit per task.
