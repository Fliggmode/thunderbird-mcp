package main

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("LoadApp (profile only)", func() {
	It("loads config and degrades gracefully when data sources are absent", func() {
		// testdata dir has prefs.js but no gloda/abook/calendar/key4 dbs.
		a, err := loadAppFromConfig(&Config{ProfileDir: "testdata", Accounts: nil, SMTP: map[string]SMTPServer{}}, &Credentials{}, false, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Gloda).To(BeNil())    // no global-messages-db.sqlite in testdata
		Expect(a.Contacts).To(BeNil()) // no abook.sqlite
		Expect(a.IMAP).NotTo(BeNil())  // always constructed
	})

	It("degrades to an empty credential store instead of failing when credentials cannot load", func() {
		// A credential-load failure only disables IMAP/SMTP; the app must still come up
		// with a working IMAP/Sender that fail cleanly (no stored password) at call time.
		a, err := loadAppFromConfig(
			&Config{ProfileDir: "testdata", Accounts: nil, SMTP: map[string]SMTPServer{}},
			&Credentials{}, false, false,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(a).NotTo(BeNil())
		Expect(a.IMAP).NotTo(BeNil())
		Expect(a.Sender).NotTo(BeNil())
	})
})

var _ = Describe("App.Close", func() {
	It("is nil-safe at both levels", func() {
		var a *App
		Expect(a.Close()).To(Succeed()) // the app itself

		// ...and each field: a profile without gloda/abook/calendar leaves
		// those nil, because Open* returns (nil, nil) when the file is absent.
		Expect((&App{}).Close()).To(Succeed())
	})

	It("removes the snapshot directory a locked source created", func() {
		// This is the only thing standing between a server exit and a full copy
		// of the database left behind in $TMPDIR, which for gloda is routinely
		// hundreds of MB on a RAM-backed /tmp.
		path, release := seededLiveDB(GinkgoT().TempDir(), "global-messages-db.sqlite",
			"testdata/gloda_seed.sql", "DELETE FROM messagesText_content")
		defer release()

		g, err := openGlodaAt(path)
		Expect(err).NotTo(HaveOccurred())

		// The lock forces the fallback, which is what creates the temp copy.
		_, err = g.Search(SearchQuery{Text: "report", Limit: 10})
		Expect(err).NotTo(HaveOccurred())

		snapDir := g.db.snapDir
		Expect(snapDir).NotTo(BeEmpty(), "the locked source must have forced a snapshot")

		// Contacts and Calendar left nil on purpose: Close has to walk past
		// them to reach Gloda, exactly as it does for a partial profile.
		a := &App{Gloda: g}
		Expect(a.Close()).To(Succeed())

		_, statErr := os.Stat(snapDir)
		Expect(os.IsNotExist(statErr)).To(BeTrue(), "App.Close must delete the snapshot directory")
	})
})

var _ = Describe("folderIsLocal", func() {
	It("treats mailbox:// as local and imap:// as remote", func() {
		a := &App{Config: &Config{Accounts: []Account{
			{Type: "imap", Hostname: "imap.example.com"},
			{Type: "none", Hostname: "Local Folders"},
		}}}
		Expect(a.folderIsLocal("mailbox://nobody@Local%20Folders/INBOX")).To(BeTrue())
		Expect(a.folderIsLocal("imap://alice@imap.example.com/INBOX")).To(BeFalse())
	})
})
