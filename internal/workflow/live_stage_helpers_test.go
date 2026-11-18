package workflow

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("build wait progress", func() {
	It("renders enabled and pending PaC components", func() {
		names := []string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}
		objects := []unstructured.Unstructured{
			componentWithPaCStatus("mathwizz-web-server", "enabled"),
			componentWithPaCStatus("mathwizz-frontend", "error"),
		}

		Expect(paCWaitStatus(names, objects)).Should(Equal("PaC onboarding: 1/3 enabled; pending mathwizz-history-worker, mathwizz-frontend"))
		Expect(paCWaitStatus(names, nil)).Should(Equal("PaC onboarding: 0/3 enabled; pending mathwizz-web-server, mathwizz-history-worker, mathwizz-frontend"))
		Expect(paCWaitStatus(names, []unstructured.Unstructured{
			componentWithPaCStatus("mathwizz-web-server", "enabled"),
			componentWithPaCStatus("mathwizz-history-worker", "enabled"),
			componentWithPaCStatus("mathwizz-frontend", "enabled"),
		})).Should(Equal("PaC onboarding: 3/3 enabled"))
	})

	It("renders each component's build phase", func() {
		now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		commits := []model.TriggerCommit{
			{Component: "running", SHA: "sha-running"},
			{Component: "pending", SHA: "sha-pending"},
			{Component: "failed", SHA: "sha-failed"},
			{Component: "succeeded", SHA: "sha-succeeded"},
			{Component: "missing", SHA: "sha-missing"},
		}
		pipelineRuns := []unstructured.Unstructured{
			pipelineRunAt("run-running", "running", "sha-running", "uid-running", "Unknown", now.Add(-90*time.Second)),
			pipelineRunAt("run-pending", "pending", "sha-pending", "uid-pending", "Unknown", now.Add(-3*time.Minute)),
			pipelineRunAt("run-failed", "failed", "sha-failed", "uid-failed", "False", now.Add(-time.Minute)),
			pipelineRunAt("run-succeeded", "succeeded", "sha-succeeded", "uid-succeeded", "True", now.Add(-time.Minute)),
		}
		taskRuns := []unstructured.Unstructured{
			buildTaskRunForWorkflow("task-running", "uid-running", "Unknown", "buildah-oci-ta"),
			buildTaskRunForWorkflow("task-succeeded", "uid-succeeded", "True", "buildah-oci-ta"),
		}

		Expect(buildWaitStatus(commits, pipelineRuns, taskRuns, now)).Should(Equal("builds: " +
			"running: running for 1m30s (PipelineRun run-running, buildah 0/1 succeeded); " +
			"pending: pending for 3m0s (PipelineRun run-pending, no Buildah TaskRun); " +
			"failed: failed (PipelineRun run-failed); " +
			"succeeded: succeeded (PipelineRun run-succeeded, buildah 1/1); " +
			"missing: no PipelineRun"))
	})

	It("does not report a succeeded PipelineRun as a finished component before Buildah work", func() {
		now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		commits := []model.TriggerCommit{{Component: "web", SHA: "sha-web"}}
		pipelineRuns := []unstructured.Unstructured{
			pipelineRunAt("run-web", "web", "sha-web", "uid-web", "True", now.Add(-time.Minute)),
		}

		Expect(buildWaitStatus(commits, pipelineRuns, nil, now)).Should(Equal("builds: web: PipelineRun run-web succeeded; waiting for Buildah TaskRuns"))

		taskRuns := []unstructured.Unstructured{
			buildTaskRunForWorkflow("task-web", "uid-web", "Unknown", "buildah-oci-ta"),
		}
		Expect(buildWaitStatus(commits, pipelineRuns, taskRuns, now)).Should(Equal("builds: web: PipelineRun run-web succeeded; buildah 0/1 succeeded"))
	})

	It("selects and deduplicates the PipelineRuns a wait matches to the triggers", func() {
		now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		objects := []unstructured.Unstructured{
			pipelineRunAt("run-web", "mathwizz-web-server", "sha-web", "uid-web", "Unknown", now),
			pipelineRunAt("run-other", "mathwizz-frontend", "sha-other", "uid-other", "Unknown", now),
		}
		commits := []model.TriggerCommit{
			{Component: "mathwizz-web-server", SHA: "sha-web"},
			{Component: "mathwizz-web-server", SHA: "sha-web"},
		}

		candidates := candidatePipelineRunsForCommits(objects, commits)
		Expect(candidates).Should(HaveLen(1))
		Expect(candidates[0].GetName()).Should(Equal("run-web"))
	})
})
