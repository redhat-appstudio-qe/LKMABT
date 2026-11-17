package clusterauth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// writeScript writes an executable script fixture with mode 0o700.
func writeScript(dir, name, body string) string {
	path := filepath.Join(dir, name)
	Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
	return path
}

type fakeRunner struct {
	lookPaths map[string]string // name -> path, absent means not found
	runs      []CommandSpec
	outputs   []CommandSpec
	onRun     func(CommandSpec) error           // optional side effects / failures
	onOutput  func(CommandSpec) (string, error) // optional side effects / output
}

// LookPath resolves name against the fake PATH table.
func (r *fakeRunner) LookPath(name string) (string, error) {
	if path, found := r.lookPaths[name]; found && path != "" {
		return path, nil
	}
	return "", os.ErrNotExist
}

// Run records the spec and applies the optional failure hook.
func (r *fakeRunner) Run(_ context.Context, spec CommandSpec) error {
	r.runs = append(r.runs, spec)
	if r.onRun != nil {
		return r.onRun(spec)
	}
	return nil
}

// Output records the spec and applies the optional output hook.
func (r *fakeRunner) Output(_ context.Context, spec CommandSpec) (string, error) {
	r.outputs = append(r.outputs, spec)
	if r.onOutput != nil {
		return r.onOutput(spec)
	}
	return "", nil
}

var _ = Describe("ExecRunner.LookPath", func() {
	When("the binary is on PATH", func() {
		It("should return its path", func() {
			dir := GinkgoT().TempDir()
			writeScript(dir, "kflux", "#!/bin/sh\nexit 0\n")
			GinkgoT().Setenv("PATH", dir)

			path, err := (ExecRunner{}).LookPath("kflux")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(path).Should(Equal(filepath.Join(dir, "kflux")))
		})
	})

	When("the binary is absent", func() {
		It("should return an error", func() {
			GinkgoT().Setenv("PATH", GinkgoT().TempDir())

			_, err := (ExecRunner{}).LookPath("kflux")
			Expect(err).Should(HaveOccurred())
		})
	})
})

var _ = Describe("ExecRunner.Run", func() {
	When("the command succeeds", func() {
		It("should run it in Dir with extra environment", func() {
			dir := GinkgoT().TempDir()
			outPath := filepath.Join(dir, "out")
			probe := writeScript(dir, "probe", "#!/bin/sh\nprintf '%s|%s' \"$PWD\" \"$LKMABT_PROBE\" > \"$1\"\n")

			err := (ExecRunner{}).Run(context.Background(), CommandSpec{Name: probe, Args: []string{outPath}, Dir: dir, Env: []string{"LKMABT_PROBE=value"}})
			Expect(err).ShouldNot(HaveOccurred())

			content, readErr := os.ReadFile(outPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal(dir + "|value"))
		})
	})

	When("the command fails", func() {
		It("should wrap the exit status with the command line", func() {
			err := (ExecRunner{}).Run(context.Background(), CommandSpec{Name: "sh", Args: []string{"-c", "exit 3"}})
			Expect(err).Should(MatchError(ContainSubstring("sh -c exit 3")))
			Expect(err).Should(MatchError(ContainSubstring("exit status 3")))
		})
	})

	When("a log writer is configured", func() {
		It("should duplicate stdout and stderr into it", func() {
			log := &bytes.Buffer{}

			err := (ExecRunner{Log: log}).Run(context.Background(), CommandSpec{Name: "sh", Args: []string{"-c", "printf out; printf err 1>&2"}})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(log.String()).Should(ContainSubstring("out"))
			Expect(log.String()).Should(ContainSubstring("err"))
		})
	})

	When("the context expires", func() {
		It("should kill the command", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			err := (ExecRunner{}).Run(ctx, CommandSpec{Name: "sh", Args: []string{"-c", "sleep 5"}})
			Expect(err).Should(HaveOccurred())
		})
	})
})

var _ = Describe("ExecRunner.Output", func() {
	When("the command succeeds", func() {
		It("should return stdout without touching the console", func() {
			output, err := (ExecRunner{}).Output(context.Background(), CommandSpec{Name: "sh", Args: []string{"-c", "printf whoami"}})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(output).Should(Equal("whoami"))
		})
	})

	When("the command fails", func() {
		It("should return its combined output with the error", func() {
			output, err := (ExecRunner{}).Output(context.Background(), CommandSpec{Name: "sh", Args: []string{"-c", "printf failed 1>&2; exit 1"}})
			Expect(err).Should(MatchError(ContainSubstring("sh -c")))
			Expect(output).Should(Equal("failed"))
		})
	})

	When("the context expires", func() {
		It("should kill the command and return the error", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			output, err := (ExecRunner{}).Output(ctx, CommandSpec{Name: "sh", Args: []string{"-c", "sleep 5"}})
			Expect(err).Should(HaveOccurred())
			Expect(output).Should(BeEmpty())
		})
	})
})
