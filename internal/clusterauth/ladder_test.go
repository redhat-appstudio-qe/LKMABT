package clusterauth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// defaultEntry builds an entry using kflux's default kubeconfig path for home.
func defaultEntry(name, home string) clusterfile.Entry {
	entry := clusterfile.Entry{Name: name}
	entry.Kubeconfig = clusterfile.DefaultKubeconfig(home, entry.CatalogName())
	return entry
}

// customEntry builds an entry with an explicit kubeconfig path and optional server.
func customEntry(name, path, server string) clusterfile.Entry {
	return clusterfile.Entry{Name: name, Kubeconfig: path, Server: server}
}

// writeFile creates a minimal kubeconfig file (and its parent directory).
func writeFile(path string) {
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
	Expect(os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600)).To(Succeed())
}

var _ = Describe("Authenticate", func() {
	When("every kubeconfig exists", func() {
		It("should verify each kubeconfig without logging in", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			c2 := defaultEntry("c2", home)
			writeFile(c1.Kubeconfig)
			writeFile(c2.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{}}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1, c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(BeEmpty())
			Expect(fake.outputs).Should(HaveLen(2))
			Expect(fake.outputs[0].Name).Should(Equal("oc"))
			Expect(fake.outputs[0].Args).Should(Equal([]string{"whoami", "--request-timeout=30s"}))
			Expect(fake.outputs[0].Env).Should(Equal([]string{"KUBECONFIG=" + c1.Kubeconfig}))
		})
	})

	When("the stored credentials were rejected", func() {
		It("should refresh a default-path kubeconfig with kflux before running", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			writeFile(c1.Kubeconfig)
			log := &bytes.Buffer{}
			authenticated := false
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					authenticated = true
				}
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) {
				if !authenticated {
					return "error: You must be logged in to the server (Unauthorized)", errors.New("exit status 1")
				}
				return "mrath\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0]).Should(Equal(CommandSpec{Name: "/usr/local/bin/kflux", Args: []string{"cluster", "login", "-c", "c1"}}))
			Expect(fake.outputs).Should(HaveLen(2))
			Expect(log.String()).Should(ContainSubstring("cluster c1 authenticated as mrath"))
		})

		It("should run oc login for a custom-path kubeconfig without kflux", func() {
			home := GinkgoT().TempDir()
			c2 := customEntry("c2", filepath.Join(home, "custom", "c2"), "")
			writeFile(c2.Kubeconfig)
			authenticated := false
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "oc" {
					authenticated = true
				}
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) {
				if !authenticated {
					return "Unauthorized", errors.New("exit status 1")
				}
				return "mrath\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Name).Should(Equal("oc"))
			Expect(fake.runs[0].Args).Should(Equal([]string{"login"}))
		})

		It("should fail at authentication when the ladder cannot replace the credentials", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			writeFile(c1.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "error: You must be logged in to the server (Unauthorized)", errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(HaveLen(1))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("authentication failed")))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring(c1.Kubeconfig)))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("Unauthorized")))
			Expect(fake.runs).Should(HaveLen(2))
			Expect(fake.runs[0].Name).Should(Equal("/usr/local/bin/kflux"))
			Expect(fake.runs[1].Name).Should(Equal("oc"))
		})
	})

	When("the API server cannot be reached", func() {
		It("should report the verification failure without attempting a login", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			writeFile(c1.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "Unable to connect to the server: dial tcp: lookup api.c1.example: no such host", errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(HaveLen(1))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("could not verify kubeconfig")))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("no such host")))
			Expect(fake.runs).Should(BeEmpty())
		})
	})

	When("one default-path kubeconfig is missing", func() {
		It("should run a single-cluster kflux login", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					writeFile(c1.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0]).Should(Equal(CommandSpec{Name: "/usr/local/bin/kflux", Args: []string{"cluster", "login", "-c", "c1"}}))
			// The missing file is never handed to oc whoami; only the kflux result is verified.
			Expect(fake.outputs).Should(HaveLen(1))
		})
	})

	When("two default-path kubeconfigs are missing", func() {
		It("should name both clusters without touching the rest of the catalog", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			c2 := defaultEntry("c2", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					writeFile(c1.Kubeconfig)
					writeFile(c2.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1, c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Args).Should(Equal([]string{"cluster", "login", "-c", "c1", "-c", "c2"}))
		})
	})

	When("an entry maps to a different kflux catalog name", func() {
		It("should log in to the catalog name and accept its kubeconfig", func() {
			home := GinkgoT().TempDir()
			entry := clusterfile.Entry{Name: "kflux-ocp-p01", Kubeconfig: clusterfile.DefaultKubeconfig(home, "ocp-p01")}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					writeFile(entry.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0]).Should(Equal(CommandSpec{Name: "/usr/local/bin/kflux", Args: []string{"cluster", "login", "-c", "ocp-p01"}}))
		})
	})

	When("a cluster name is a real Konflux host label", func() {
		It("should resolve the kflux catalog alias and use its kubeconfig", func() {
			home := GinkgoT().TempDir()
			entry := defaultEntry("stone-stage-p01", home)
			resolvedPath := clusterfile.DefaultKubeconfig(home, "stg-p01")
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onStdout = func(spec CommandSpec) (string, error) {
				Expect(spec.Name).Should(Equal("/usr/local/bin/kflux"))
				Expect(spec.Args).Should(Equal([]string{"cluster", "list", "--output", "json"}))
				return `[{"name":"stg-p01","host":"stone-stage-p01.hpmt.p1.openshiftapps.com"}]`, nil
			}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"cluster", "login", "-c", "stg-p01"}))
				writeFile(resolvedPath)
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				Expect(spec.Env).Should(Equal([]string{"KUBECONFIG=" + resolvedPath}))
				return "mrath\n", nil
			}

			resolved, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(resolved).Should(HaveLen(1))
			Expect(resolved[0].Kubeconfig).Should(Equal(resolvedPath))
			Expect(resolved[0].CatalogName()).Should(Equal("stg-p01"))
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.stdouts).Should(HaveLen(1))
			Expect(log.String()).Should(ContainSubstring("resolved kflux catalog cluster stg-p01"))
		})

		It("should resolve from an explicit server host", func() {
			home := GinkgoT().TempDir()
			entry := clusterfile.Entry{
				Name:       "stone-alias",
				Kubeconfig: clusterfile.DefaultKubeconfig(home, "stone-alias"),
				Server:     "https://api.stone-stage-p01.hpmt.p1.openshiftapps.com:6443",
			}
			resolvedPath := clusterfile.DefaultKubeconfig(home, "stg-p01")
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onStdout = func(CommandSpec) (string, error) {
				return `[{"name":"stg-p01","host":"stone-stage-p01.hpmt.p1.openshiftapps.com"}]`, nil
			}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"cluster", "login", "-c", "stg-p01"}))
				writeFile(resolvedPath)
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				Expect(spec.Env).Should(Equal([]string{"KUBECONFIG=" + resolvedPath}))
				return "mrath\n", nil
			}

			resolved, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(resolved).Should(HaveLen(1))
			Expect(resolved[0].Kubeconfig).Should(Equal(resolvedPath))
			Expect(resolved[0].CatalogName()).Should(Equal("stg-p01"))
		})
	})

	When("a cluster has no kubeconfig and no server", func() {
		It("should refuse oc login instead of using the ambient context", func() {
			home := GinkgoT().TempDir()
			entry := defaultEntry("unknown-cluster", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onStdout = func(CommandSpec) (string, error) { return `[]`, nil }
			fake.onRun = func(CommandSpec) error { return errors.New("exit status 1") }

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(HaveKey("unknown-cluster"))
			Expect(failures["unknown-cluster"]).Should(MatchError(ContainSubstring("no readable kubeconfig")))
			Expect(failures["unknown-cluster"]).Should(MatchError(ContainSubstring("no server")))
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Name).Should(Equal("/usr/local/bin/kflux"))
		})
	})

	When("an interactive cluster uses a FAS identity provider", func() {
		It("should log in through the browser with oc login --web", func() {
			home := GinkgoT().TempDir()
			oauth := oauthLoginServer("FAS-OpenID")
			defer oauth.Close()
			entry := clusterfile.Entry{Name: "kflux-fedora-01", Server: oauth.URL, Kubeconfig: filepath.Join(home, "config-fedora-01")}
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Name).Should(Equal("oc"))
				Expect(spec.Args).Should(Equal([]string{"login", "--web", "--server=" + oauth.URL}))
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) { return "mrath\n", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Interactive: true, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(log.String()).Should(ContainSubstring("FAS identity provider detected"))
			Expect(log.String()).Should(ContainSubstring("running oc login --web --server=" + oauth.URL))
		})

		It("should read the server from a rejected kubeconfig", func() {
			home := GinkgoT().TempDir()
			oauth := oauthLoginServer("FAS-OpenID")
			defer oauth.Close()
			entry := clusterfile.Entry{Name: "kflux-fedora-01", Kubeconfig: filepath.Join(home, "config-fedora-01")}
			writeKubeconfig(entry.Kubeconfig, oauth.URL)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"login", "--web", "--server=" + oauth.URL}))
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) {
				if len(fake.outputs) < 2 {
					return "error: You must be logged in to the server (Unauthorized)", errors.New("exit status 1")
				}
				return "mrath\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Interactive: true, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Args).Should(Equal([]string{"login", "--web", "--server=" + oauth.URL}))
		})
	})

	When("an interactive cluster uses another identity provider", func() {
		It("should keep the password-based oc login", func() {
			home := GinkgoT().TempDir()
			oauth := oauthLoginServer("redhat-sso")
			defer oauth.Close()
			entry := clusterfile.Entry{Name: "kflux-ocp-p01", Server: oauth.URL, Kubeconfig: filepath.Join(home, "config-ocp-p01")}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"login", "--server=" + oauth.URL}))
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) { return "mrath\n", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Interactive: true, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
		})
	})

	When("the OAuth identity-provider probe fails", func() {
		It("should keep the password-based oc login", func() {
			home := GinkgoT().TempDir()
			oauth := oauthErrorServer()
			defer oauth.Close()
			entry := clusterfile.Entry{Name: "kflux-ocp-p01", Server: oauth.URL, Kubeconfig: filepath.Join(home, "config-ocp-p01")}
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"login", "--server=" + oauth.URL}))
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) { return "mrath\n", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Interactive: true, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(log.String()).Should(ContainSubstring("inspect OAuth identity providers"))
		})
	})

	When("a FAS cluster is not interactive", func() {
		It("should not use the browser login", func() {
			home := GinkgoT().TempDir()
			oauth := oauthLoginServer("FAS-OpenID")
			defer oauth.Close()
			entry := clusterfile.Entry{Name: "kflux-fedora-01", Server: oauth.URL, Kubeconfig: filepath.Join(home, "config-fedora-01")}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				Expect(spec.Args).Should(Equal([]string{"login", "--server=" + oauth.URL}))
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(CommandSpec) (string, error) { return "mrath\n", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})

			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
		})
	})

	When("the kflux batch login fails", func() {
		It("should retry valid catalog clusters individually", func() {
			home := GinkgoT().TempDir()
			valid := defaultEntry("kflux-ocp-p01", home)
			unknown := defaultEntry("stone-unknown", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onStdout = func(CommandSpec) (string, error) { return `[]`, nil }
			fake.onRun = func(spec CommandSpec) error {
				if len(spec.Args) > 4 {
					return errors.New("unknown cluster stone-unknown")
				}
				if spec.Args[len(spec.Args)-1] == "ocp-p01" {
					writeFile(valid.Kubeconfig)
					return nil
				}
				return errors.New("unknown cluster stone-unknown")
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				Expect(spec.Env).Should(Equal([]string{"KUBECONFIG=" + valid.Kubeconfig}))
				return "mrath\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{valid, unknown})

			Expect(failures).Should(HaveKey("stone-unknown"))
			Expect(failures).NotTo(HaveKey("kflux-ocp-p01"))
			Expect(fake.runs).Should(HaveLen(3))
		})
	})

	When("two entries resolve to one kubeconfig", func() {
		It("should reject the colliding entry instead of overwriting it", func() {
			home := GinkgoT().TempDir()
			host := defaultEntry("stone-stage-p01", home)
			alias := defaultEntry("kflux-stg-p01", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onStdout = func(CommandSpec) (string, error) {
				return `[{"name":"stg-p01","host":"stone-stage-p01.hpmt.p1.openshiftapps.com"}]`, nil
			}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					writeFile(alias.Kubeconfig)
				}
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				Expect(spec.Env).Should(Equal([]string{"KUBECONFIG=" + alias.Kubeconfig}))
				return "mrath\n", nil
			}

			resolved, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{host, alias})

			Expect(failures).Should(HaveKey("stone-stage-p01"))
			Expect(failures["stone-stage-p01"]).Should(MatchError(ContainSubstring("resolve to the same kubeconfig path")))
			Expect(failures).NotTo(HaveKey("kflux-stg-p01"))
			Expect(resolved).Should(HaveLen(2))
			Expect(resolved[0].Kubeconfig).Should(Equal(alias.Kubeconfig))
			Expect(resolved[1].Kubeconfig).Should(Equal(alias.Kubeconfig))
		})
	})

	When("a custom-path kubeconfig is missing", func() {
		It("should skip kflux and run oc login for that path", func() {
			home := GinkgoT().TempDir()
			c2 := customEntry("c2", filepath.Join(home, "custom", "c2"), "https://api.c2.example:6443")
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "oc" {
					writeFile(c2.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Name).Should(Equal("oc"))
			Expect(fake.runs[0].Args).Should(Equal([]string{"login", "--server=https://api.c2.example:6443"}))
			Expect(fake.runs[0].Env).Should(Equal([]string{"KUBECONFIG=" + c2.Kubeconfig}))
		})

		It("should pass --server when the entry declares it", func() {
			home := GinkgoT().TempDir()
			c2 := customEntry("c2", filepath.Join(home, "custom", "c2"), "https://api.c2.example:6443")
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "oc" {
					writeFile(c2.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs[0].Args).Should(Equal([]string{"login", "--server=https://api.c2.example:6443"}))
		})

		It("should create the kubeconfig parent directory before oc login", func() {
			home := GinkgoT().TempDir()
			path := filepath.Join(home, "missing", "dir", "config-c2")
			c2 := customEntry("c2", path, "https://api.c2.example:6443")
			var dirMode os.FileMode
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "oc" {
					info, err := os.Stat(filepath.Dir(path))
					Expect(err).ShouldNot(HaveOccurred())
					dirMode = info.Mode().Perm()
					writeFile(path)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c2})
			Expect(failures).Should(BeEmpty())
			Expect(dirMode).Should(Equal(os.FileMode(0o700)))
		})

		It("should log the oc login command with its KUBECONFIG", func() {
			home := GinkgoT().TempDir()
			c2 := customEntry("c2", filepath.Join(home, "custom", "c2"), "https://api.c2.example:6443")
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "oc" {
					writeFile(c2.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{c2})
			Expect(failures).Should(BeEmpty())
			Expect(log.String()).Should(ContainSubstring("running oc login"))
			Expect(log.String()).Should(ContainSubstring("for cluster c2"))
			Expect(log.String()).Should(ContainSubstring("KUBECONFIG="))
			Expect(log.String()).Should(ContainSubstring(c2.Kubeconfig))
		})
	})

	When("the kflux login fails", func() {
		It("should still attempt oc login for the default-path cluster", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			c1.Server = "https://api.c1.example:6443"
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "/usr/local/bin/kflux" {
					return errors.New("exit status 1")
				}
				if spec.Name == "oc" {
					writeFile(c1.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(2))
			Expect(fake.runs[0].Name).Should(Equal("/usr/local/bin/kflux"))
			Expect(fake.runs[1].Name).Should(Equal("oc"))
			Expect(log.String()).Should(ContainSubstring("kflux cluster login failed"))
		})
	})

	When("kflux cannot be installed", func() {
		It("should fall back to oc login", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			c1.Server = "https://api.c1.example:6443"
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "git" {
					return errors.New("exit status 128")
				}
				if spec.Name == "oc" {
					writeFile(c1.Kubeconfig)
				}
				return nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(BeEmpty())
			Expect(log.String()).Should(ContainSubstring("kflux unavailable"))
			Expect(fake.runs).Should(HaveLen(2))
			Expect(fake.runs[1].Name).Should(Equal("oc"))
		})
	})

	When("both kflux and oc fail", func() {
		It("should report an authentication failure for that cluster", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(CommandSpec) error {
				return errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(HaveLen(1))
			Expect(failures["c1"]).Should(HaveOccurred())
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("authentication failed")))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring(filepath.Join(home, ".kube", "config-c1"))))
			// A kubeconfig that was never written is missing, not rejected; the
			// API server never saw it.
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("no readable kubeconfig")))
			Expect(failures["c1"]).ShouldNot(MatchError(ContainSubstring("was rejected")))
		})
	})

	When("the kubeconfig path is a directory", func() {
		It("should treat it as unauthenticated", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			Expect(os.MkdirAll(c1.Kubeconfig, 0o750)).To(Succeed())
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onRun = func(CommandSpec) error {
				return errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1})
			Expect(failures).Should(HaveKey("c1"))
		})
	})

	When("one cluster is unauthenticated and another is readable", func() {
		It("should report only the unauthenticated cluster", func() {
			home := GinkgoT().TempDir()
			c1 := defaultEntry("c1", home)
			c2 := defaultEntry("c2", home)
			writeFile(c1.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(CommandSpec) error {
				return errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{c1, c2})
			Expect(failures).Should(HaveLen(1))
			Expect(failures).Should(HaveKey("c2"))
		})
	})
})
