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
	command.AddCommand(newAdminResetPasswordCommand())
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
			return runAdminResetPassword(command.OutOrStdout(), command.ErrOrStderr(), cfg, username, passwordFile)
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
}

func runAdminResetPassword(
	out, errOut io.Writer,
	cfg appconfig.Config,
	username, passwordFile string,
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
	if _, err := store.UserByUsername(ctx, username); err != nil {
		if meta.IsNotFound(err) {
			recordResetAudit(ctx, store, errOut, username, false, "not_found")
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
	reset, err := account.ResetPassword(
		ctx, store, sessions, username, password, outcome.generatedPath != "",
	)
	if err != nil {
		reason := "reset_failed"
		if account.Stage(err) == account.StageSessionRevokeAfterReset {
			// The new password is stored: keep the generated file, it is the
			// only copy of credentials that now work.
			reason = "session_revoke_failed"
			recordResetAudit(ctx, store, errOut, username, false, reason)
			printGeneratedPath(out, username, outcome.generatedPath)
			return fmt.Errorf("password reset for %q; session revocation failed, rerun: %w", username, err)
		}
		if account.Code(err) == account.CodeNotFound {
			reason = "not_found"
		}
		recordResetAudit(ctx, store, errOut, username, false, reason)
		cleanupGeneratedPassword(outcome)
		return fmt.Errorf("reset password for %q: %w", username, err)
	}
	// A reset exists to get past failed sign-ins, so the lockout counter must
	// go too. Only Redis keeps it where a second process can clear it.
	if cfg.Redis.Enabled {
		if err := account.ClearLoginFailures(ctx, shared.loginGate, username); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: clear login lockout for %s: %v\n", username, err)
		}
	}
	recordResetAudit(ctx, store, errOut, username, true, "")
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
		outcome.generatedBefore = true
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

// cleanupGeneratedPassword removes the generated file only when this run
// created it. A file that existed before is left in place: removing it would
// destroy a credential the operator had before the failed reset.
func cleanupGeneratedPassword(outcome adminResetOutcome) {
	if outcome.generatedPath == "" || outcome.generatedBefore {
		return
	}
	_ = os.Remove(outcome.generatedPath)
}

func recordResetAudit(
	ctx context.Context,
	store *meta.Store,
	errOut io.Writer,
	username string,
	success bool,
	failureReason string,
) {
	record := meta.AuditRecord{
		Action:        "auth.password_reset",
		ActorUsername: cliAuditActor,
		TargetType:    "user",
		TargetName:    username,
		ChangedFields: "password",
		Success:       success,
		FailureReason: failureReason,
	}
	if err := store.RecordAudit(ctx, record); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: record password reset audit event: %v\n", err)
	}
}
