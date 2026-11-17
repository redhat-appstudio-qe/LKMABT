package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("runLog", func() {
	When("output arrives before Attach", func() {
		It("should buffer it and flush on attach", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "cluster", "run.log")
			log := newRunLog()
			fmt.Fprintf(log, "early\n")
			Expect(log.Attach(path)).Should(Succeed())
			fmt.Fprintf(log, "late\n")
			Expect(log.Close()).Should(Succeed())
			Expect(readFile(path)).Should(Equal("early\nlate\n"))
		})
	})

	When("the log is attached up front", func() {
		It("should write directly", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "cluster", "run.log")
			log, err := newAttachedRunLog(path)
			Expect(err).ShouldNot(HaveOccurred())
			fmt.Fprintf(log, "direct\n")
			Expect(log.Close()).Should(Succeed())
			Expect(readFile(path)).Should(Equal("direct\n"))
			dirInfo, err := os.Stat(filepath.Dir(path))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(dirInfo.Mode().Perm()).Should(Equal(os.FileMode(0o750)))
			fileInfo, err := os.Stat(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(fileInfo.Mode().Perm()).Should(Equal(os.FileMode(0o640)))
		})
	})

	When("Attach is called twice", func() {
		It("should keep the first target", func() {
			dir := GinkgoT().TempDir()
			pathA := filepath.Join(dir, "a", "run.log")
			pathB := filepath.Join(dir, "b", "run.log")
			log := newRunLog()
			Expect(log.Attach(pathA)).Should(Succeed())
			Expect(log.Attach(pathB)).Should(Succeed())
			fmt.Fprintf(log, "x")
			Expect(log.Close()).Should(Succeed())
			Expect(readFile(pathA)).Should(Equal("x"))
			_, err := os.Stat(pathB)
			Expect(err).Should(HaveOccurred())
		})
	})

	When("no file was ever attached", func() {
		It("should close without error", func() {
			Expect(newRunLog().Close()).Should(Succeed())
		})
	})

	When("the parent directory cannot be created", func() {
		It("should report the failure", func() {
			dir := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(dir, "blocked"), nil, 0o640)).Should(Succeed())
			log := newRunLog()
			Expect(log.Attach(filepath.Join(dir, "blocked", "run.log"))).Should(MatchError(ContainSubstring("create run log directory")))
			count, err := fmt.Fprintf(log, "still buffered\n")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(count).Should(Equal(len("still buffered\n")))
		})
	})

	When("the flush writes fewer bytes than buffered", func() {
		It("should report the short write and retain the remainder", func() {
			log := newRunLog()
			fmt.Fprintf(log, "early\n")

			Expect(log.flushBuffer(shortWriter{limit: 2})).Should(MatchError(ContainSubstring("wrote 2 of 6 buffered bytes")))
			Expect(log.buffer.String()).Should(Equal("rly\n"))
		})
	})

	When("the flush fails", func() {
		It("should report the failure and keep every byte buffered", func() {
			log := newRunLog()
			fmt.Fprintf(log, "early\n")

			Expect(log.flushBuffer(failingWriter{})).Should(MatchError(ContainSubstring("disk full")))
			Expect(log.buffer.String()).Should(Equal("early\n"))
		})
	})

	When("the attached file rejects the buffered flush", func() {
		It("should return the flush failure and keep the unwritten bytes buffered", func() {
			if _, statErr := os.Stat("/dev/full"); statErr != nil {
				Skip("/dev/full is not available on this platform")
			}
			log := newRunLog()
			_, err := log.Write([]byte("rly\n"))
			Expect(err).ShouldNot(HaveOccurred())

			Expect(log.Attach("/dev/full")).Should(MatchError(ContainSubstring("flush run log /dev/full")))
			Expect(log.file).Should(BeNil())
			Expect(log.buffer.String()).Should(Equal("rly\n"))

			_, err = log.Write([]byte("more\n"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(log.buffer.String()).Should(Equal("rly\nmore\n"))
		})
	})
})

// shortWriter accepts only limit bytes per Write and reports no error.
type shortWriter struct{ limit int }

func (w shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

// failingWriter accepts nothing and reports a write error.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// readFile returns the file content or fails the spec on error.
func readFile(path string) string {
	content, err := os.ReadFile(path)
	Expect(err).ShouldNot(HaveOccurred())
	return string(content)
}
