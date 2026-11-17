package auth

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("GitLabToken", func() {
	var (
		store          *Store
		tempRoot       string
		originalSecret func() ([]byte, error)
	)
	BeforeEach(func() {
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
		originalSecret = readSecret
		DeferCleanup(func() { readSecret = originalSecret })
	})

	When("a token is entered", func() {
		It("should prompt for the API URL and store a private token file", func() {
			readSecret = func() ([]byte, error) { return []byte("glpat-token\n"), nil }
			out := &bytes.Buffer{}

			token, err := GitLabToken(store, true, "", out)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("glpat-token"))
			Expect(out.String()).Should(ContainSubstring("GitLab token for https://gitlab.com/api/v4"))
			Expect(out.String()).Should(ContainSubstring("needs api scope"))

			tokenPath := filepath.Join(store.Directory(), gitlabTokenFilename)
			info, statErr := os.Stat(tokenPath)
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(info.Mode().Perm() & 0o077).Should(BeZero())
			content, readErr := os.ReadFile(tokenPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("glpat-token"))
		})
	})

	When("a custom API URL is configured", func() {
		It("should name it in the prompt", func() {
			readSecret = func() ([]byte, error) { return []byte("glpat-token"), nil }
			out := &bytes.Buffer{}

			_, err := GitLabToken(store, true, "https://gitlab.example.com/api/v4", out)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(out.String()).Should(ContainSubstring("GitLab token for https://gitlab.example.com/api/v4"))
		})
	})

	When("the session is not interactive", func() {
		It("should explain the token alternative without reading", func() {
			called := false
			readSecret = func() ([]byte, error) {
				called = true
				return nil, nil
			}

			_, err := GitLabToken(store, false, "", io.Discard)
			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(called).Should(BeFalse())
		})
	})

	When("the token is empty", func() {
		It("should fail", func() {
			readSecret = func() ([]byte, error) { return []byte(" \n"), nil }

			_, err := GitLabToken(store, true, "", io.Discard)
			Expect(err).Should(MatchError("GitLab token is required"))
		})
	})

	When("reading the token fails", func() {
		It("should return the error", func() {
			readSecret = func() ([]byte, error) { return nil, errors.New("terminal closed") }

			_, err := GitLabToken(store, true, "", io.Discard)
			Expect(err).Should(MatchError(ContainSubstring("read GitLab token")))
		})
	})

	When("no credential store is supplied", func() {
		It("should fail", func() {
			readSecret = func() ([]byte, error) { return []byte("glpat-token"), nil }

			_, err := GitLabToken(nil, true, "", io.Discard)
			Expect(err).Should(MatchError("credential store is required"))
		})
	})
})
