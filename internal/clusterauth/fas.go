package clusterauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// fasProbeTimeout bounds the unauthenticated OAuth metadata and login-page
// inspection; neither request carries credentials.
const fasProbeTimeout = 15 * time.Second

// maxOAuthPageBytes bounds a metadata document or login page read into memory.
const maxOAuthPageBytes = 1 << 20

// maxOAuthRedirects bounds the unauthenticated probe's redirect chain.
const maxOAuthRedirects = 5

var anchorHrefPattern = regexp.MustCompile(`(?i)<a\b[^>]*?\shref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

// fasWebLoginTarget returns the API server for oc login --web when the entry's
// OAuth login page advertises a Fedora Account System identity provider. The
// probe runs only for interactive ladders because a non-interactive run cannot
// complete a browser login. An unreadable kubeconfig server or a failed probe is
// logged and leaves the caller on the password-based oc login.
func (l Ladder) fasWebLoginTarget(ctx context.Context, entry clusterfile.Entry) (string, bool) {
	if !l.Interactive {
		return "", false
	}
	server := entry.Server
	if server == "" {
		resolved, err := cluster.ServerFromKubeconfig(entry.Kubeconfig)
		if err != nil {
			l.logf("cluster %s: read the kubeconfig server: %v", entry.Name, err)
			return "", false
		}
		server = resolved
	}
	if server == "" {
		return "", false
	}
	fas, err := probeFASIdentityProvider(ctx, server, entry.Kubeconfig)
	if err != nil {
		l.logf("cluster %s: inspect OAuth identity providers: %v", entry.Name, err)
		return "", false
	}
	if !fas {
		return "", false
	}
	l.logf("cluster %s: FAS identity provider detected; using oc login --web", entry.Name)
	return server, true
}

// probeFASIdentityProvider reports whether server's OAuth login page offers a
// Fedora Account System identity provider. It reads the API server's OAuth
// metadata and then the issuer's login page, looking for an idp link whose name
// contains "fas" (for example FAS-OpenID), or for a redirect to
// fedoraproject.org when the identity provider bypasses the chooser.
func probeFASIdentityProvider(ctx context.Context, server, kubeconfig string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, fasProbeTimeout)
	defer cancel()
	client := probeHTTPClient(kubeconfig)

	issuer, err := oauthIssuer(ctx, client, server)
	if err != nil {
		return false, err
	}
	page, finalURL, err := oauthLoginPage(ctx, client, issuer)
	if err != nil {
		return false, err
	}
	if isFedoraHost(finalURL.Hostname()) {
		return true, nil
	}
	for _, href := range anchorHrefs(page) {
		parsed, err := url.Parse(html.UnescapeString(href))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(parsed.Query().Get("idp")), "fas") {
			return true, nil
		}
	}
	return false, nil
}

// probeHTTPClient trusts the kubeconfig's CA in addition to the system roots, so
// a cluster with a private CA can still be probed. It carries no authentication,
// rejects non-HTTP(S) redirects, and refuses a TLS downgrade.
func probeHTTPClient(kubeconfig string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = probeTLSConfig(kubeconfig)
	client := &http.Client{Transport: transport, CheckRedirect: secureOAuthRedirect}
	if jar, err := cookiejar.New(nil); err == nil {
		client.Jar = jar
	}
	return client
}

// probeTLSConfig returns a TLS configuration that adds the kubeconfig's CA to
// the system roots. A kubeconfig that skips verification is honored because the
// probe sends no credentials.
func probeTLSConfig(kubeconfig string) *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	config := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if kubeconfig == "" {
		return config
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfig
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return config
	}
	if len(restConfig.CAData) > 0 {
		pool.AppendCertsFromPEM(restConfig.CAData)
	}
	if restConfig.CAFile != "" {
		if pemData, err := os.ReadFile(restConfig.CAFile); err == nil {
			pool.AppendCertsFromPEM(pemData)
		}
	}
	config.InsecureSkipVerify = restConfig.Insecure
	return config
}

// secureOAuthRedirect keeps the unauthenticated probe on HTTP(S) and refuses to
// downgrade an HTTPS request to plaintext.
func secureOAuthRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= maxOAuthRedirects {
		return fmt.Errorf("stopped after %d redirects", maxOAuthRedirects)
	}
	switch request.URL.Scheme {
	case "https":
		return nil
	case "http":
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" {
			return errors.New("refusing HTTPS to HTTP redirect")
		}
		return nil
	default:
		return fmt.Errorf("refusing redirect to scheme %q", request.URL.Scheme)
	}
}

// oauthIssuer returns the issuer advertised by the API server's OAuth metadata.
// The issuer must use HTTPS unless the API server itself was addressed over
// plaintext (tests), and it must not carry user information.
func oauthIssuer(ctx context.Context, client *http.Client, server string) (string, error) {
	endpoint := strings.TrimRight(server, "/") + "/.well-known/oauth-authorization-server"
	body, _, err := getOAuthPage(ctx, client, endpoint)
	if err != nil {
		return "", err
	}
	var metadata struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		return "", fmt.Errorf("decode OAuth metadata from %s: %w", endpoint, err)
	}
	issuer := strings.TrimRight(strings.TrimSpace(metadata.Issuer), "/")
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("OAuth metadata at %s has an invalid issuer %q", endpoint, metadata.Issuer)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && strings.HasPrefix(server, "http://")) {
		return "", fmt.Errorf("OAuth issuer %s does not use HTTPS", issuer)
	}
	return issuer, nil
}

// oauthLoginPage returns the issuer's token-request page and its final URL after
// redirects; the final URL identifies an identity provider that bypasses the
// chooser.
func oauthLoginPage(ctx context.Context, client *http.Client, issuer string) (string, *url.URL, error) {
	return getOAuthPage(ctx, client, issuer+"/oauth/token/request")
}

// getOAuthPage fetches one unauthenticated OAuth page.
func getOAuthPage(ctx context.Context, client *http.Client, endpoint string) (string, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request for %s: %w", endpoint, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("GET %s: %s", endpoint, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthPageBytes))
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", endpoint, err)
	}
	return string(body), response.Request.URL, nil
}

// isFedoraHost reports whether host is fedoraproject.org or one of its
// subdomains.
func isFedoraHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "fedoraproject.org" || strings.HasSuffix(host, ".fedoraproject.org")
}

// anchorHrefs returns the href values of anchor elements.
func anchorHrefs(page string) []string {
	matches := anchorHrefPattern.FindAllStringSubmatch(page, -1)
	hrefs := make([]string, 0, len(matches))
	for _, match := range matches {
		for _, candidate := range match[1:] {
			if candidate != "" {
				hrefs = append(hrefs, candidate)
				break
			}
		}
	}
	return hrefs
}
