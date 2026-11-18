package filelock_test

import (
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/filelock"
)

var _ = Describe("TryAcquire", func() {
	When("no other process holds the lock", func() {
		It("should acquire, reject a second holder, and allow reacquisition after release", func() {
			path := filepath.Join(GinkgoT().TempDir(), "tenant.lock")

			release, err := filelock.TryAcquire(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(release).ShouldNot(BeNil())

			_, err = filelock.TryAcquire(path)
			Expect(errors.Is(err, filelock.ErrLocked)).Should(BeTrue(), "second acquisition should report ErrLocked: %v", err)
			Expect(err).Should(MatchError(ContainSubstring(path)))

			release()
			again, err := filelock.TryAcquire(path)
			Expect(err).ShouldNot(HaveOccurred())
			again()
		})

		It("should tolerate a repeated release", func() {
			path := filepath.Join(GinkgoT().TempDir(), "tenant.lock")

			release, err := filelock.TryAcquire(path)
			Expect(err).ShouldNot(HaveOccurred())

			Expect(func() { release() }).ShouldNot(Panic())
			Expect(func() { release() }).ShouldNot(Panic())
		})
	})

	When("the lock path is unusable", func() {
		It("should reject a missing parent directory", func() {
			path := filepath.Join(GinkgoT().TempDir(), "missing", "tenant.lock")

			_, err := filelock.TryAcquire(path)
			Expect(err).Should(HaveOccurred())
			Expect(errors.Is(err, filelock.ErrLocked)).Should(BeFalse())
		})

		It("should reject a directory", func() {
			path := filepath.Join(GinkgoT().TempDir(), "tenant.lock")
			Expect(os.Mkdir(path, 0o700)).Should(Succeed())

			_, err := filelock.TryAcquire(path)
			Expect(err).Should(HaveOccurred())
			Expect(errors.Is(err, filelock.ErrLocked)).Should(BeFalse())
		})

		It("should reject a symlink without following it", func() {
			dir := GinkgoT().TempDir()
			target := filepath.Join(dir, "target.lock")
			Expect(os.WriteFile(target, nil, 0o600)).Should(Succeed())
			path := filepath.Join(dir, "tenant.lock")
			Expect(os.Symlink(target, path)).Should(Succeed())

			_, err := filelock.TryAcquire(path)
			Expect(err).Should(HaveOccurred())
			Expect(errors.Is(err, filelock.ErrLocked)).Should(BeFalse())
		})
	})
})
