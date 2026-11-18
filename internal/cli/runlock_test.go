package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/config"
)

// tenantLockTestDirectory keeps every CLI spec's namespace locks out of the user cache.
var tenantLockTestDirectory string

var _ = BeforeSuite(func() {
	directory, err := os.MkdirTemp("", "lkmabt-tenant-locks-")
	Expect(err).ShouldNot(HaveOccurred())
	tenantLockTestDirectory = directory
	tenantLockDirectory = func() (string, error) { return filepath.Join(directory, "locks"), nil }
})

var _ = AfterSuite(func() {
	Expect(os.RemoveAll(tenantLockTestDirectory)).Should(Succeed())
})

var _ = Describe("tenant namespace locks", func() {
	It("should derive one path for equivalent server spellings and distinct paths per namespace", func() {
		first, err := tenantLockPath("https://api.example/", "tenant")
		Expect(err).ShouldNot(HaveOccurred())
		second, err := tenantLockPath("https://API.example:443", "tenant")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(second).Should(Equal(first))

		other, err := tenantLockPath("https://api.example", "tenant-other")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(other).ShouldNot(Equal(first))

		Expect(filepath.Base(first)).Should(HaveSuffix(".lock"))
		Expect(filepath.Base(first)).ShouldNot(ContainSubstring("tenant"))
	})

	It("should allow both providers' distinct namespaces to be held at once", func() {
		githubRelease, err := acquireTenantNamespaceLock("https://api.example", config.DefaultGitHubTenantNamespace, tenantLockHolder("run-github", time.Now()))
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(githubRelease)

		gitlabRelease, err := acquireTenantNamespaceLock("https://api.example", config.DefaultGitLabTenantNamespace, tenantLockHolder("run-gitlab", time.Now()))
		Expect(err).ShouldNot(HaveOccurred())
		gitlabRelease()
	})

	When("the namespace is already held", func() {
		It("should name the namespace, server, and holder", func() {
			release, err := acquireTenantNamespaceLock("https://api.example", "tenant", tenantLockHolder("run-1", time.Now()))
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(release)

			_, err = acquireTenantNamespaceLock("https://api.example/", "tenant", tenantLockHolder("run-2", time.Now()))
			Expect(err).Should(MatchError(And(
				ContainSubstring("tenant namespace tenant"),
				ContainSubstring("https://api.example"),
				ContainSubstring("run-1"),
				ContainSubstring("pid"),
			)))
		})

		It("should degrade when the holder record is unreadable", func() {
			release, err := acquireTenantNamespaceLock("https://api.example", "tenant", tenantLockHolder("run-1", time.Now()))
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(release)

			lockPath, err := tenantLockPath("https://api.example", "tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(os.WriteFile(lockPath, nil, 0o600)).Should(Succeed())

			_, err = acquireTenantNamespaceLock("https://api.example", "tenant", tenantLockHolder("run-2", time.Now()))
			Expect(err).Should(MatchError(And(
				ContainSubstring("tenant namespace tenant"),
				ContainSubstring("https://api.example"),
				ContainSubstring("locked by another LKMABT process"),
			)))
		})

		It("should allow reacquisition after release", func() {
			release, err := acquireTenantNamespaceLock("https://api.example", "tenant", tenantLockHolder("run-1", time.Now()))
			Expect(err).ShouldNot(HaveOccurred())
			release()

			again, err := acquireTenantNamespaceLock("https://api.example/", "tenant", tenantLockHolder("run-2", time.Now()))
			Expect(err).ShouldNot(HaveOccurred())
			again()
		})
	})

	It("should refuse a run before creating artifacts while the namespace is held", func() {
		request := config.Defaults()
		request.RunID = "run-held"
		request.Provider = config.ProviderGitHub
		request.StateDir = GinkgoT().TempDir()
		request.Kubeconfig = writeKubeconfig(GinkgoT().TempDir(), "c1")
		request.ClusterServer = "https://api.c1.example"
		request.TenantNamespace = "tenant-held"
		request.Credentials = config.Credentials{GitHubToken: "token"}

		release, err := acquireTenantNamespaceLock(request.ClusterServer, request.TenantNamespace, tenantLockHolder("run-other", time.Now()))
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)

		_, err = (LiveRunner{Out: io.Discard}).Run(context.Background(), request, newRunLog())
		Expect(err).Should(MatchError(ContainSubstring("in use by run run-other")))
		entries, readErr := os.ReadDir(request.StateDir)
		Expect(readErr).ShouldNot(HaveOccurred())
		Expect(entries).Should(BeEmpty())
	})

	It("should refuse cleanup while the namespace is held and release after deletion", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		request := cleanupTestRequest("run-1", store.Root)
		prompt := &cleanupTestPrompt{approved: true}

		release, err := acquireTenantNamespaceLock(request.ClusterServer, "tenant-run-1", tenantLockHolder("run-other", time.Now()))
		Expect(err).ShouldNot(HaveOccurred())

		err = cleanupRun(context.Background(), request, store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("in use by run run-other")))
		Expect(prompt.calls).Should(BeZero())
		release()

		Expect(cleanupRun(context.Background(), request, store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)).Should(Succeed())
		Expect(prompt.calls).Should(Equal(1))

		again, err := acquireTenantNamespaceLock(request.ClusterServer, "tenant-run-1", tenantLockHolder("run-next", time.Now()))
		Expect(err).ShouldNot(HaveOccurred())
		again()
	})
})
