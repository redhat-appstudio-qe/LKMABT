package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/filelock"
)

// tenantLockDirectory resolves the directory holding cross-process tenant
// namespace locks. Tests replace it so lock files stay in a temporary directory.
var tenantLockDirectory = defaultTenantLockDirectory

// defaultTenantLockDirectory returns <user cache>/lkmabt/locks.
func defaultTenantLockDirectory() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve the user cache directory for tenant namespace locks: %w", err)
	}
	return filepath.Join(base, "lkmabt", "locks"), nil
}

// tenantLockPath returns the lock file that guards namespace on server.
func tenantLockPath(server, namespace string) (string, error) {
	root, err := tenantLockDirectory()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(serverIdentity(server) + "\x00" + namespace))
	return filepath.Join(root, hex.EncodeToString(sum[:])+".lock"), nil
}

// acquireTenantNamespaceLock reserves namespace on server for this process and
// returns its release function. A contended acquisition names the namespace,
// server, and recorded holder, so two LKMABT processes cannot share a tenant
// namespace and delete each other's resources.
func acquireTenantNamespaceLock(server, namespace, holder string) (func(), error) {
	lockPath, err := tenantLockPath(server, namespace)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create tenant namespace lock directory: %w", err)
	}
	release, err := filelock.TryAcquire(lockPath)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, tenantNamespaceContention(server, namespace, lockPath)
	}
	if err != nil {
		return nil, fmt.Errorf("lock tenant namespace %s: %w", namespace, err)
	}
	if err := os.WriteFile(lockPath, []byte(holder+"\n"), 0o600); err != nil {
		release()
		return nil, fmt.Errorf("record the holder of tenant namespace %s: %w", namespace, err)
	}
	return release, nil
}

// tenantNamespaceContention names the recorded holder, degrading when the lock file is unreadable.
func tenantNamespaceContention(server, namespace, lockPath string) error {
	data, err := os.ReadFile(lockPath)
	holder := strings.TrimSpace(string(data))
	if err != nil || holder == "" {
		return fmt.Errorf("tenant namespace %s on server %s is locked by another LKMABT process", namespace, tenantLockServerLabel(server))
	}
	return fmt.Errorf("tenant namespace %s on server %s is in use by %s; wait for that run or choose another namespace", namespace, tenantLockServerLabel(server), holder)
}

// tenantLockHolder describes this process in lock-contention messages.
func tenantLockHolder(runID string, started time.Time) string {
	return fmt.Sprintf("run %s (pid %d, started %s)", runID, os.Getpid(), started.UTC().Format(time.RFC3339))
}

// tenantLockServer returns the API server that identifies the tenant namespace
// lock, falling back to the connected server when the request carries none.
func tenantLockServer(configured string, clients *cluster.ClientSet) string {
	if normalizeServer(configured) != "" {
		return configured
	}
	if clients != nil && clients.Config != nil {
		return clients.Config.Host
	}
	return ""
}

// tenantLockServerLabel renders a server for operator-facing messages.
func tenantLockServerLabel(server string) string {
	if normalized := normalizeServer(server); normalized != "" {
		return normalized
	}
	return "the default cluster"
}
