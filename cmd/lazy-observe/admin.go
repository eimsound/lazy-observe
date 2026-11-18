package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/secret"
	"github.com/spf13/cobra"
)

// adminResetPasswordTimeout bounds the direct metadata round trip; the CLI is
// an operator action, not a long-running service.
const adminResetPasswordTimeout = time.Minute

// cliAuditActor is the actor recorded for audit events created by CLI
// subcommands: there is no HTTP request and no authenticated session.
const cliAuditActor = "cli"

func newAdminCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "admin",
		Short: "Operate platform accounts without the HTTP API",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newAdminResetPasswordCommand(), newAdminResetKeyFingerprintCommand())
	return command
}

func newAdminResetPasswordCommand() *cobra.Command {
	var (
		username     string
		passwordFile string
	)
	configFlags := newConfigFlagSet("admin-reset-password")
	command := &cobra.Command{
		Use:   "reset-password",
		Short: "Replace an account password directly in the metadata database",
		Long: "Replace an account password directly in the metadata database and revoke every session of\n" +
			"that account. Only the admin account can use a generated password: it is written to\n" +
			"<data_dir>/initial-admin-password with mode 0600 and forced to change at the next sign-in.\n" +
			"Every other account, and any deployment that sets security.admin_password_file, must pass\n" +
			"--password-file. The plaintext is never printed.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, _, err := appconfig.LoadFlagSet(configFlags, os.LookupEnv)
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			return runAdminResetPassword(
				command.OutOrStdout(),
				command.ErrOrStderr(),
				cfg,
				username,
				passwordFile,
				account.ResetPassword,
			)
		},
	}
	command.Flags().StringVar(
		&username, "username", meta.BootstrapAdminUsername, "account whose password is replaced",
	)
	command.Flags().StringVar(
		&passwordFile,
		"password-file",
		"",
		"file holding the new password; omit to generate one for the admin account",
	)
	command.Flags().AddFlagSet(configFlags)
	return command
}

type adminResetOutcome struct {
	generatedPath string
	// generatedBefore is true when the generated path already held a file, so a
	// failed reset must not remove it.
	generatedBefore bool
	// previousContent is that file's content, read before the new password
	// overwrote it so a failure ahead of the hash write can put it back. It is
	// plaintext and never leaves the process except through the file.
	previousContent []byte
}

// resetPasswordFunc mirrors account.ResetPassword. It is a parameter rather
// than a direct call so the CLI tests can fail the reset at a chosen stage: a
// broken metadata store or session backend is not injectable from a test.
type resetPasswordFunc func(
	ctx context.Context,
	metadata account.PasswordResetStore,
	sessions account.Sessions,
	username, newPassword string,
	mustChangePassword bool,
) (meta.UserInfo, error)

func runAdminResetPassword(
	out, errOut io.Writer,
	cfg appconfig.Config,
	username, passwordFile string,
	resetPassword resetPasswordFunc,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminResetPasswordTimeout)
	defer cancel()

	// Everything that can fail without touching a file happens first: opening
	// the database, the session backend, and the account lookup. A typo in
	// --username must never overwrite the file that still holds the admin's
	// only working password.
	store, err := openMetadataStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()
	shared, err := openServerSharedState(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open shared session state: %w", err)
	}
	defer shared.Close()
	target, err := store.UserByUsername(ctx, username)
	if err != nil {
		if meta.IsNotFound(err) {
			recordResetAudit(ctx, store, errOut, username, meta.UserInfo{}, false, "not_found")
			return fmt.Errorf("user %q does not exist", username)
		}
		return fmt.Errorf("find user %q: %w", username, err)
	}

	outcome := adminResetOutcome{}
	password, err := resetPasswordSource(cfg, username, passwordFile, out, &outcome)
	if err != nil {
		return err
	}

	sessions := account.NewMetadataSessions(store)
	if cfg.Redis.Enabled {
		sessions = account.NewStateSessions(shared.sessions)
	}
	reset, err := resetPassword(ctx, store, sessions, username, password, outcome.generatedPath != "")
	if err != nil {
		reason := "reset_failed"
		if account.Stage(err) == account.StageSessionRevokeAfterReset {
			// The new password is stored: keep the generated file, it is the
			// only copy of credentials that now work.
			reason = "session_revoke_failed"
			recordResetAudit(ctx, store, errOut, username, target, false, reason)
			printGeneratedPath(out, username, outcome.generatedPath)
			return fmt.Errorf("password reset for %q; session revocation failed, rerun: %w", username, err)
		}
		if account.Code(err) == account.CodeNotFound {
			reason = "not_found"
		}
		recordResetAudit(ctx, store, errOut, username, target, false, reason)
		if restoreErr := restoreGeneratedPassword(outcome); restoreErr != nil {
			// The operator has to know the file no longer holds a password that
			// works, and that one more run replaces it.
			return fmt.Errorf(
				"reset password for %q: %w; %s now holds a password that never took effect and "+
					"restoring the previous content failed: %v; rerun this command",
				username,
				err,
				outcome.generatedPath,
				restoreErr,
			)
		}
		return fmt.Errorf("reset password for %q: %w", username, err)
	}
	// A reset exists to get past failed sign-ins, so the lockout counter must
	// go too. Only Redis keeps it where a second process can clear it.
	if cfg.Redis.Enabled {
		if err := account.ClearLoginFailures(ctx, shared.loginGate, username); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: clear login lockout for %s: %v\n", username, err)
		}
	}
	recordResetAudit(ctx, store, errOut, username, target, true, "")
	if outcome.generatedPath != "" {
		printGeneratedPath(out, username, outcome.generatedPath)
		return nil
	}
	_, err = fmt.Fprintf(out, "password reset for %s\n", reset.Username)
	return err
}

// resetPasswordSource picks the new password. Generated passwords are limited
// to the admin account and to deployments without security.admin_password_file:
// the generated path is the admin's bootstrap file, so another account must not
// take it over, and a deployer-owned file must never be overwritten.
func resetPasswordSource(
	cfg appconfig.Config,
	username, passwordFile string,
	out io.Writer,
	outcome *adminResetOutcome,
) (string, error) {
	if passwordFile != "" {
		return meta.ReadAdminPasswordFile(passwordFile)
	}
	if username != meta.BootstrapAdminUsername {
		return "", fmt.Errorf(
			"only the %q account can use a generated password; pass --password-file for %q",
			meta.BootstrapAdminUsername,
			username,
		)
	}
	generatedPath := gatewayInitialAdminPasswordFile(cfg)
	if cfg.Security.AdminPasswordFile != "" || generatedPath == "" {
		return "", fmt.Errorf(
			"security.admin_password_file is configured; pass --password-file instead of generating a password",
		)
	}
	outcome.generatedPath = generatedPath
	if _, err := os.Stat(generatedPath); err == nil {
		// The new password replaces the file before it is stored, so remember
		// the old bytes: a failure ahead of the hash write must leave the
		// operator with the credential that still works.
		previous, readErr := readPreviousGeneratedPassword(generatedPath)
		if readErr != nil {
			return "", readErr
		}
		outcome.generatedBefore = true
		outcome.previousContent = previous
		// Contents stay out of the output: only the operator who can read the
		// file learns the password it held.
		if _, err := fmt.Fprintf(out, "replacing existing %s\n", generatedPath); err != nil {
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect %s: %w", generatedPath, err)
	}
	password, err := meta.GenerateInitialAdminPassword(generatedPath)
	if err != nil {
		return "", fmt.Errorf("generate initial admin password: %w", err)
	}
	return password, nil
}

func printGeneratedPath(out io.Writer, username, path string) {
	if path == "" {
		return
	}
	_, _ = fmt.Fprintf(out, "initial admin password for %s written to %s\n", username, path)
}

// maxPreviousGeneratedPasswordBytes bounds the copy kept in memory of the file
// a failed reset restores. The file only ever holds a password, so anything
// larger is not something this CLI wrote and must not be silently replaced.
const maxPreviousGeneratedPasswordBytes = 4 << 10

// readPreviousGeneratedPassword copies the file that is about to be replaced.
// Plaintext stays in memory and goes back only into the same path.
func readPreviousGeneratedPassword(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxPreviousGeneratedPasswordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(content) > maxPreviousGeneratedPasswordBytes {
		return nil, fmt.Errorf(
			"%s is larger than %d bytes, so it is not a generated password; refusing to overwrite it",
			path,
			maxPreviousGeneratedPasswordBytes,
		)
	}
	return content, nil
}

// restoreGeneratedPassword undoes this run's write to the generated path. A
// file that existed before is restored from the copy taken ahead of the
// overwrite (a reset that failed before the hash write would otherwise leave a
// password nothing accepts behind); a file this run created is removed.
func restoreGeneratedPassword(outcome adminResetOutcome) error {
	if outcome.generatedPath == "" {
		return nil
	}
	if !outcome.generatedBefore {
		_ = os.Remove(outcome.generatedPath)
		return nil
	}
	return meta.WriteInitialAdminPasswordFile(outcome.generatedPath, outcome.previousContent)
}

func recordResetAudit(
	ctx context.Context,
	store *meta.Store,
	errOut io.Writer,
	username string,
	target meta.UserInfo,
	success bool,
	failureReason string,
) {
	record := meta.AuditRecord{
		Action:        "auth.password_reset",
		ActorUsername: cliAuditActor,
		TargetType:    "user",
		// The zero target means the account was not found: the HTTP password
		// paths fill these from the same user row, and an unknown name has none.
		TargetID:      target.ID,
		TargetName:    username,
		TargetRole:    target.Role,
		ChangedFields: "password",
		Success:       success,
		FailureReason: failureReason,
	}
	if err := store.RecordAudit(ctx, record); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: record password reset audit event: %v\n", err)
	}
}

// newAdminResetKeyFingerprintCommand is the only way past a platform key
// mismatch. Startup never rotates the recorded fingerprint on its own: an
// operator confirms the rotation here, accepting that values sealed with the
// previous key can no longer be decrypted.
func newAdminResetKeyFingerprintCommand() *cobra.Command {
	configFlags := newConfigFlagSet("admin-reset-key-fingerprint")
	command := &cobra.Command{
		Use:   "reset-key-fingerprint",
		Short: "Record the configured platform encryption key as the new fingerprint",
		Long: "Record the fingerprint of the configured platform encryption key (security.secret_key_file)\n" +
			"in the metadata database, replacing the recorded one. Run this only to confirm an intentional\n" +
			"rotation: values sealed with the previous key, including the AI provider API key, can no longer\n" +
			"be decrypted and have to be entered again. Stop every replica before rotating: a running replica\n" +
			"keeps sealing with the key it loaded at startup. The key file must already exist and is only\n" +
			"read, never created or rewritten.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, _, err := appconfig.LoadFlagSet(configFlags, os.LookupEnv)
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			return runAdminResetKeyFingerprint(command.OutOrStdout(), command.ErrOrStderr(), cfg)
		},
	}
	command.Flags().AddFlagSet(configFlags)
	return command
}

func runAdminResetKeyFingerprint(out, errOut io.Writer, cfg appconfig.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminResetPasswordTimeout)
	defer cancel()

	// secret.Load never creates a key: a mistyped path or a Secret that was not
	// mounted must fail here instead of recording a fingerprint that no real
	// replica can match.
	keyFile := secretKeyFile(cfg)
	box, err := secret.Load(keyFile)
	if err != nil {
		return fmt.Errorf(
			"load platform encryption key: %w (mount the key file the server reads via "+
				"security.secret_key_file and retry; this command never creates a key)",
			err,
		)
	}
	store, err := openMetadataStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	previous, hadPrevious, err := store.RecordedPlatformKeyFingerprint(ctx)
	if err != nil {
		recordPlatformKeyResetAudit(ctx, store, errOut, box.Fingerprint(), false, "read_failed")
		return fmt.Errorf("read recorded platform key fingerprint: %w", err)
	}
	previousText := "none"
	if hadPrevious {
		previousText = previous.Fingerprint
	}
	if hadPrevious && previous.Fingerprint == box.Fingerprint() {
		_, err := fmt.Fprintf(
			out,
			"platform encryption key fingerprint already recorded for %s: %s; nothing to do\n"+
				"stop every replica before rotating a key: a running replica keeps sealing with the key it "+
				"loaded at startup\n",
			keyFile,
			box.Fingerprint(),
		)
		return err
	}
	recorded, err := store.ReplacePlatformKeyFingerprint(ctx, box.Fingerprint())
	if err != nil {
		recordPlatformKeyResetAudit(ctx, store, errOut, box.Fingerprint(), false, "replace_failed")
		return fmt.Errorf("record platform key fingerprint: %w", err)
	}
	recordPlatformKeyResetAudit(ctx, store, errOut, recorded.Fingerprint, true, "")
	if !hadPrevious {
		// Nothing was sealed under a previous key, so the rotation warning
		// would only confuse an operator recording the first fingerprint.
		_, err = fmt.Fprintf(
			out,
			"platform encryption key fingerprint recorded for %s: %s (first record)\n",
			keyFile,
			recorded.Fingerprint,
		)
		return err
	}
	_, err = fmt.Fprintf(
		out,
		"platform encryption key fingerprint recorded for %s: %s (previous %s)\n"+
			"values sealed with the previous key can no longer be decrypted; re-enter the AI provider API key "+
			"in Settings > AI\n",
		keyFile,
		recorded.Fingerprint,
		previousText,
	)
	return err
}

// recordPlatformKeyResetAudit writes the rotation audit event. changed_fields
// follows the audit convention of comma-joined field names, so it only names
// the field; the audit row keeps the new fingerprint in target_name. There is
// no column for the value that was replaced, so the previous fingerprint is
// deliberately not stored here (it is printed to the operator instead).
func recordPlatformKeyResetAudit(
	ctx context.Context,
	store *meta.Store,
	errOut io.Writer,
	fingerprint string,
	success bool,
	failureReason string,
) {
	record := meta.AuditRecord{
		Action:        "security.key_fingerprint_reset",
		ActorUsername: cliAuditActor,
		TargetType:    "platform_key",
		TargetName:    fingerprint,
		ChangedFields: "fingerprint",
		Success:       success,
		FailureReason: failureReason,
	}
	if err := store.RecordAudit(ctx, record); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: record platform key fingerprint audit event: %v\n", err)
	}
}
