package meta

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InitialAdminPasswordFileName is the file under the data directory that
// receives a generated first admin password when the deployer did not supply
// one. It mirrors Jenkins' initialAdminPassword.
const InitialAdminPasswordFileName = "initial-admin-password"

// BootstrapAdminUsername is the account created on an empty metadata database
// and the default target of `lazy-observe admin reset-password`.
const BootstrapAdminUsername = "admin"

// minBootstrapAdminPasswordLength matches the account and management password
// policy (8 characters) so a deployer-provided password could also have been
// set through the UI.
const minBootstrapAdminPasswordLength = 8

// BootstrapAdmin selects where the first admin password comes from. It is only
// consulted while the admin account does not exist yet.
type BootstrapAdmin struct {
	// PasswordFile is a deployer-provided file holding the initial password.
	// Surrounding whitespace and line breaks are ignored.
	PasswordFile string
	// GeneratedPasswordFile receives a freshly generated password (mode 0600,
	// written atomically) when PasswordFile is empty.
	GeneratedPasswordFile string
}

func (a BootstrapAdmin) validate() error {
	if a.PasswordFile == "" && a.GeneratedPasswordFile == "" {
		return errors.New("bootstrap admin needs a password file or a generated password file path")
	}
	return nil
}

// ReadAdminPasswordFile reads a deployer-provided password. Only surrounding
// whitespace is trimmed: Kubernetes Secrets and `echo` output usually end with
// a newline, while inner spaces may be part of the password. Both bootstrap and
// `lazy-observe admin reset-password` use it so the two paths accept exactly
// the same passwords.
func ReadAdminPasswordFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read admin password file %s: %w", path, err)
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return "", fmt.Errorf("admin password file %s is empty", path)
	}
	if len(password) < minBootstrapAdminPasswordLength {
		return "", fmt.Errorf(
			"admin password file %s must contain at least %d characters",
			path,
			minBootstrapAdminPasswordLength,
		)
	}
	return password, nil
}

// writeInitialAdminPassword atomically persists a generated password with mode
// 0600: readers either see no file or the complete password.
func writeInitialAdminPassword(path, password string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create initial admin password directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create initial admin password file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict initial admin password file: %w", err)
	}
	if _, err := tmp.WriteString(password + "\n"); err != nil {
		return fmt.Errorf("write initial admin password file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync initial admin password file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close initial admin password file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("persist initial admin password file: %w", err)
	}
	return nil
}

// GenerateInitialAdminPassword writes a fresh random password to path (mode
// 0600, written atomically) and returns it so the caller can hash it. The
// plaintext only ever leaves this function to the account row and the file.
func GenerateInitialAdminPassword(path string) (string, error) {
	password, err := randomPassword()
	if err != nil {
		return "", err
	}
	if err := writeInitialAdminPassword(path, password); err != nil {
		return "", err
	}
	return password, nil
}

func bootstrapHost() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}
