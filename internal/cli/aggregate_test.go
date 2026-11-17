package cli

import (
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var _ = Describe("newAggregateRun", func() {
	When("the state directory is writable", func() {
		It("should create the aggregate directory, log, and latest pointer", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())

			Expect(aggregate.RunID).Should(MatchRegexp("^run-[0-9a-f-]{36}$"))
			Expect(aggregate.Name).Should(MatchRegexp(`^multi-run-\d{2}:\d{2}_\d{1,2}\.\d{1,2}\.\d{4}_run-[0-9a-f-]+$`))
			Expect(aggregate.Directory).Should(Equal(filepath.Join(stateDir, aggregate.Name)))
			info, err := os.Stat(aggregate.Directory)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(info.IsDir()).Should(BeTrue())

			logPath := filepath.Join(aggregate.Directory, "run.log")
			logInfo, err := os.Stat(logPath)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(logInfo.Mode().Perm()).Should(Equal(os.FileMode(0o640)))

			link, err := os.Readlink(filepath.Join(stateDir, "latest"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(link).Should(Equal(aggregate.Name))

			Expect(aggregate.StartedAt.Location()).Should(Equal(time.UTC))
			Expect(aggregate.Log).ShouldNot(BeNil())
		})

		It("should record the aggregate start time", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())

			data, err := os.ReadFile(filepath.Join(aggregate.Directory, ".started-at"))
			Expect(err).ShouldNot(HaveOccurred())
			recorded, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(recorded).Should(Equal(aggregate.StartedAt))
		})

		It("should append to the aggregate log", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())

			fmt.Fprintf(aggregate.Log, "cluster c1: started\n")
			Expect(aggregate.Log.Close()).Should(Succeed())
			Expect(readFile(filepath.Join(aggregate.Directory, "run.log"))).Should(ContainSubstring("cluster c1: started\n"))
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
	It("should return the Markdown and HTML paths", func() {
		stateDir := GinkgoT().TempDir()
		aggregate, err := newAggregateRun(stateDir)
		Expect(err).ShouldNot(HaveOccurred())

		markdownPath, htmlPath := aggregate.reportPaths()
		Expect(markdownPath).Should(Equal(filepath.Join(aggregate.Directory, "report.md")))
		Expect(htmlPath).Should(Equal(filepath.Join(aggregate.Directory, "report.html")))
	})
})
