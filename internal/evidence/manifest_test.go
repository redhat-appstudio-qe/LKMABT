package evidence

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("ManifestStore", func() {
	It("persists phases and latest", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		createdAt := time.Date(2026, 10, 5, 14, 23, 0, 0, time.FixedZone("IDT", 3*60*60))
		manifest := model.RunManifest{RunID: "run-1", Provider: "github", Phase: model.PhasePreflight, CreatedAt: createdAt, TargetClusterServer: "https://api.example", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		Expect(store.Create(&manifest)).To(Succeed())
		manifest, err := store.Load("run-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.ArtifactDirectory).To(Equal("github-run-14:23_5.10.2026_run-1"))
		Expect(store.Transition(&manifest, model.PhaseFixtureReady)).To(Succeed())
		loaded, err := store.Load("run-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Phase).To(Equal(model.PhaseFixtureReady))
		latest, err := store.Latest()
		Expect(err).NotTo(HaveOccurred())
		Expect(latest).To(Equal("run-1"))
		for _, name := range []string{"manifest.json", "status.json"} {
			_, err = os.Stat(filepath.Join(store.RunDirFor(loaded), name))
			Expect(err).NotTo(HaveOccurred())
		}
		latestInfo, err := os.Lstat(filepath.Join(store.Root, "latest"))
		Expect(err).NotTo(HaveOccurred())
		Expect(latestInfo.Mode() & os.ModeSymlink).NotTo(BeZero())
		latestTarget, err := os.Readlink(filepath.Join(store.Root, "latest"))
		Expect(err).NotTo(HaveOccurred())
		Expect(latestTarget).To(Equal(manifest.ArtifactDirectory))
		Expect(filepath.IsAbs(latestTarget)).To(BeFalse())
	})

	It("writes identity and redacts credentials", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		manifest := model.RunManifest{RunID: "run-2", Phase: model.PhasePreflight, PipelineRuns: []model.PipelineRunIdentity{{Namespace: "tenant", Name: "build", UID: types.UID("uid-1"), StartedAt: time.Now().UTC()}}, BuildOutputs: []model.BuildOutputEvidence{{CreatedOutputs: map[string]string{"password": "secret-value"}}}}
		Expect(store.Create(&manifest)).To(Succeed())
		data, err := os.ReadFile(filepath.Join(store.RunDirFor(manifest), "manifest.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).NotTo(ContainSubstring("do-not-write"))
		Expect(string(data)).To(ContainSubstring("uid-1"))
	})

	It("tracks the active run through failure and success while preserving prior artifacts", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		createdAt := time.Date(2026, 10, 5, 14, 24, 0, 0, time.UTC)
		first := model.RunManifest{RunID: "run-first", Provider: "gitlab", CreatedAt: createdAt, Phase: model.PhasePreflight}
		Expect(store.Create(&first)).To(Succeed())
		expectLatestRun(store, "run-first")
		firstArtifact := filepath.Join(store.RunDirFor(first), "failure-note.txt")
		Expect(os.WriteFile(firstArtifact, []byte("first run"), 0o640)).To(Succeed())
		latestArtifact, err := os.ReadFile(filepath.Join(store.Root, "latest", "failure-note.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(latestArtifact)).To(Equal("first run"))
		Expect(store.Transition(&first, model.PhaseFailed)).To(Succeed())
		expectLatestRun(store, "run-first")

		second := model.RunManifest{RunID: "run-second", Provider: "gitlab", CreatedAt: createdAt, Phase: model.PhasePreflight}
		Expect(store.Create(&second)).To(Succeed())
		expectLatestRun(store, "run-second")
		firstContents, err := os.ReadFile(firstArtifact)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(firstContents)).To(Equal("first run"))

		for _, phase := range []model.Phase{model.PhaseFixtureReady, model.PhaseTriggered, model.PhaseBuildsVerified, model.PhaseBuildOutputsVerified, model.PhaseCompleted} {
			Expect(store.Transition(&second, phase)).To(Succeed())
		}
		expectLatestRun(store, "run-second")
	})

	It("does not move latest back to an older run created later", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		newer := model.RunManifest{RunID: "run-newer", Provider: "github", CreatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Phase: model.PhasePreflight}
		Expect(store.Create(&newer)).To(Succeed())
		older := model.RunManifest{RunID: "run-older", Provider: "github", CreatedAt: newer.CreatedAt.Add(-time.Minute), Phase: model.PhasePreflight}
		Expect(store.Create(&older)).To(Succeed())
		expectLatestRun(store, "run-newer")
	})

	It("moves aside legacy latest directories before switching the pointer", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		first := model.RunManifest{RunID: "run-first", Provider: "github", CreatedAt: time.Date(2026, 10, 5, 14, 24, 0, 0, time.UTC), Phase: model.PhasePreflight}
		Expect(store.Create(&first)).To(Succeed())
		latestPath := filepath.Join(store.Root, "latest")
		Expect(os.Remove(latestPath)).To(Succeed())
		Expect(os.Mkdir(latestPath, 0o750)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(latestPath, "legacy-note.txt"), []byte("preserve"), 0o640)).To(Succeed())

		second := model.RunManifest{RunID: "run-second", Provider: "github", CreatedAt: first.CreatedAt.Add(time.Second), Phase: model.PhasePreflight}
		Expect(store.Create(&second)).To(Succeed())
		expectLatestRun(store, "run-second")
		info, err := os.Lstat(latestPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode() & os.ModeSymlink).NotTo(BeZero())
		legacyPaths, err := filepath.Glob(filepath.Join(store.Root, ".latest-legacy-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(legacyPaths).To(HaveLen(1))
		legacyContents, err := os.ReadFile(filepath.Join(legacyPaths[0], "legacy-note.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(legacyContents)).To(Equal("preserve"))
	})

	It("rejects artifact directories outside the state root without changing latest", func() {
		root := GinkgoT().TempDir()
		store := NewManifestStore(filepath.Join(root, "runs"))
		first := model.RunManifest{RunID: "run-first", Provider: "github", CreatedAt: time.Now().UTC(), Phase: model.PhasePreflight}
		Expect(store.Create(&first)).To(Succeed())

		unsafe := model.RunManifest{RunID: "run-unsafe", ArtifactDirectory: "../outside", Phase: model.PhasePreflight}
		Expect(store.Create(&unsafe)).To(MatchError(ContainSubstring("single directory name")))
		Expect(store.SetLatest(unsafe)).To(MatchError(ContainSubstring("single directory name")))
		expectLatestRun(store, "run-first")
		_, err := os.Stat(filepath.Join(root, "outside"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("rejects invalid phase transition", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		manifest := model.RunManifest{RunID: "run-3", Phase: model.PhasePreflight}
		Expect(store.Create(&manifest)).To(Succeed())
		Expect(store.Transition(&manifest, model.PhaseBuildOutputsVerified)).To(HaveOccurred())
	})
})

var _ = Describe("PointLatestAt", func() {
	When("the aggregate directory exists", func() {
		It("should point latest at it with a relative link", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			name := ArtifactDirectoryName("multi", time.Date(2026, 10, 8, 10, 46, 0, 0, time.UTC), "run-a")
			Expect(name).Should(Equal("multi-run-10:46_8.10.2026_run-a"))
			Expect(os.MkdirAll(filepath.Join(store.Root, name), 0o750)).Should(Succeed())

			Expect(store.PointLatestAt(name)).Should(Succeed())

			latestPath := filepath.Join(store.Root, "latest")
			info, err := os.Lstat(latestPath)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(info.Mode() & os.ModeSymlink).ShouldNot(BeZero())
			target, err := os.Readlink(latestPath)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(target).Should(Equal(name))
			Expect(filepath.IsAbs(target)).Should(BeFalse())
		})
	})

	When("the path is not a single directory name", func() {
		DescribeTable("should reject the path",
			func(directory string) {
				store := NewManifestStore(GinkgoT().TempDir())

				err := store.PointLatestAt(directory)
				Expect(err).Should(MatchError(ContainSubstring("single directory name")))
				_, statErr := os.Lstat(filepath.Join(store.Root, "latest"))
				Expect(statErr).Should(MatchError(os.ErrNotExist))
			},
			Entry("escapes the state root", "../outside"),
			Entry("nests below the state root", "nested/child"),
			Entry("is empty", ""),
		)
	})

	When("the directory does not exist", func() {
		It("should report the missing aggregate directory", func() {
			store := NewManifestStore(GinkgoT().TempDir())

			err := store.PointLatestAt("multi-run-10:46_8.10.2026_run-gone")
			Expect(err).Should(MatchError(ContainSubstring("read aggregate run directory")))
		})
	})

	When("the path is a regular file", func() {
		It("should reject a non-directory", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			Expect(os.WriteFile(filepath.Join(store.Root, "multi-run-file"), nil, 0o640)).Should(Succeed())

			err := store.PointLatestAt("multi-run-file")
			Expect(err).Should(MatchError(ContainSubstring("aggregate run path is not a directory")))
		})
	})

	When("latest already points at a newer run", func() {
		It("should keep the newer target", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			manifest := model.RunManifest{RunID: "run-newer", Provider: "github", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), Phase: model.PhasePreflight}
			Expect(store.Create(&manifest)).Should(Succeed())
			name := ArtifactDirectoryName("multi", time.Date(2026, 10, 8, 10, 46, 0, 0, time.UTC), "run-a")
			Expect(os.MkdirAll(filepath.Join(store.Root, name), 0o750)).Should(Succeed())

			Expect(store.PointLatestAt(name)).Should(Succeed())

			target, err := os.Readlink(filepath.Join(store.Root, "latest"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(target).Should(Equal(manifest.ArtifactDirectory))
		})
	})

	When("the manifest store root is missing", func() {
		It("should create it before linking", func() {
			root := filepath.Join(GinkgoT().TempDir(), "runs")
			store := NewManifestStore(root)
			name := ArtifactDirectoryName("multi", time.Date(2026, 10, 8, 10, 46, 0, 0, time.UTC), "run-a")
			Expect(os.MkdirAll(filepath.Join(store.Root, name), 0o750)).Should(Succeed())

			Expect(store.PointLatestAt(name)).Should(Succeed())
			Expect(filepath.Join(store.Root, "latest")).Should(BeAnExistingFile())
		})
	})
})

var _ = Describe("RecordAggregateStart", func() {
	minute := time.Date(2026, 10, 8, 10, 46, 0, 0, time.UTC)

	When("two aggregate runs share one clock minute", func() {
		It("should follow the recorded start time, not the run ID order", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			older := ArtifactDirectoryName("multi", minute, "run-zzz")
			newer := ArtifactDirectoryName("multi", minute.Add(time.Second), "run-aaa")
			// Both names encode the same minute; only the recorded times differ.
			Expect(older).Should(Equal("multi-run-10:46_8.10.2026_run-zzz"))
			Expect(newer).Should(Equal("multi-run-10:46_8.10.2026_run-aaa"))
			Expect(os.MkdirAll(filepath.Join(store.Root, older), 0o750)).Should(Succeed())
			Expect(os.MkdirAll(filepath.Join(store.Root, newer), 0o750)).Should(Succeed())
			Expect(store.RecordAggregateStart(older, minute)).Should(Succeed())
			Expect(store.RecordAggregateStart(newer, minute.Add(time.Second))).Should(Succeed())

			Expect(store.PointLatestAt(older)).Should(Succeed())
			Expect(latestTarget(store)).Should(Equal(older))
			Expect(store.PointLatestAt(newer)).Should(Succeed())
			Expect(latestTarget(store)).Should(Equal(newer))
			// A delayed older invocation must not steal the pointer back.
			Expect(store.PointLatestAt(older)).Should(Succeed())
			Expect(latestTarget(store)).Should(Equal(newer))
		})
	})

	When("only one candidate has a recorded time", func() {
		It("should decide by the directory name", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			withMarker := ArtifactDirectoryName("multi", minute, "run-zzz")
			withoutMarker := ArtifactDirectoryName("multi", minute.Add(time.Minute), "run-aaa")
			Expect(os.MkdirAll(filepath.Join(store.Root, withMarker), 0o750)).Should(Succeed())
			Expect(os.MkdirAll(filepath.Join(store.Root, withoutMarker), 0o750)).Should(Succeed())
			Expect(store.RecordAggregateStart(withMarker, minute)).Should(Succeed())

			Expect(store.PointLatestAt(withoutMarker)).Should(Succeed())
			Expect(store.PointLatestAt(withMarker)).Should(Succeed())
			Expect(latestTarget(store)).Should(Equal(withoutMarker))
		})
	})

	When("the marker request is invalid", func() {
		DescribeTable("should reject the marker request",
			func(directory string, startedAt time.Time, prepare func(store ManifestStore, name string), expected string) {
				store := NewManifestStore(GinkgoT().TempDir())
				prepare(store, directory)

				err := store.RecordAggregateStart(directory, startedAt)
				Expect(err).Should(MatchError(ContainSubstring(expected)))
				_, statErr := os.Lstat(filepath.Join(store.Root, "latest"))
				Expect(statErr).Should(MatchError(os.ErrNotExist))
			},
			Entry("escapes the state root", "../outside", minute, func(ManifestStore, string) {}, "single directory name"),
			Entry("has a zero start time", "multi-run-10:46_8.10.2026_run-gone", time.Time{}, func(store ManifestStore, name string) {
				Expect(os.MkdirAll(filepath.Join(store.Root, name), 0o750)).Should(Succeed())
			}, "aggregate start time is required"),
			Entry("names an absent directory", "multi-run-10:46_8.10.2026_run-gone", minute, func(ManifestStore, string) {}, "read aggregate run directory"),
			Entry("names a regular file", "multi-run-file", minute, func(store ManifestStore, name string) {
				Expect(os.WriteFile(filepath.Join(store.Root, name), nil, 0o640)).Should(Succeed())
			}, "aggregate run path is not a directory"),
		)
	})
})

var _ = Describe("ManifestStore.Transition", func() {
	When("the manifest cannot be persisted", func() {
		It("should keep the last persisted phase in memory", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			manifest := model.RunManifest{RunID: "run-1", Provider: "github", Phase: model.PhasePreflight}
			Expect(store.Create(&manifest)).Should(Succeed())
			runDir := store.RunDirFor(manifest)
			Expect(os.Remove(filepath.Join(runDir, "manifest.json"))).Should(Succeed())
			Expect(os.Mkdir(filepath.Join(runDir, "manifest.json"), 0o750)).Should(Succeed())

			Expect(store.Transition(&manifest, model.PhaseFixtureReady)).Should(HaveOccurred())
			Expect(manifest.Phase).Should(Equal(model.PhasePreflight))
		})
	})

	When("the transition is valid", func() {
		It("should advance the in-memory and persisted phase", func() {
			store := NewManifestStore(GinkgoT().TempDir())
			manifest := model.RunManifest{RunID: "run-1", Provider: "github", Phase: model.PhasePreflight}
			Expect(store.Create(&manifest)).Should(Succeed())

			Expect(store.Transition(&manifest, model.PhaseFixtureReady)).Should(Succeed())
			Expect(manifest.Phase).Should(Equal(model.PhaseFixtureReady))
			loaded, err := store.Load("run-1")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(loaded.Phase).Should(Equal(model.PhaseFixtureReady))
		})
	})
})

// expectLatestRun asserts that store's latest alias resolves to runID.
func expectLatestRun(store ManifestStore, runID string) {
	latest, err := store.Latest()
	Expect(err).ShouldNot(HaveOccurred())
	Expect(latest).Should(Equal(runID))
}

// latestTarget returns the current latest symlink target.
func latestTarget(store ManifestStore) string {
	target, err := os.Readlink(filepath.Join(store.Root, "latest"))
	Expect(err).ShouldNot(HaveOccurred())
	return target
}
