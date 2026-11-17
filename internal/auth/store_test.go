package auth

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Store", func() {
	var (
		store    *Store
		tempRoot string
	)
	BeforeEach(func() {
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
	})

	When("a credential is written", func() {
		It("should keep it private and destroy the directory", func() {
			directoryInfo, statErr := os.Stat(store.Directory())
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(directoryInfo.Mode().Perm() & 0o077).Should(BeZero())

			Expect(store.Write("github-token", "secret")).Should(Succeed())
			tokenPath := filepath.Join(store.Directory(), "github-token")
			tokenInfo, tokenErr := os.Stat(tokenPath)
			Expect(tokenErr).ShouldNot(HaveOccurred())
			Expect(tokenInfo.Mode().Perm() & 0o077).Should(BeZero())
			content, readErr := os.ReadFile(tokenPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("secret"))

			Expect(store.Destroy()).Should(Succeed())
			Expect(store.Directory()).Should(BeEmpty())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
			Expect(store.Destroy()).Should(Succeed())
		})
	})

	When("the store is not initialized", func() {
		It("should reject writes and destroy as a no-op", func() {
			var empty *Store
			Expect(empty.Write("github-token", "secret")).Should(MatchError("credential store is not initialized"))
			Expect(empty.Directory()).Should(BeEmpty())
			Expect(empty.Destroy()).Should(Succeed())
		})
	})
})
