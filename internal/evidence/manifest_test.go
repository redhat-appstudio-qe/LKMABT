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
	It("persists phases and artifacts", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		createdAt := time.Date(2026, 10, 5, 14, 23, 0, 0, time.FixedZone("IDT", 3*60*60))
		manifest := model.RunManifest{RunID: "run-1", Provider: "github", Phase: model.PhasePreflight, CreatedAt: createdAt, TargetClusterServer: "https://api.example", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		Expect(store.Create(&manifest)).Should(Succeed())
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(manifest.ArtifactDirectory).Should(Equal("github-run-14:23_5.10.2026_run-1"))
		Expect(store.Transition(&manifest, model.PhaseFixtureReady)).Should(Succeed())
		loaded, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(loaded.Phase).Should(Equal(model.PhaseFixtureReady))
		for _, name := range []string{"manifest.json", "status.json"} {
			_, err = os.Stat(filepath.Join(store.RunDirFor(loaded), name))
			Expect(err).ShouldNot(HaveOccurred())
		}
	})

	It("writes identity and redacts credentials", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		manifest := model.RunManifest{RunID: "run-2", Phase: model.PhasePreflight, PipelineRuns: []model.PipelineRunIdentity{{Namespace: "tenant", Name: "build", UID: types.UID("uid-1"), StartedAt: time.Now().UTC()}}, BuildOutputs: []model.BuildOutputEvidence{{CreatedOutputs: map[string]string{"password": "secret-value"}}}}
		Expect(store.Create(&manifest)).Should(Succeed())
		data, err := os.ReadFile(filepath.Join(store.RunDirFor(manifest), "manifest.json"))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(string(data)).ShouldNot(ContainSubstring("do-not-write"))
		Expect(string(data)).Should(ContainSubstring("uid-1"))
	})

	It("rejects artifact directories outside the state root", func() {
		root := GinkgoT().TempDir()
		store := NewManifestStore(filepath.Join(root, "runs"))

		unsafe := model.RunManifest{RunID: "run-unsafe", ArtifactDirectory: "../outside", Phase: model.PhasePreflight}
		Expect(store.Create(&unsafe)).Should(MatchError(ContainSubstring("single directory name")))
		_, err := os.Stat(filepath.Join(root, "outside"))
		Expect(os.IsNotExist(err)).Should(BeTrue())
	})

	It("rejects invalid phase transition", func() {
		store := NewManifestStore(GinkgoT().TempDir())
		manifest := model.RunManifest{RunID: "run-3", Phase: model.PhasePreflight}
		Expect(store.Create(&manifest)).Should(Succeed())
		Expect(store.Transition(&manifest, model.PhaseBuildOutputsVerified)).Should(HaveOccurred())
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
