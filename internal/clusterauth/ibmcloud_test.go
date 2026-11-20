package clusterauth

import (
	"bytes"
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// roksEntry builds a ROKS entry using the default kubeconfig path for home.
func roksEntry(name, home string) clusterfile.Entry {
	return clusterfile.Entry{
		Name:             name,
		Type:             clusterfile.TypeROKS,
		IBMCloud:         name,
		IBMCloudEndpoint: clusterfile.DefaultIBMCloudEndpoint,
		Kubeconfig:       clusterfile.DefaultKubeconfig(home, name),
	}
}

// ksConfigSpec builds the expected ibmcloud ks cluster config spec for entry.
func ksConfigSpec(binary string, entry clusterfile.Entry) CommandSpec {
	return CommandSpec{
		Name: binary,
		Args: []string{"ks", "cluster", "config", "--cluster", entry.IBMCloud, "--endpoint", entry.IBMCloudEndpoint, "-q"},
		Env:  []string{"KUBECONFIG=" + entry.Kubeconfig},
	}
}

// whoamiSpec builds the expected oc whoami spec for entry.
func whoamiSpec(entry clusterfile.Entry) CommandSpec {
	return CommandSpec{Name: "oc", Args: []string{"whoami", "--request-timeout=30s"}, Env: []string{"KUBECONFIG=" + entry.Kubeconfig}}
}

var _ = Describe("Authenticate ROKS", func() {
	When("the kubeconfig already exists", func() {
		It("should verify it without the IBM Cloud login", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			writeFile(entry.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "IAM#user@redhat.com\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(BeEmpty())
			Expect(fake.outputs).Should(Equal([]CommandSpec{whoamiSpec(entry)}))
		})
	})

	When("the existing kubeconfig is rejected", func() {
		It("should refresh it through the IBM Cloud CLI and verify again", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			writeFile(entry.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(spec CommandSpec) error {
				writeFile(entry.Kubeconfig)
				return nil
			}
			whoamiCalls := 0
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					whoamiCalls++
					if whoamiCalls == 1 {
						return "error: You must be logged in to the server (Unauthorized)\n", errors.New("exit status 1")
					}
					return "IAM#user@redhat.com\n", nil
				}
				return "", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(Equal([]CommandSpec{ksConfigSpec("/usr/local/bin/ibmcloud", entry)}))
			Expect(fake.outputs).Should(Equal([]CommandSpec{
				whoamiSpec(entry),
				{Name: "/usr/local/bin/ibmcloud", Args: []string{"target", "-q"}},
				whoamiSpec(entry),
			}))
		})

		It("should name the missing refresh path when ibmcloud is unavailable", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			writeFile(entry.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "error: You must be logged in to the server (Unauthorized)\n", errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("existing kubeconfig was rejected")))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("ibmcloud CLI is not on PATH")))
			Expect(fake.runs).Should(BeEmpty())
			Expect(fake.outputs).Should(Equal([]CommandSpec{whoamiSpec(entry)}))
		})
	})

	When("the existing kubeconfig fails for another reason", func() {
		It("should fail without refreshing credentials", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			writeFile(entry.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "Unable to connect to the server: dial tcp 10.0.0.1:31533: i/o timeout\n", errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("dial tcp 10.0.0.1:31533")))
			Expect(fake.runs).Should(BeEmpty())
			Expect(fake.outputs).Should(Equal([]CommandSpec{whoamiSpec(entry)}))
		})
	})

	When("a non-interactive check times out", func() {
		It("should fail the stalled cluster and still authenticate the rest", func() {
			home := GinkgoT().TempDir()
			stalled := roksEntry("stalled", home)
			healthy := roksEntry("healthy", home)
			writeFile(stalled.Kubeconfig)
			writeFile(healthy.Kubeconfig)
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if strings.Contains(spec.Env[0], "stalled") {
					return "", context.DeadlineExceeded
				}
				return "IAM#user@redhat.com\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{stalled, healthy})
			Expect(failures).Should(HaveLen(1))
			Expect(failures).Should(HaveKey("stalled"))
			Expect(failures["stalled"]).Should(MatchError(ContainSubstring("oc whoami timed out after")))
			Expect(fake.outputs).Should(HaveLen(2))
		})

		It("should report a stalled ibmcloud session probe", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onOutput = func(CommandSpec) (string, error) {
				return "", context.DeadlineExceeded
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("ibmcloud target timed out after")))
			Expect(fake.runs).Should(BeEmpty())
		})

		It("should report a stalled kubeconfig download", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(CommandSpec) error { return context.DeadlineExceeded }
			fake.onOutput = func(CommandSpec) (string, error) { return "", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("ibmcloud ks cluster config timed out after")))
			// Only the session probe reached Output; no whoami ran.
			Expect(fake.outputs).Should(HaveLen(1))
		})
	})

	When("the ibmcloud CLI is missing", func() {
		It("should fail without falling through to kflux or oc login", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("ibmcloud CLI is not on PATH")))
			Expect(fake.runs).Should(BeEmpty())
			Expect(fake.outputs).Should(BeEmpty())
		})
	})

	When("an ibmcloud session is already active", func() {
		It("should skip SSO and write the entry's kubeconfig", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			entry.IBMCloud = "cluster-id"
			entry.IBMCloudEndpoint = "private"
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(spec CommandSpec) error {
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					return "IAM#user@redhat.com\n", nil
				}
				return "API endpoint: https://cloud.ibm.com\n", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0]).Should(Equal(ksConfigSpec("/usr/local/bin/ibmcloud", entry)))
			Expect(fake.outputs).Should(HaveLen(2))
			Expect(fake.outputs[0]).Should(Equal(CommandSpec{Name: "/usr/local/bin/ibmcloud", Args: []string{"target", "-q"}}))
			Expect(fake.outputs[1]).Should(Equal(whoamiSpec(entry)))
			Expect(log.String()).Should(ContainSubstring("authenticated as IAM#user@redhat.com"))
		})
	})

	When("no ibmcloud session is active", func() {
		It("should run SSO once for every ROKS cluster", func() {
			home := GinkgoT().TempDir()
			c1 := roksEntry("c1", home)
			c2 := roksEntry("c2", home)
			log := &bytes.Buffer{}
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Args[0] == "ks" {
					writeFile(strings.TrimPrefix(spec.Env[0], "KUBECONFIG="))
				}
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					return "IAM#user@redhat.com\n", nil
				}
				return "Not logged in. Use 'ibmcloud login' to log in.", errors.New("exit status 1")
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: log}).Authenticate(context.Background(), []clusterfile.Entry{c1, c2})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(3))
			Expect(fake.runs[0]).Should(Equal(CommandSpec{Name: "/usr/local/bin/ibmcloud", Args: []string{"login", "--sso"}}))
			Expect(fake.runs[1]).Should(Equal(ksConfigSpec("/usr/local/bin/ibmcloud", c1)))
			Expect(fake.runs[2]).Should(Equal(ksConfigSpec("/usr/local/bin/ibmcloud", c2)))
			Expect(log.String()).Should(ContainSubstring("login --sso"))
		})
	})

	When("ibmcloud cannot write the kubeconfig", func() {
		It("should report the cluster without running oc whoami", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(CommandSpec) error { return errors.New("exit status 1") }
			fake.onOutput = func(CommandSpec) (string, error) { return "", nil }

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("ibmcloud ks cluster config")))
			Expect(fake.outputs).Should(HaveLen(1))
		})
	})

	When("the API server rejects the written kubeconfig", func() {
		It("should explain the first-time console login instead of retrying with oc login", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(spec CommandSpec) error {
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					return "error: You must be logged in to the server (Unauthorized)\n", errors.New("exit status 1")
				}
				return "", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("first-time login")))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("IBM Cloud console")))
			// The kubeconfig exists on disk, so only the verification result can fail the cluster.
			Expect(readableFile(entry.Kubeconfig)).Should(BeTrue())
			Expect(fake.runs).Should(HaveLen(1))
		})
	})

	When("oc whoami fails for another reason", func() {
		It("should keep the underlying output in the failure", func() {
			home := GinkgoT().TempDir()
			entry := roksEntry("c1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud"}}
			fake.onRun = func(spec CommandSpec) error {
				writeFile(entry.Kubeconfig)
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					return "Unable to connect to the server: dial tcp 10.0.0.1:31533: i/o timeout\n", errors.New("exit status 1")
				}
				return "", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{entry})
			Expect(failures).Should(HaveKey("c1"))
			Expect(failures["c1"]).Should(MatchError(ContainSubstring("dial tcp 10.0.0.1:31533")))
			Expect(failures["c1"]).ShouldNot(MatchError(ContainSubstring("first-time login")))
		})
	})

	When("ROKS and kflux clusters are listed together", func() {
		It("should keep the ROKS entry out of the kflux login", func() {
			home := GinkgoT().TempDir()
			roks := roksEntry("roks1", home)
			legacy := defaultEntry("legacy1", home)
			fake := &fakeRunner{lookPaths: map[string]string{"ibmcloud": "/usr/local/bin/ibmcloud", "kflux": "/usr/local/bin/kflux"}}
			fake.onRun = func(spec CommandSpec) error {
				switch spec.Name {
				case "/usr/local/bin/ibmcloud":
					writeFile(roks.Kubeconfig)
				case "/usr/local/bin/kflux":
					writeFile(legacy.Kubeconfig)
				}
				return nil
			}
			fake.onOutput = func(spec CommandSpec) (string, error) {
				if spec.Name == "oc" {
					return "IAM#user@redhat.com\n", nil
				}
				return "", nil
			}

			_, failures := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).Authenticate(context.Background(), []clusterfile.Entry{roks, legacy})
			Expect(failures).Should(BeEmpty())
			Expect(fake.runs).Should(HaveLen(2))
			Expect(fake.runs[0]).Should(Equal(ksConfigSpec("/usr/local/bin/ibmcloud", roks)))
			Expect(fake.runs[1]).Should(Equal(CommandSpec{Name: "/usr/local/bin/kflux", Args: []string{"cluster", "login", "-c", "legacy1"}}))
		})
	})
})
