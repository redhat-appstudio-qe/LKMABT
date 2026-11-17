package clusterfile_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestClusterfile runs the clusterfile package's Ginkgo test suite.
func TestClusterfile(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Clusterfile Suite")
}
