package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

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

	It("writes each snapshot generation to its own path", func() {
		// A second copy must never be written over the file backing the first.
		// A caller can still be iterating *sql.Rows from the previous snapshot,
		// and truncating that inode under it yields torn pages, not just stale
		// data. Distinct paths per generation are what make that impossible.
		f.lock()

		s, err := openDBSource(f.path, "fixture")
		Expect(err).NotTo(HaveOccurred())
		defer s.Close()

		n, err := countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))
		first := s.snapSub
		Expect(first).NotTo(BeEmpty())

		// Expire the TTL and invalidate the mtime memo, so the next query is
		// forced to take a fresh copy rather than reuse the warm one.
		s.snapAt = time.Now().Add(-2 * snapshotTTL)
		s.srcMod = time.Time{}

		n, err = countVia(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))

		second := s.snapSub
		Expect(second).NotTo(Equal(first))
		Expect(filepath.Dir(second)).To(Equal(s.snapDir))
		// The old generation is gone, not overwritten: nothing was written
		// through the path the previous reader's descriptor points at.
		_, statErr := os.Stat(filepath.Join(first, filepath.Base(f.path)))
		Expect(os.IsNotExist(statErr)).To(BeTrue())
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
		Expect(copies).To(Equal(2))         // still warm, nothing re-copied
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
