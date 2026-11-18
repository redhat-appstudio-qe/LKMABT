package filelock_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestFilelock runs the Ginkgo suite for the filelock package.
func TestFilelock(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Filelock Suite")
}
