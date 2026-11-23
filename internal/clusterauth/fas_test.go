package clusterauth

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// oauthLoginHandler serves OAuth metadata and a login page advertising idp. An
// empty idp renders a page without identity-provider links.
func oauthLoginHandler(idp string, issuer *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_, _ = fmt.Fprintf(w, `{"issuer":%q}`, *issuer)
		case "/oauth/token/request":
			if idp == "" {
				_, _ = fmt.Fprint(w, `<html><body><form>basic auth</form></body></html>`)
				return
			}
			_, _ = fmt.Fprintf(w, `<a href="/oauth/authorize?client_id=x&amp;idp=%s&amp;response_type=code">Log In</a>`, url.QueryEscape(idp))
		default:
			http.NotFound(w, r)
		}
	})
}

// oauthLoginServerFor starts a plaintext or TLS OAuth server advertising idp.
func oauthLoginServerFor(idp string, secure bool) *httptest.Server {
	var issuer string
	handler := oauthLoginHandler(idp, &issuer)
	var server *httptest.Server
	if secure {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	issuer = server.URL
	return server
}

// oauthLoginServer serves OAuth metadata and a login page over plaintext.
func oauthLoginServer(idp string) *httptest.Server {
	return oauthLoginServerFor(idp, false)
}

// oauthLoginTLSServer serves OAuth metadata and a login page over TLS with a
// self-signed certificate.
func oauthLoginTLSServer(idp string) *httptest.Server {
	return oauthLoginServerFor(idp, true)
}

// oauthLinkServer serves OAuth metadata and a login page containing link.
func oauthLinkServer(link string) *httptest.Server {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_, _ = fmt.Fprintf(w, `{"issuer":%q}`, issuer)
		case "/oauth/token/request":
			_, _ = fmt.Fprint(w, link)
		default:
			http.NotFound(w, r)
		}
	}))
	issuer = server.URL
	return server
}

// oauthErrorServer fails every request so the probe's fallback can be tested.
func oauthErrorServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
}

// writeKubeconfig writes a minimal kubeconfig whose API server is server.
func writeKubeconfig(path, server string) {
	content := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    server: %s
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: c
current-context: c
users:
- name: u
  user:
    token: expired
`, server)
	writeKubeconfigContent(path, content)
}

// writeKubeconfigWithCA writes a kubeconfig that trusts certificate for server.
func writeKubeconfigWithCA(path, server string, certificate *x509.Certificate) {
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	content := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: c
current-context: c
users:
- name: u
  user:
    token: expired
`, base64.StdEncoding.EncodeToString(pemData), server)
	writeKubeconfigContent(path, content)
}

// writeKubeconfigContent writes a kubeconfig fixture in an isolated directory.
func writeKubeconfigContent(path, content string) {
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
}

var _ = Describe("probeFASIdentityProvider", func() {
	When("the login page offers a FAS identity provider", func() {
		It("should report FAS", func() {
			server := oauthLoginServer("FAS-OpenID")
			defer server.Close()

			fas, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).ShouldNot(HaveOccurred())
			Expect(fas).Should(BeTrue())
		})
	})

	When("the login page offers another identity provider", func() {
		It("should not report FAS", func() {
			server := oauthLoginServer("redhat-sso")
			defer server.Close()

			fas, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).ShouldNot(HaveOccurred())
			Expect(fas).Should(BeFalse())
		})
	})

	When("the login page has no identity-provider links", func() {
		It("should not report FAS", func() {
			server := oauthLoginServer("")
			defer server.Close()

			fas, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).ShouldNot(HaveOccurred())
			Expect(fas).Should(BeFalse())
		})
	})

	When("the OAuth metadata is unavailable", func() {
		It("should return the request error", func() {
			server := oauthErrorServer()
			defer server.Close()

			_, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).Should(MatchError(ContainSubstring("503")))
		})
	})

	When("the kubeconfig trusts the OAuth server CA", func() {
		It("should probe the TLS server", func() {
			server := oauthLoginTLSServer("FAS-OpenID")
			defer server.Close()
			kubeconfig := filepath.Join(GinkgoT().TempDir(), "config")
			writeKubeconfigWithCA(kubeconfig, server.URL, server.Certificate())

			fas, err := probeFASIdentityProvider(context.Background(), server.URL, kubeconfig)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(fas).Should(BeTrue())
		})
	})

	When("the kubeconfig does not trust the OAuth server CA", func() {
		It("should fail certificate verification", func() {
			server := oauthLoginTLSServer("FAS-OpenID")
			defer server.Close()

			_, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).Should(HaveOccurred())
		})
	})

	When("the OAuth issuer uses plaintext", func() {
		It("should reject the metadata", func() {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{"issuer":"http://example.invalid"}`)
			}))
			defer server.Close()
			kubeconfig := filepath.Join(GinkgoT().TempDir(), "config")
			writeKubeconfigWithCA(kubeconfig, server.URL, server.Certificate())

			_, err := probeFASIdentityProvider(context.Background(), server.URL, kubeconfig)

			Expect(err).Should(MatchError(ContainSubstring("does not use HTTPS")))
		})
	})

	When("the login page redirects to plaintext", func() {
		It("should refuse the downgrade", func() {
			var issuer string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/oauth-authorization-server":
					_, _ = fmt.Fprintf(w, `{"issuer":%q}`, issuer)
				case "/oauth/token/request":
					http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			issuer = server.URL
			kubeconfig := filepath.Join(GinkgoT().TempDir(), "config")
			writeKubeconfigWithCA(kubeconfig, server.URL, server.Certificate())

			_, err := probeFASIdentityProvider(context.Background(), server.URL, kubeconfig)

			Expect(err).Should(MatchError(ContainSubstring("HTTPS to HTTP")))
		})
	})
})

var _ = Describe("FAS provider links", func() {
	DescribeTable("recognized attribute forms",
		func(link string) {
			server := oauthLinkServer(link)
			defer server.Close()

			fas, err := probeFASIdentityProvider(context.Background(), server.URL, "")

			Expect(err).ShouldNot(HaveOccurred())
			Expect(fas).Should(BeTrue())
		},
		Entry("double quotes", `<a href="/oauth/authorize?idp=FAS-OpenID&amp;response_type=code">Log In</a>`),
		Entry("single quotes", `<a href='/oauth/authorize?idp=FAS-OpenID&amp;response_type=code'>Log In</a>`),
		Entry("spaced equals", `<a href = "/oauth/authorize?idp=FAS-OpenID&amp;response_type=code">Log In</a>`),
	)

	It("should ignore data-href attributes", func() {
		Expect(anchorHrefs(`<a data-href="/oauth/authorize?idp=FAS-OpenID">Log In</a>`)).Should(BeEmpty())
	})
})

var _ = Describe("isFedoraHost", func() {
	DescribeTable("host matching",
		func(host string, want bool) {
			Expect(isFedoraHost(host)).Should(Equal(want))
		},
		Entry("the root domain", "fedoraproject.org", true),
		Entry("a subdomain", "id.fedoraproject.org", true),
		Entry("uppercase", "ID.FEDORAPROJECT.ORG", true),
		Entry("a trailing dot", "id.fedoraproject.org.", true),
		Entry("a lookalike suffix", "fedoraproject.org.example.com", false),
		Entry("a lookalike prefix", "notfedoraproject.org", false),
	)
})
