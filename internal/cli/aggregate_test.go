package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
)

var _ = Describe("newAggregateRun", func() {
	When("the state directory is writable", func() {
		It("should create a real latest aggregate directory and log", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(aggregate.Close)

			Expect(aggregate.RunID).Should(MatchRegexp("^run-[0-9a-f-]{36}$"))
			Expect(aggregate.Directory).Should(Equal(filepath.Join(stateDir, "latest")))
			info, err := os.Lstat(aggregate.Directory)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(info.IsDir()).Should(BeTrue())
			Expect(info.Mode() & os.ModeSymlink).Should(BeZero())

			var metadata struct {
				RunID     string    `json:"runID"`
				StartedAt time.Time `json:"startedAt"`
			}
			data, err := os.ReadFile(filepath.Join(aggregate.Directory, ".aggregate.json"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(json.Unmarshal(data, &metadata)).Should(Succeed())
			Expect(metadata.RunID).Should(Equal(aggregate.RunID))
			Expect(metadata.StartedAt.Equal(aggregate.StartedAt)).Should(BeTrue())

			logPath := filepath.Join(aggregate.Directory, "run.log")
			logInfo, err := os.Stat(logPath)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(logInfo.Mode().Perm()).Should(Equal(os.FileMode(0o640)))

			Expect(aggregate.StartedAt.Location()).Should(Equal(time.UTC))
			Expect(aggregate.Log).ShouldNot(BeNil())
		})

		It("should rotate the previous latest run when a second run starts", func() {
			stateDir := GinkgoT().TempDir()
			first, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(first.Directory, "report.md"), []byte("first run"), 0o640)).Should(Succeed())
			Expect(first.Close()).Should(Succeed())

			second, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(second.Directory).Should(Equal(filepath.Join(stateDir, "latest")))

			rotated := filepath.Join(stateDir, evidence.ArtifactDirectoryName("multi", first.StartedAt, first.RunID))
			Expect(os.ReadFile(filepath.Join(rotated, "report.md"))).Should(Equal([]byte("first run")))
			Expect(filepath.Join(stateDir, "latest", "report.md")).ShouldNot(BeAnExistingFile())
		})

		It("should append to the aggregate log", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())

			fmt.Fprintf(aggregate.Log, "cluster c1: started\n")
			Expect(aggregate.Close()).Should(Succeed())
			Expect(readFile(filepath.Join(aggregate.Directory, "run.log"))).Should(ContainSubstring("cluster c1: started\n"))
		})

		It("should refuse a second run while the state directory is held", func() {
			stateDir := GinkgoT().TempDir()
			first, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(first.Close)

			_, err = newAggregateRun(stateDir)
			Expect(err).Should(MatchError(ContainSubstring("another LKMABT run is using state directory")))
			Expect(err).Should(MatchError(ContainSubstring("--state-dir")))
		})
	})

	When("the state root is a regular file", func() {
		It("should fail", func() {
			dir := GinkgoT().TempDir()
			statePath := filepath.Join(dir, "state")
			Expect(os.WriteFile(statePath, nil, 0o640)).Should(Succeed())

			_, err := newAggregateRun(statePath)
			Expect(err).Should(MatchError(ContainSubstring("create aggregate run directory")))
		})
	})
})

var _ = Describe("reportPaths", func() {
	It("should return the Markdown and HTML paths under latest", func() {
		stateDir := GinkgoT().TempDir()
		aggregate, err := newAggregateRun(stateDir)
		Expect(err).ShouldNot(HaveOccurred())

		markdownPath, htmlPath := aggregate.reportPaths()
		Expect(markdownPath).Should(Equal(filepath.Join(stateDir, "latest", "report.md")))
		Expect(htmlPath).Should(Equal(filepath.Join(stateDir, "latest", "report.html")))
	})
})
