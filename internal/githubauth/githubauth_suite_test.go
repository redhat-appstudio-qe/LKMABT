package githubauth

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestGitHubAuth runs the GitHub authentication package suite.
func TestGitHubAuth(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitHub Auth Suite")
}
