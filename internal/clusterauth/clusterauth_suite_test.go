package clusterauth

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestClusterauth runs the clusterauth package's Ginkgo test suite.
func TestClusterauth(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Clusterauth Suite")
}
