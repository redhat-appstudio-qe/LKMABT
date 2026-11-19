package clusterauth

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// KfluxRepository is the kflux source repository cloned when PATH has no kflux.
	KfluxRepository = "https://gitlab.cee.redhat.com/hares/kflux.git"
	kfluxBinaryName = "kflux"
)

// kfluxSourceDir returns the checkout location for the kflux sources.
func kfluxSourceDir(home string) string {
	return filepath.Join(home, ".local", "share", "lkmabt", "kflux-src")
}

// kfluxBinaryPath returns the install location for the built kflux binary.
func kfluxBinaryPath(home string) string {
	return filepath.Join(home, ".local", "bin", kfluxBinaryName)
}

// ensureKflux returns a runnable kflux path. A kflux on PATH is used as-is;
// otherwise the source is cloned when absent, rebuilt on every call, and installed
// under ~/.local/bin.
func (l Ladder) ensureKflux(ctx context.Context) (string, error) {
	// An existing kflux on PATH is used as-is; no pinning, no upgrade.
	if path, err := l.Runner.LookPath(kfluxBinaryName); err == nil {
		return path, nil
	}
	source := kfluxSourceDir(l.home())
	if _, err := os.Stat(source); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
			return "", fmt.Errorf("create kflux source directory: %w", err)
		}
		if err := l.Runner.Run(ctx, CommandSpec{Name: "git", Args: []string{"clone", KfluxRepository, source}}); err != nil {
			return "", fmt.Errorf("clone kflux: %w", err)
		}
	}
	if err := l.Runner.Run(ctx, CommandSpec{Name: "make", Args: []string{"build"}, Dir: source}); err != nil {
		return "", fmt.Errorf("build kflux: %w", err)
	}
	target := kfluxBinaryPath(l.home())
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("create kflux bin directory: %w", err)
	}
	if err := installBinary(filepath.Join(source, "bin", kfluxBinaryName), target); err != nil {
		return "", err
	}
	return target, nil
}

// installBinary copies an executable to target with mode 0755, creating the
// target's parent directory.
func installBinary(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("read built kflux %s: %w", source, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create kflux bin directory: %w", err)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("install kflux to %s: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("install kflux to %s: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("install kflux to %s: %w", target, err)
	}
	return os.Chmod(target, 0o755)
}
