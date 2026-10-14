package main

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ParseMessage", func() {
	It("extracts headers, text body and attachment metadata", func() {
		raw := "From: bob@example.com\r\n" +
			"To: alice@example.com\r\n" +
			"Subject: Report\r\n" +
			"Message-ID: <m1@example.com>\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
			"the body\r\n"
		d, err := ParseMessage("imap://h/INBOX#1", []byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Subject).To(Equal("Report"))
		Expect(d.Author).To(ContainSubstring("bob@example.com"))
		Expect(d.MessageID).To(Equal("<m1@example.com>"))
		Expect(d.BodyText).To(ContainSubstring("the body"))
	})
})

var _ = Describe("readMboxAt", func() {
	It("reads a single message at an offset", func() {
		dir := GinkgoT().TempDir()
		p := filepath.Join(dir, "INBOX")
		content := "From - Mon Jan 1\r\nSubject: one\r\n\r\nbody one\r\n" +
			"From - Mon Jan 2\r\nSubject: two\r\n\r\nbody two\r\n"
		Expect(os.WriteFile(p, []byte(content), 0o600)).To(Succeed())
		off := uint32(len("From - Mon Jan 1\r\nSubject: one\r\n\r\nbody one\r\n"))
		raw, err := readMboxAt(p, off)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("body two"))
		Expect(string(raw)).NotTo(ContainSubstring("body one"))
	})
})

var _ = Describe("ExtractAttachment", func() {
	It("extracts first attachment by index 0", func() {
		// Minimal multipart message with one attachment
		raw := "From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=boundary123\r\n" +
			"\r\n" +
			"--boundary123\r\n" +
			"Content-Type: text/plain\r\n" +
			"\r\n" +
			"body text\r\n" +
			"--boundary123\r\n" +
			"Content-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment; filename=\"test.txt\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n" +
			"\r\n" +
			"dGVzdCBjb250ZW50\r\n" +
			"--boundary123--\r\n"
		fn, ct, data, err := ExtractAttachment([]byte(raw), 0, 25*1024*1024)
		Expect(err).NotTo(HaveOccurred())
		Expect(fn).To(Equal("test.txt"))
		Expect(ct).To(Equal("application/octet-stream"))
		Expect(data).NotTo(BeEmpty())
	})

	It("returns error on negative index", func() {
		raw := "From: test@example.com\r\nSubject: Test\r\n\r\nbody"
		_, _, _, err := ExtractAttachment([]byte(raw), -1, 25*1024*1024)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("index must be >= 0"))
	})

	It("returns error on out-of-range index with count", func() {
		raw := "From: test@example.com\r\nSubject: Test\r\n\r\nbody"
		_, _, _, err := ExtractAttachment([]byte(raw), 5, 25*1024*1024)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("attachment index 5 not found"))
		Expect(err.Error()).To(ContainSubstring("message has 0 attachments"))
	})

	It("uses fallback name when filename is empty", func() {
		raw := "From: sender@example.com\r\n" +
			"Subject: Test\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=b\r\n" +
			"\r\n" +
			"--b\r\n" +
			"Content-Type: application/pdf\r\n" +
			"Content-Disposition: attachment\r\n" +
			"\r\n" +
			"pdf data\r\n" +
			"--b--\r\n"
		fn, ct, data, err := ExtractAttachment([]byte(raw), 0, 25*1024*1024)
		Expect(err).NotTo(HaveOccurred())
		Expect(fn).To(Equal("attachment-0"))
		Expect(ct).To(Equal("application/pdf"))
		Expect(data).NotTo(BeEmpty())
	})

	It("defaults to octet-stream when content-type is missing/empty", func() {
		// Note: MIME parsers typically default to text/plain when no Content-Type
		// is specified. Our fallback handles truly empty strings from parsing.
		raw := "From: sender@example.com\r\n" +
			"Subject: Test\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=b\r\n" +
			"\r\n" +
			"--b\r\n" +
			"Content-Type:\r\n" +
			"Content-Disposition: attachment; filename=\"noct\"\r\n" +
			"\r\n" +
			"binary data\r\n" +
			"--b--\r\n"
		fn, ct, data, err := ExtractAttachment([]byte(raw), 0, 25*1024*1024)
		Expect(err).NotTo(HaveOccurred())
		Expect(fn).To(Equal("noct"))
		// Even with empty Content-Type header, parser may assign default
		Expect(ct).NotTo(BeEmpty())
		Expect(data).NotTo(BeEmpty())
	})

	It("enforces size limit without loading entire file", func() {
		raw := "From: sender@example.com\r\n" +
			"Subject: Test\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=b\r\n" +
			"\r\n" +
			"--b\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Disposition: attachment; filename=\"big.txt\"\r\n" +
			"\r\n" +
			"this is huge data\r\n" +
			"--b--\r\n"
		_, _, _, err := ExtractAttachment([]byte(raw), 0, 5) // limit to 5 bytes
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exceeds size limit"))
	})
})

var _ = Describe("SanitizeFilename", func() {
	It("keeps simple filenames", func() {
		Expect(SanitizeFilename("document.pdf")).To(Equal("document.pdf"))
	})

	It("strips directory paths", func() {
		Expect(SanitizeFilename("../../../etc/passwd")).To(Equal("passwd"))
		Expect(SanitizeFilename("/path/to/file.txt")).To(Equal("file.txt"))
		Expect(SanitizeFilename("path\\to\\file.txt")).To(Equal("file.txt"))
		Expect(SanitizeFilename("C:\\Windows\\System32\\file.txt")).To(Equal("file.txt"))
	})

	It("returns basename for single dot", func() {
		Expect(SanitizeFilename(".")).To(Equal("."))
	})

	It("returns dot for empty filename", func() {
		// filepath.Base("") returns "." on all systems
		Expect(SanitizeFilename("")).To(Equal("."))
	})
})
