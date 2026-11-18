package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ManifestStore.PrepareLatest", func() {
	startedAt := time.Date(2026, 10, 8, 10, 46, 12, 0, time.UTC)

	It("creates a fresh real latest directory with metadata", func() {
		store := NewManifestStore(GinkgoT().TempDir())

		directory, release, err := store.PrepareLatest("run-a", startedAt)
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)
		Expect(directory).Should(Equal(filepath.Join(store.Root, "latest")))

		info, err := os.Lstat(directory)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(info.IsDir()).Should(BeTrue())
		Expect(info.Mode() & os.ModeSymlink).Should(BeZero())

		metadata, err := readAggregateMetadata(directory)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(metadata.RunID).Should(Equal("run-a"))
		Expect(metadata.StartedAt.Equal(startedAt)).Should(BeTrue())
	})

	It("rotates the previous latest run to its metadata-derived name", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		first, firstRelease, err := store.PrepareLatest("run-a", startedAt)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(first, "report.md"), []byte("first run"), 0o640)).Should(Succeed())
		firstRelease()

		secondStartedAt := startedAt.Add(time.Minute)
		second, secondRelease, err := store.PrepareLatest("run-b", secondStartedAt)
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(secondRelease)
		Expect(second).Should(Equal(filepath.Join(store.Root, "latest")))

		entries, err := os.ReadDir(second)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(entries).Should(HaveLen(1))
		Expect(entries[0].Name()).Should(Equal(aggregateMetadataName))

		rotated := filepath.Join(store.Root, ArtifactDirectoryName("multi", startedAt, "run-a"))
		Expect(os.ReadFile(filepath.Join(rotated, "report.md"))).Should(Equal([]byte("first run")))
		metadata, err := readAggregateMetadata(second)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(metadata.RunID).Should(Equal("run-b"))
	})

	It("removes a legacy latest symlink without touching its target", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		targetName := ArtifactDirectoryName("multi", startedAt, "run-target")
		target := filepath.Join(store.Root, targetName)
		Expect(os.MkdirAll(target, 0o750)).Should(Succeed())
		Expect(os.WriteFile(filepath.Join(target, "report.md"), []byte("target run"), 0o640)).Should(Succeed())
		Expect(os.Symlink(targetName, filepath.Join(store.Root, "latest"))).Should(Succeed())

		directory, release, err := store.PrepareLatest("run-a", startedAt.Add(time.Minute))
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)
		Expect(directory).Should(Equal(filepath.Join(store.Root, "latest")))
		info, err := os.Lstat(directory)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(info.Mode() & os.ModeSymlink).Should(BeZero())
		Expect(os.ReadFile(filepath.Join(target, "report.md"))).Should(Equal([]byte("target run")))
	})

	It("replaces an empty latest directory", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		Expect(os.Mkdir(filepath.Join(store.Root, "latest"), 0o750)).Should(Succeed())

		directory, release, err := store.PrepareLatest("run-a", startedAt)
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)
		metadata, err := readAggregateMetadata(directory)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(metadata.RunID).Should(Equal("run-a"))
	})

	It("rotates a corrupt latest directory to a legacy name preserving its files", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		latest := filepath.Join(store.Root, "latest")
		Expect(os.Mkdir(latest, 0o750)).Should(Succeed())
		Expect(os.WriteFile(filepath.Join(latest, "report.md"), []byte("corrupt run"), 0o640)).Should(Succeed())
		Expect(os.WriteFile(filepath.Join(latest, aggregateMetadataName), []byte("{not json"), 0o640)).Should(Succeed())

		directory, release, err := store.PrepareLatest("run-a", startedAt)
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)
		Expect(directory).Should(Equal(latest))
		rotated, err := filepath.Glob(filepath.Join(store.Root, "multi-run-*-legacy-*"))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(rotated).Should(HaveLen(1))
		Expect(os.ReadFile(filepath.Join(rotated[0], "report.md"))).Should(Equal([]byte("corrupt run")))
	})

	It("fails without touching latest when the rotation target exists", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		rotatedName := ArtifactDirectoryName("multi", startedAt, "run-a")
		Expect(os.MkdirAll(filepath.Join(store.Root, rotatedName), 0o750)).Should(Succeed())
		latest := filepath.Join(store.Root, "latest")
		Expect(os.Mkdir(latest, 0o750)).Should(Succeed())
		Expect(store.writeJSON(filepath.Join(latest, aggregateMetadataName), aggregateMetadata{RunID: "run-a", StartedAt: startedAt})).Should(Succeed())

		_, release, err := store.PrepareLatest("run-b", startedAt.Add(time.Minute))
		Expect(release).Should(BeNil())
		Expect(err).Should(MatchError(ContainSubstring("already exists")))
		metadata, err := readAggregateMetadata(latest)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(metadata.RunID).Should(Equal("run-a"))
	})

	It("rejects a concurrent run while latest is held", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		_, release, err := store.PrepareLatest("run-a", startedAt)
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(release)

		_, secondRelease, err := store.PrepareLatest("run-b", startedAt.Add(time.Minute))
		Expect(secondRelease).Should(BeNil())
		Expect(errors.Is(err, ErrLatestLocked)).Should(BeTrue())
	})

	DescribeTable("rejects invalid input",
		func(runID string, startedAt time.Time) {
			store := NewManifestStore(GinkgoT().TempDir())

			_, release, err := store.PrepareLatest(runID, startedAt)
			Expect(release).Should(BeNil())
			Expect(err).Should(HaveOccurred())
			_, statErr := os.Lstat(filepath.Join(store.Root, "latest"))
			Expect(statErr).Should(MatchError(os.ErrNotExist))
		},
		Entry("empty run ID", "", startedAt),
		Entry("zero started at", "run-a", time.Time{}),
	)
})
