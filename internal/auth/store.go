package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const storePrefix = "lkmabt-auth-"

// Store holds run-scoped credentials in a private temporary directory.
type Store struct {
	directory string
}

// NewStore creates a private directory for run-scoped credentials. The directory
// is created with mode 0700 in the OS temporary directory and must be removed
// with Destroy.
func NewStore() (*Store, error) {
	directory, err := os.MkdirTemp("", storePrefix)
	if err != nil {
		return nil, fmt.Errorf("create temporary credential directory: %w", err)
	}
	return &Store{directory: directory}, nil
}

// Directory returns the directory holding the credentials, or an empty string
// for a nil store or one already destroyed.
func (s *Store) Directory() string {
	if s == nil {
		return ""
	}
	return s.directory
}

// Write stores a credential in a 0600 file, overwriting any existing value. It
// returns an error when the store is nil or not initialized.
func (s *Store) Write(name, value string) error {
	if s == nil || s.directory == "" {
		return fmt.Errorf("credential store is not initialized")
	}
	if err := os.WriteFile(filepath.Join(s.directory, name), []byte(value), 0o600); err != nil {
		return fmt.Errorf("store temporary credential %s: %w", name, err)
	}
	return nil
}

// Destroy recursively removes every stored credential and reports whether the
// directory is gone. It is safe to call on a nil or already destroyed store, and
// clears the directory so later Write calls fail.
func (s *Store) Destroy() error {
	if s == nil || s.directory == "" {
		return nil
	}
	if err := os.RemoveAll(s.directory); err != nil {
		return fmt.Errorf("delete temporary credentials in %s: %w", s.directory, err)
	}
	if _, err := os.Stat(s.directory); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("temporary credential directory %s is still present", s.directory)
	}
	s.directory = ""
	return nil
}
