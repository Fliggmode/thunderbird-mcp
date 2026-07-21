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
