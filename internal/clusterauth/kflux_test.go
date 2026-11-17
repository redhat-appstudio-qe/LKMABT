package clusterauth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// kfluxSourcePath returns the source-tree layout path for home.
func kfluxSourcePath(home string) string {
	return filepath.Join(home, ".local", "share", "lkmabt", "kflux-src")
}

// kfluxInstallPath returns the install target for home.
func kfluxInstallPath(home string) string {
	return filepath.Join(home, ".local", "bin", "kflux")
}

// seedSourceBinary writes the fake built binary into the source tree.
func seedSourceBinary(source string) {
	Expect(os.MkdirAll(filepath.Join(source, "bin"), 0o750)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(source, "bin", "kflux"), []byte("binary"), 0o755)).To(Succeed())
}

var _ = Describe("ensureKflux", func() {
	When("kflux is on PATH", func() {
		It("should return it without cloning or building", func() {
			fake := &fakeRunner{lookPaths: map[string]string{"kflux": "/usr/local/bin/kflux"}}

			path, err := (Ladder{Runner: fake, Home: GinkgoT().TempDir(), Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(path).Should(Equal("/usr/local/bin/kflux"))
			Expect(fake.runs).Should(BeEmpty())
		})
	})

	When("the source tree exists", func() {
		It("should build and install without cloning", func() {
			home := GinkgoT().TempDir()
			source := kfluxSourcePath(home)
			seedSourceBinary(source)
			fake := &fakeRunner{lookPaths: map[string]string{}}

			path, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(path).Should(Equal(kfluxInstallPath(home)))
			Expect(fake.runs).Should(HaveLen(1))
			Expect(fake.runs[0].Name).Should(Equal("make"))
			Expect(fake.runs[0].Args).Should(Equal([]string{"build"}))
			Expect(fake.runs[0].Dir).Should(Equal(source))
		})

		It("should install the binary with mode 0755 and identical content", func() {
			home := GinkgoT().TempDir()
			source := kfluxSourcePath(home)
			seedSourceBinary(source)
			fake := &fakeRunner{lookPaths: map[string]string{}}

			_, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).ShouldNot(HaveOccurred())

			info, statErr := os.Stat(kfluxInstallPath(home))
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o755)))
			content, readErr := os.ReadFile(kfluxInstallPath(home))
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("binary"))
		})
	})

	When("the source tree is missing", func() {
		It("should clone into the LKMABT share directory", func() {
			home := GinkgoT().TempDir()
			source := kfluxSourcePath(home)
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "git" {
					seedSourceBinary(source)
				}
				return nil
			}

			path, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(fake.runs).Should(HaveLen(2))
			Expect(fake.runs[0].Name).Should(Equal("git"))
			Expect(fake.runs[0].Args).Should(Equal([]string{"clone", KfluxRepository, source}))
			Expect(fake.runs[0].Dir).Should(Equal(""))
			Expect(fake.runs[1].Name).Should(Equal("make"))
			Expect(path).Should(Equal(kfluxInstallPath(home)))
		})
	})

	When("the clone fails", func() {
		It("should report the clone failure and not install", func() {
			home := GinkgoT().TempDir()
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "git" {
					return errors.New("exit status 128")
				}
				return nil
			}

			_, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).Should(MatchError(ContainSubstring("clone kflux")))
			_, statErr := os.Stat(kfluxInstallPath(home))
			Expect(statErr).Should(HaveOccurred())
		})
	})

	When("the build fails", func() {
		It("should report the build failure", func() {
			home := GinkgoT().TempDir()
			seedSourceBinary(kfluxSourcePath(home))
			fake := &fakeRunner{lookPaths: map[string]string{}}
			fake.onRun = func(spec CommandSpec) error {
				if spec.Name == "make" {
					return errors.New("exit status 2")
				}
				return nil
			}

			_, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).Should(MatchError(ContainSubstring("build kflux")))
		})
	})

	When("the built binary is missing", func() {
		It("should report the read failure", func() {
			home := GinkgoT().TempDir()
			source := kfluxSourcePath(home)
			Expect(os.MkdirAll(source, 0o750)).To(Succeed())
			fake := &fakeRunner{lookPaths: map[string]string{}}

			_, err := (Ladder{Runner: fake, Home: home, Log: &bytes.Buffer{}}).ensureKflux(context.Background())
			Expect(err).Should(MatchError(ContainSubstring("read built kflux")))
		})
	})
})

var _ = Describe("installBinary", func() {
	When("the source is absent", func() {
		It("should fail", func() {
			home := GinkgoT().TempDir()

			err := installBinary(filepath.Join(home, "absent"), filepath.Join(home, "bin", "target"))
			Expect(err).Should(MatchError(ContainSubstring("read built kflux")))
		})
	})

	When("the target directory does not exist", func() {
		It("should create the install directory", func() {
			home := GinkgoT().TempDir()
			source := filepath.Join(home, "source")
			Expect(os.WriteFile(source, []byte("binary"), 0o755)).To(Succeed())
			target := filepath.Join(home, "deep", "bin", "kflux")

			Expect(installBinary(source, target)).Should(Succeed())
			Expect(target).Should(BeAnExistingFile())
		})
	})
})
