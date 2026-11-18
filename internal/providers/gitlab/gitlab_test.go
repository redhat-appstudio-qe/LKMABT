package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/providers"
	gl "github.com/xanzy/go-gitlab"
)

type fakeClient struct {
	project          *gl.Project
	getProjectErrors []error
	updateErrors     []error
	headCommits      []*gl.Commit
	headErrors       []error
	getCalls         int
	updateCalls      int
	headCalls        int
	revokeCalls      int
	revokeErrors     []error
}

// GetProject returns the configured fake project or the next queued error.
func (f *fakeClient) GetProject(string) (*gl.Project, error) {
	f.getCalls++
	if len(f.getProjectErrors) > 0 {
		err := f.getProjectErrors[0]
		f.getProjectErrors = f.getProjectErrors[1:]
		return nil, err
	}
	if f.project == nil {
		return nil, errors.New("not found")
	}
	return f.project, nil
}

// GetFileMetaData returns fixed file metadata for provider tests.
func (f *fakeClient) GetFileMetaData(string, string, string) (*gl.File, error) {
	return &gl.File{CommitID: "sha-1"}, nil
}

// GetFile returns base64-encoded file content for provider tests.
func (f *fakeClient) GetFile(string, string, string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte("FROM base\n")), nil
}

// UpdateFile returns the update's error or nil for provider tests.
func (f *fakeClient) UpdateFile(string, string, string, string, string, string) error {
	f.updateCalls++
	if len(f.updateErrors) > 0 {
		err := f.updateErrors[0]
		f.updateErrors = f.updateErrors[1:]
		return err
	}
	return nil
}

// GetBranchHead returns the next queued branch head for provider tests.
func (f *fakeClient) GetBranchHead(string, string) (*gl.Commit, error) {
	f.headCalls++
	if len(f.headErrors) > 0 {
		err := f.headErrors[0]
		f.headErrors = f.headErrors[1:]
		return nil, err
	}
	if len(f.headCommits) == 0 {
		return nil, errors.New("no branch head configured")
	}
	index := f.headCalls - 1
	if index >= len(f.headCommits) {
		index = len(f.headCommits) - 1
	}
	return f.headCommits[index], nil
}

// RevokeToken returns the next queued revocation error.
func (f *fakeClient) RevokeToken(context.Context) error {
	f.revokeCalls++
	if len(f.revokeErrors) > 0 {
		err := f.revokeErrors[0]
		f.revokeErrors = f.revokeErrors[1:]
		return err
	}
	return nil
}

var _ = Describe("GitLab provider", func() {
	It("revokes the run-scoped personal access token", func() {
		client := &fakeClient{}

		Expect(New(client).RevokeToken(context.Background())).Should(Succeed())
		Expect(client.revokeCalls).Should(Equal(1))
	})

	It("treats a missing personal access token as already revoked", func() {
		client := &fakeClient{revokeErrors: []error{&gl.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}}}

		Expect(New(client).RevokeToken(context.Background())).Should(Succeed())
		Expect(client.revokeCalls).Should(Equal(1))
	})

	It("treats an OAuth token rejection as already revoked", func() {
		client := &fakeClient{revokeErrors: []error{&gl.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusBadRequest},
			Message:  "400 Bad request - This endpoint requires token type to be a personal access token",
		}}}

		Expect(New(client).RevokeToken(context.Background())).Should(Succeed())
		Expect(client.revokeCalls).Should(Equal(1))
	})

	It("surfaces a token revocation failure", func() {
		client := &fakeClient{revokeErrors: []error{errors.New("forbidden")}}

		Expect(New(client).RevokeToken(context.Background())).Should(MatchError("forbidden"))
	})

	It("authenticates with a bearer token for glab OAuth tokens", func() {
		var authorization, privateToken string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = r.Header.Get("Authorization")
			privateToken = r.Header.Get("PRIVATE-TOKEN")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"path_with_namespace":"konflux-qe/dr_test_mathwizz_gl"}`))
		}))
		defer server.Close()

		adapter, err := NewFromCredentials("oauth-token", server.URL+"/api/v4")
		Expect(err).ShouldNot(HaveOccurred())
		_, err = adapter.ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(authorization).Should(Equal("Bearer oauth-token"))
		Expect(privateToken).Should(BeEmpty())
	})

	It("reuses canonical project", func() {
		client := &fakeClient{project: &gl.Project{Path: "dr_test_mathwizz_gl", PathWithNamespace: "konflux-qe/dr_test_mathwizz_gl", WebURL: CanonicalFixtureURL}}
		got, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl"})
		if err != nil {
			Expect(err).ShouldNot(HaveOccurred())
		}
		want := providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: CanonicalFixtureURL}
		Expect(got).Should(Equal(want))
		Expect(client.getCalls).Should(Equal(1))
	})

	It("retries project validation after a 5xx response", func() {
		client := &fakeClient{
			project:          &gl.Project{PathWithNamespace: "konflux-qe/dr_test_mathwizz_gl"},
			getProjectErrors: []error{&gl.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})

		_, err := adapter.ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(client.getCalls).Should(Equal(2))
	})

	It("rejects non-canonical project without creation", func() {
		client := &fakeClient{}
		_, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "other", Name: "dr_test_mathwizz_gl"})
		Expect(err).Should(HaveOccurred())
		Expect(client.getCalls).Should(Equal(0))
		Expect(client.updateCalls).Should(Equal(0))
	})

	It("reads and updates files in canonical project", func() {
		client := &fakeClient{headCommits: []*gl.Commit{{ID: "head-1"}, {ID: "commit-1", ParentIDs: []string{"head-1"}}}}
		adapter := New(client)
		fixture := providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: CanonicalFixtureURL}
		file, err := adapter.ReadFile(context.Background(), fixture, "web-server/Dockerfile", "main")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(file.SHA).Should(Equal("sha-1"))
		Expect(file.Content).Should(Equal("FROM base\n"))
		_, err = adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(client.updateCalls).Should(Equal(1))
	})

	It("retries file updates after a 5xx response", func() {
		client := &fakeClient{
			updateErrors: []error{&gl.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
			headCommits:  []*gl.Commit{{ID: "head-1"}, {ID: "commit-1", ParentIDs: []string{"head-1"}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})
		fixture := providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: CanonicalFixtureURL}

		_, err := adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(client.updateCalls).Should(Equal(2))
	})
})
