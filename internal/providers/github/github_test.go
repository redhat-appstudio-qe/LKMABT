package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	gh "github.com/google/go-github/v66/github"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
)

type fakeClient struct {
	repository             *gh.Repository
	getRepositoryErrors    []error
	updateErrors           []error
	installations          []*gh.Installation
	installationErr        error
	installationRepos      []*gh.Repository
	installationRepoErr    error
	installationRepoErrors []error
	getCalls               int
	updateCalls            int
	updateMessage          string
	installationCalls      int
	installationRepoCalls  []int64
}

// GetRepository returns the configured fake repository or the next queued error.
func (f *fakeClient) GetRepository(context.Context, string, string) (*gh.Repository, error) {
	f.getCalls++
	if len(f.getRepositoryErrors) > 0 {
		err := f.getRepositoryErrors[0]
		f.getRepositoryErrors = f.getRepositoryErrors[1:]
		return nil, err
	}
	if f.repository == nil {
		return nil, errors.New("not found")
	}
	return f.repository, nil
}

// GetFile returns fixed encoded file content for provider tests.
func (f *fakeClient) GetFile(context.Context, string, string, string, string) (*gh.RepositoryContent, error) {
	return &gh.RepositoryContent{SHA: gh.String("sha-1"), Content: gh.String("ZmlsZQ==")}, nil
}

// UpdateFile returns the next queued error or a fixed commit response.
func (f *fakeClient) UpdateFile(_ context.Context, _ string, _ string, _ string, _ string, _ string, _ string, message string) (*gh.RepositoryContentResponse, error) {
	f.updateCalls++
	f.updateMessage = message
	if len(f.updateErrors) > 0 {
		err := f.updateErrors[0]
		f.updateErrors = f.updateErrors[1:]
		return nil, err
	}
	return &gh.RepositoryContentResponse{Commit: gh.Commit{SHA: gh.String("commit-1")}}, nil
}

// ListOrganizationInstallations returns the configured fake installations or error.
func (f *fakeClient) ListOrganizationInstallations(context.Context, string) ([]*gh.Installation, error) {
	f.installationCalls++
	return f.installations, f.installationErr
}

// ListUserInstallationRepositories returns the configured fake installation repositories or error.
func (f *fakeClient) ListUserInstallationRepositories(_ context.Context, id int64) ([]*gh.Repository, error) {
	f.installationRepoCalls = append(f.installationRepoCalls, id)
	if len(f.installationRepoErrors) > 0 {
		err := f.installationRepoErrors[0]
		f.installationRepoErrors = f.installationRepoErrors[1:]
		return nil, err
	}
	return f.installationRepos, f.installationRepoErr
}

var _ = Describe("GitHub provider", func() {
	It("reuses canonical repository", func() {
		client := &fakeClient{repository: &gh.Repository{Name: gh.String("dr_test_mathwizz"), Owner: &gh.User{Login: gh.String("redhat-appstudio-qe")}}}
		got, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz"})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		want := providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz", URL: CanonicalFixtureURL}
		Expect(got).To(Equal(want))
		Expect(client.getCalls).To(Equal(1))
	})

	It("retries repository validation after a 5xx response", func() {
		client := &fakeClient{
			repository:          &gh.Repository{Name: gh.String("dr_test_mathwizz"), Owner: &gh.User{Login: gh.String("redhat-appstudio-qe")}},
			getRepositoryErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})

		_, err := adapter.ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz"})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.getCalls).To(Equal(2))
	})

	It("rejects non-canonical repository without creation", func() {
		client := &fakeClient{}
		_, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "other", Name: "dr_test_mathwizz"})
		Expect(err).To(HaveOccurred())
		Expect(client.getCalls).To(Equal(0))
		Expect(client.updateCalls).To(Equal(0))
	})

	It("reads and updates files in canonical repository", func() {
		client := &fakeClient{}
		adapter := New(client)
		fixture := providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz", URL: CanonicalFixtureURL}
		file, err := adapter.ReadFile(context.Background(), fixture, "web-server/Dockerfile", "main")
		Expect(err).NotTo(HaveOccurred())
		Expect(file.SHA).To(Equal("sha-1"))
		_, err = adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1", providers.CommitMessageTrigger)
		Expect(err).NotTo(HaveOccurred())
		Expect(client.updateCalls).To(Equal(1))
		Expect(client.updateMessage).To(Equal(providers.CommitMessageTrigger))
	})

	It("retries file updates after a 5xx response", func() {
		client := &fakeClient{
			updateErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})
		fixture := providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz", URL: CanonicalFixtureURL}

		_, err := adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1", providers.CommitMessageTrigger)
		Expect(err).NotTo(HaveOccurred())
		Expect(client.updateCalls).To(Equal(2))
	})
})

var _ = Describe("CheckOrganizationAccess", func() {
	When("the token can list installations", func() {
		It("should pass", func() {
			client := &fakeClient{installations: []*gh.Installation{{AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("all")}}}

			Expect(New(client).CheckOrganizationAccess(context.Background())).Should(Succeed())
		})
	})

	When("the token cannot list installations", func() {
		It("should return the API error", func() {
			client := &fakeClient{installationErr: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}}

			err := New(client).CheckOrganizationAccess(context.Background())
			Expect(err).Should(MatchError(ContainSubstring("list GitHub App installations for redhat-appstudio-qe")))
		})
	})
})

var _ = Describe("CheckInstallationRepositoryAccess", func() {
	When("a selected installation's repositories can be listed", func() {
		It("should pass and list exactly that installation", func() {
			client := &fakeClient{installations: []*gh.Installation{
				{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")},
				{ID: gh.Int64(8), AppSlug: gh.String("other-app"), RepositorySelection: gh.String("selected")},
			}}

			Expect(New(client).CheckInstallationRepositoryAccess(context.Background())).Should(Succeed())
			Expect(client.installationRepoCalls).Should(Equal([]int64{7, 8}))
		})
	})

	When("one selected installation is not visible to the token", func() {
		It("should skip the 404 and check the remaining installations", func() {
			client := &fakeClient{
				installations: []*gh.Installation{
					{ID: gh.Int64(7), RepositorySelection: gh.String("selected")},
					{ID: gh.Int64(8), RepositorySelection: gh.String("selected")},
				},
				installationRepoErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}},
			}

			Expect(New(client).CheckInstallationRepositoryAccess(context.Background())).Should(Succeed())
			Expect(client.installationRepoCalls).Should(Equal([]int64{7, 8}))
		})
	})

	When("the token cannot list a selected installation's repositories", func() {
		It("should reject the credential and name the read:user scope", func() {
			client := &fakeClient{
				installations:       []*gh.Installation{{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")}},
				installationRepoErr: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}},
			}

			err := New(client).CheckInstallationRepositoryAccess(context.Background())
			Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeTrue())
			Expect(err).Should(MatchError(ContainSubstring("read:user")))
			Expect(err).Should(MatchError(ContainSubstring("installation 7")))
		})
	})

	When("listing a selected installation's repositories fails transiently", func() {
		It("should not reject the credential on a 5xx response", func() {
			client := &fakeClient{
				installations:       []*gh.Installation{{ID: gh.Int64(7), RepositorySelection: gh.String("selected")}},
				installationRepoErr: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}},
			}

			err := New(client).CheckInstallationRepositoryAccess(context.Background())
			Expect(err).Should(HaveOccurred())
			Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeFalse())
		})

		It("should not reject the credential on a rate limit", func() {
			client := &fakeClient{
				installations:       []*gh.Installation{{ID: gh.Int64(7), RepositorySelection: gh.String("selected")}},
				installationRepoErr: &gh.RateLimitError{},
			}

			err := New(client).CheckInstallationRepositoryAccess(context.Background())
			Expect(err).Should(HaveOccurred())
			Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeFalse())
		})
	})

	When("the credential is a gh OAuth user token", func() {
		It("should skip the installation repository listing", func() {
			client := &fakeClient{installations: []*gh.Installation{
				{ID: gh.Int64(7), RepositorySelection: gh.String("selected")},
			}}
			adapter := New(client)
			adapter.oauthToken = true

			Expect(adapter.CheckInstallationRepositoryAccess(context.Background())).Should(Succeed())
			Expect(client.installationRepoCalls).Should(BeEmpty())
		})
	})

	When("a selected installation is suspended", func() {
		It("should skip it without listing repositories", func() {
			client := &fakeClient{installations: []*gh.Installation{
				{ID: gh.Int64(7), RepositorySelection: gh.String("selected"), SuspendedAt: &gh.Timestamp{}},
			}}

			Expect(New(client).CheckInstallationRepositoryAccess(context.Background())).Should(Succeed())
			Expect(client.installationRepoCalls).Should(BeEmpty())
		})
	})

	When("an installation covers all repositories", func() {
		It("should not list repositories", func() {
			client := &fakeClient{installations: []*gh.Installation{
				{ID: gh.Int64(7), RepositorySelection: gh.String("all")},
			}}

			Expect(New(client).CheckInstallationRepositoryAccess(context.Background())).Should(Succeed())
			Expect(client.installationRepoCalls).Should(BeEmpty())
		})
	})
})

var _ = Describe("CheckFixtureWriteAccess", func() {
	It("passes when the token can push to the fixture", func() {
		client := &fakeClient{repository: &gh.Repository{Name: gh.String("dr_test_mathwizz"), Permissions: map[string]bool{"push": true}}}

		Expect(New(client).CheckFixtureWriteAccess(context.Background())).Should(Succeed())
		Expect(client.getCalls).Should(Equal(1))
	})

	It("rejects a token without push permission", func() {
		client := &fakeClient{repository: &gh.Repository{Name: gh.String("dr_test_mathwizz"), Permissions: map[string]bool{"pull": true}}}

		err := New(client).CheckFixtureWriteAccess(context.Background())
		Expect(err).Should(MatchError(ContainSubstring("cannot push to " + CanonicalFixtureURL)))
		Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeTrue())
	})

	It("rejects a credential GitHub refuses", func() {
		client := &fakeClient{getRepositoryErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}}}

		err := New(client).CheckFixtureWriteAccess(context.Background())
		Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeTrue())
	})

	It("retries a transient server failure without rejecting the credential", func() {
		client := &fakeClient{
			repository:          &gh.Repository{Name: gh.String("dr_test_mathwizz"), Permissions: map[string]bool{"push": true}},
			getRepositoryErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})

		err := adapter.CheckFixtureWriteAccess(context.Background())
		Expect(err).ShouldNot(HaveOccurred())
		Expect(client.getCalls).Should(Equal(2))
	})

	It("does not reject the credential on a rate limit", func() {
		client := &fakeClient{installationErr: &gh.RateLimitError{}}
		adapter := New(client)

		err := adapter.CheckOrganizationAccess(context.Background())
		Expect(err).Should(MatchError(ContainSubstring("list GitHub App installations")))
		Expect(errors.Is(err, providers.ErrInsufficientGitHubAccess)).Should(BeFalse())
	})
})

var _ = Describe("VerifyGitHubAppInstallation", func() {
	When("the cluster app is installed for all repositories", func() {
		It("should pass", func() {
			client := &fakeClient{installations: []*gh.Installation{{AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("all")}}}

			Expect(New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")).Should(Succeed())
		})
	})

	When("the cluster app is not installed in the fixture organization", func() {
		It("should return the typed installation error", func() {
			client := &fakeClient{installations: []*gh.Installation{{AppSlug: gh.String("other-app"), RepositorySelection: gh.String("all")}}}

			err := New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(err).Should(MatchError(ContainSubstring("GitHub App cluster-app is not installed for organization redhat-appstudio-qe")))
			var required *providers.AppInstallationRequiredError
			Expect(errors.As(err, &required)).Should(BeTrue())
			Expect(required.AppSlug).Should(Equal("cluster-app"))
			Expect(required.Owner).Should(Equal("redhat-appstudio-qe"))
			Expect(required.Repository).Should(Equal("redhat-appstudio-qe/dr_test_mathwizz"))
			Expect(required.InstallURL).Should(Equal("https://github.com/apps/cluster-app/installations/new"))
		})
	})

	When("the app is installed for selected repositories", func() {
		It("should pass when the fixture repository is selected", func() {
			client := &fakeClient{
				installations:     []*gh.Installation{{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")}},
				installationRepos: []*gh.Repository{{Owner: &gh.User{Login: gh.String("redhat-appstudio-qe")}, Name: gh.String("dr_test_mathwizz")}},
			}

			Expect(New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")).Should(Succeed())
			Expect(client.installationRepoCalls).Should(Equal([]int64{7}))
		})

		It("should fail when the fixture repository is not selected", func() {
			client := &fakeClient{
				installations:     []*gh.Installation{{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")}},
				installationRepos: []*gh.Repository{{Owner: &gh.User{Login: gh.String("redhat-appstudio-qe")}, Name: gh.String("other")}},
			}

			err := New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(err).Should(MatchError(ContainSubstring("not for redhat-appstudio-qe/dr_test_mathwizz")))
			Expect(err.Error()).Should(ContainSubstring("https://github.com/organizations/redhat-appstudio-qe/settings/installations/7"))
			var selection *providers.AppRepositorySelectionError
			Expect(errors.As(err, &selection)).Should(BeTrue())
		})

		It("should fail when the selected repositories cannot be listed", func() {
			client := &fakeClient{
				installations:       []*gh.Installation{{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")}},
				installationRepoErr: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}},
			}

			err := New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(err).Should(MatchError(ContainSubstring("cannot be verified")))
			Expect(errors.Is(err, providers.ErrUnverifiable)).Should(BeTrue())
		})

		It("should skip the listing for a gh OAuth user token", func() {
			client := &fakeClient{
				installations: []*gh.Installation{{ID: gh.Int64(7), AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("selected")}},
			}
			adapter := New(client)
			adapter.oauthToken = true

			err := adapter.VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(errors.Is(err, providers.ErrUnverifiable)).Should(BeTrue())
			Expect(errors.Is(err, providers.ErrTokenCannotListInstallationRepositories)).Should(BeTrue())
			Expect(client.installationRepoCalls).Should(BeEmpty())
		})
	})

	When("the installation is suspended", func() {
		It("should fail", func() {
			client := &fakeClient{installations: []*gh.Installation{{AppSlug: gh.String("cluster-app"), RepositorySelection: gh.String("all"), SuspendedAt: &gh.Timestamp{}}}}

			err := New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(err).Should(MatchError(ContainSubstring("is suspended")))
		})
	})

	When("the organization installations cannot be listed", func() {
		It("should return the API error with the token requirement", func() {
			client := &fakeClient{installationErr: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}}

			err := New(client).VerifyGitHubAppInstallation(context.Background(), "cluster-app")
			Expect(err).Should(MatchError(ContainSubstring("needs the read:org scope")))
		})
	})

	When("the app slug is empty", func() {
		It("should fail without calling the API", func() {
			client := &fakeClient{}

			Expect(New(client).VerifyGitHubAppInstallation(context.Background(), " ")).Should(MatchError("GitHub App slug is required"))
			Expect(client.installationCalls).Should(Equal(0))
		})
	})
})
