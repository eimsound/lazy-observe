package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

// adminResetFixture seeds a metadata database the way a first start would and
// returns the data directory, the metadata path, and the seeded admin ID.
func adminResetFixture(t *testing.T, extraUsers ...string) (string, string, int) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "meta", "lazy-observe.db")
	store, err := meta.OpenStore(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.CreateUser(context.Background(), "admin", "old-admin-password", "admin")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	sessions := account.NewMetadataSessions(store)
	if _, err := sessions.Create(context.Background(), admin.ID, time.Now(), time.Now().Add(time.Hour)); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	for _, username := range extraUsers {
		if _, err := store.CreateUser(
			context.Background(), username, "old-"+username+"-password", "member",
		); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir, dbPath, admin.ID
}

// writeAdminConfig writes a minimal YAML configuration: security.
// admin_password_file has no CLI flag, so the tests that need it use a file.
func writeAdminConfig(t *testing.T, dataDir, adminPasswordFile string) string {
	t.Helper()
	content := "data_dir: " + strconv.Quote(dataDir) + "\n"
	if adminPasswordFile != "" {
		content += "security:\n  admin_password_file: " + strconv.Quote(adminPasswordFile) + "\n"
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runAdminReset(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := newRootCommand()
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(append([]string{"admin", "reset-password"}, args...))
	err := command.Execute()
	return output.String(), err
}

// failingReset replaces account.ResetPassword with a failure at stage. The CLI
// only learns about a failed session revocation or hash write through this
// return value, so the test can reach both error paths without a broken
// metadata database.
func failingReset(stage string, onFail func()) resetPasswordFunc {
	return func(
		context.Context, account.PasswordResetStore, account.Sessions, string, string, bool,
	) (meta.UserInfo, error) {
		if onFail != nil {
			onFail()
		}
		return meta.UserInfo{}, &account.Error{
			Code: account.CodeUnavailable, Stage: stage, Cause: errors.New("injected failure"),
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openResetResultStore(t *testing.T, dbPath string) *meta.Store {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// A generated password replaces the file before it is stored. When the reset
// fails first (the session revocation or the hash write), the file must come
// back byte for byte: it is the operator's only working credential.
func TestAdminResetPasswordRestoresReplacedFileWhenResetFails(t *testing.T) {
	const previousContent = "bootstrap-admin-password\n"
	for _, stage := range []string{"session_revoke", "reset_password"} {
		t.Run(stage, func(t *testing.T) {
			dataDir, dbPath, _ := adminResetFixture(t)
			generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
			writeFile(t, generatedPath, previousContent)

			cfg := appconfig.Defaults()
			cfg.DataDir = dataDir
			var output bytes.Buffer
			err := runAdminResetPassword(&output, &output, cfg, "admin", "", failingReset(stage, nil))
			if err == nil {
				t.Fatal("reset reported success")
			}
			if strings.Contains(output.String(), "bootstrap-admin-password") {
				t.Fatalf("output leaked the previous password: %q", output.String())
			}

			raw, readErr := os.ReadFile(generatedPath)
			if readErr != nil {
				t.Fatalf("initial admin password file disappeared: %v", readErr)
			}
			if string(raw) != previousContent {
				t.Fatalf("file content = %q, want the previous password restored", raw)
			}
			info, statErr := os.Stat(generatedPath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("restored file mode = %o, want 600", info.Mode().Perm())
			}

			// Nothing was stored, so the old credential still works.
			store := openResetResultStore(t, dbPath)
			if _, authErr := store.AuthenticateCredentials(
				context.Background(), "admin", "old-admin-password",
			); authErr != nil {
				t.Fatalf("a failed reset changed the admin password: %v", authErr)
			}
		})
	}
}

// A file this run created is removed again when the reset fails, as before.
func TestAdminResetPasswordRemovesFileItCreatedWhenResetFails(t *testing.T) {
	dataDir, _, _ := adminResetFixture(t)
	cfg := appconfig.Defaults()
	cfg.DataDir = dataDir
	var output bytes.Buffer
	if err := runAdminResetPassword(
		&output, &output, cfg, "admin", "", failingReset("session_revoke", nil),
	); err == nil {
		t.Fatal("reset reported success")
	}
	if _, err := os.Stat(filepath.Join(dataDir, meta.InitialAdminPasswordFileName)); !os.IsNotExist(err) {
		t.Fatalf("a failed reset left its generated file behind: %v", err)
	}
}

// The second session revocation happens after the hash is stored, so the new
// password already works: the file must keep it instead of rolling back.
func TestAdminResetPasswordKeepsNewFileWhenSessionRevokeAfterResetFails(t *testing.T) {
	dataDir, _, adminID := adminResetFixture(t)
	generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
	const previousContent = "bootstrap-admin-password\n"
	writeFile(t, generatedPath, previousContent)

	reset := func(
		context.Context, account.PasswordResetStore, account.Sessions, string, string, bool,
	) (meta.UserInfo, error) {
		return meta.UserInfo{ID: adminID, Username: "admin", Role: "admin"}, &account.Error{
			Code:  account.CodeUnavailable,
			Stage: account.StageSessionRevokeAfterReset,
			Cause: errors.New("injected failure"),
		}
	}
	cfg := appconfig.Defaults()
	cfg.DataDir = dataDir
	var output bytes.Buffer
	err := runAdminResetPassword(&output, &output, cfg, "admin", "", reset)
	if err == nil {
		t.Fatal("reset reported success")
	}
	if !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("error = %v, want a rerun hint", err)
	}
	if !strings.Contains(output.String(), generatedPath) {
		t.Fatalf("output %q does not name the file that holds the working password", output.String())
	}
	raw, readErr := os.ReadFile(generatedPath)
	if readErr != nil {
		t.Fatalf("the applied password file disappeared: %v", readErr)
	}
	if string(raw) == previousContent {
		t.Fatal("the applied password file was rolled back to a password nothing accepts")
	}
	if strings.Contains(output.String(), strings.TrimSpace(string(raw))) {
		t.Fatalf("output leaked the new password: %q", output.String())
	}
	info, statErr := os.Stat(generatedPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode = %o, want 600", info.Mode().Perm())
	}
}

// When the old content cannot be put back, the operator must learn that the
// file no longer holds a working password and that a rerun fixes it.
func TestAdminResetPasswordReportsUnrestorableOverwrite(t *testing.T) {
	dataDir, _, _ := adminResetFixture(t)
	generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
	writeFile(t, generatedPath, "bootstrap-admin-password\n")

	blockRestore := func() {
		// A directory at the path makes the atomic rename back fail.
		if err := os.Remove(generatedPath); err != nil {
			t.Fatalf("remove generated file: %v", err)
		}
		if err := os.Mkdir(generatedPath, 0o700); err != nil {
			t.Fatalf("replace generated file with a directory: %v", err)
		}
	}
	cfg := appconfig.Defaults()
	cfg.DataDir = dataDir
	var output bytes.Buffer
	err := runAdminResetPassword(
		&output, &output, cfg, "admin", "", failingReset("session_revoke", blockRestore),
	)
	if err == nil {
		t.Fatal("reset reported success")
	}
	if !strings.Contains(err.Error(), "never took effect") || !strings.Contains(err.Error(), generatedPath) {
		t.Fatalf("error = %v, want it to name %s and say the password never took effect", err, generatedPath)
	}
	if strings.Contains(err.Error(), "bootstrap-admin-password") {
		t.Fatalf("error leaked the previous password: %v", err)
	}
}

func TestAdminResetPasswordGeneratesInitialPasswordFile(t *testing.T) {
	dataDir, dbPath, adminID := adminResetFixture(t)
	output, err := runAdminReset(t, "--data-dir", dataDir)
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}

	path := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("generated initial admin password file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("generated file mode = %o, want 600", info.Mode().Perm())
	}
	if !strings.Contains(output, path) {
		t.Fatalf("stdout %q does not name the generated file %q", output, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(raw))
	if len(password) < 8 {
		t.Fatalf("generated password length = %d", len(password))
	}
	if strings.Contains(output, password) {
		t.Fatalf("stdout leaked the generated password: %q", output)
	}

	store := openResetResultStore(t, dbPath)
	if _, err := store.AuthenticateCredentials(context.Background(), "admin", "old-admin-password"); err == nil {
		t.Fatal("old admin password still authenticates after reset")
	}
	reset, err := store.AuthenticateCredentials(context.Background(), "admin", password)
	if err != nil {
		t.Fatalf("generated password rejected: %v", err)
	}
	if !reset.MustChangePassword {
		t.Fatal("a generated password must force a change at the next sign-in")
	}
	sessions, err := account.NewMetadataSessions(store).List(context.Background(), adminID, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions after reset = %d, want 0", len(sessions))
	}
}

func TestAdminResetPasswordUsesPasswordFile(t *testing.T) {
	dataDir, dbPath, _ := adminResetFixture(t)
	passwordFile := filepath.Join(t.TempDir(), "admin-password")
	const supplied = "deployer-chosen-password"
	if err := os.WriteFile(passwordFile, []byte("  "+supplied+"\r\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	output, err := runAdminReset(t, "--data-dir", dataDir, "--password-file", passwordFile)
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}
	if strings.Contains(output, supplied) {
		t.Fatalf("stdout leaked the supplied password: %q", output)
	}
	if _, err := os.Stat(filepath.Join(dataDir, meta.InitialAdminPasswordFileName)); !os.IsNotExist(err) {
		t.Fatalf("--password-file must not write a generated file: %v", err)
	}
	raw, err := os.ReadFile(passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "  "+supplied+"\r\n" {
		t.Fatal("the reset CLI modified the deployer-provided password file")
	}

	store := openResetResultStore(t, dbPath)
	reset, err := store.AuthenticateCredentials(context.Background(), "admin", supplied)
	if err != nil {
		t.Fatalf("supplied password rejected: %v", err)
	}
	if reset.MustChangePassword {
		t.Fatal("a supplied password must not force a change")
	}
}

func TestAdminResetPasswordRejectsUnusablePasswordFile(t *testing.T) {
	dataDir, _, _ := adminResetFixture(t)
	passwordFile := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(passwordFile, []byte("short7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminReset(t, "--data-dir", dataDir, "--password-file", passwordFile); err == nil {
		t.Fatal("reset accepted a password file shorter than the policy")
	}
	if _, err := os.Stat(filepath.Join(dataDir, meta.InitialAdminPasswordFileName)); !os.IsNotExist(err) {
		t.Fatalf("a rejected reset wrote a generated file: %v", err)
	}
}

func TestAdminResetPasswordRejectsUnknownUser(t *testing.T) {
	dataDir, dbPath, _ := adminResetFixture(t)
	generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
	const bootstrapPassword = "bootstrap-admin-password"
	if err := os.WriteFile(generatedPath, []byte(bootstrapPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runAdminReset(t, "--data-dir", dataDir, "--username", "ghost")
	if err == nil {
		t.Fatal("reset accepted an unknown user")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the unknown user", err)
	}
	// A typo in --username must not overwrite the file that still holds the
	// admin's only working password.
	raw, readErr := os.ReadFile(generatedPath)
	if readErr != nil {
		t.Fatalf("existing initial admin password file disappeared: %v", readErr)
	}
	if string(raw) != bootstrapPassword+"\n" {
		t.Fatalf("existing file content = %q, want it untouched", raw)
	}
	store := openResetResultStore(t, dbPath)
	if _, err := store.AuthenticateCredentials(context.Background(), "admin", "old-admin-password"); err != nil {
		t.Fatalf("a failed reset changed the admin password: %v", err)
	}
	// There is no account to point at, so the audit event keeps its target
	// empty instead of naming the user that was typed in by mistake.
	events, listErr := store.ListAudit(context.Background(), meta.AuditListOptions{
		Action: "auth.password_reset", Limit: 10,
	})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if events[0].Success || events[0].TargetID != 0 || events[0].TargetRole != "" {
		t.Fatalf("not_found audit target = %#v, want success=false and no target", events[0])
	}
}

// Only the bootstrap admin account owns <data_dir>/initial-admin-password.
// Generating a password for any other account would overwrite the admin's
// working credential and leave that other plaintext on disk.
func TestAdminResetPasswordRefusesGeneratedPasswordForOtherUsers(t *testing.T) {
	dataDir, dbPath, _ := adminResetFixture(t, "bob")
	generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
	const bootstrapPassword = "bootstrap-admin-password"
	if err := os.WriteFile(generatedPath, []byte(bootstrapPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runAdminReset(t, "--data-dir", dataDir, "--username", "bob")
	if err == nil || !strings.Contains(err.Error(), "--password-file") {
		t.Fatalf("error = %v, want a --password-file hint", err)
	}
	raw, readErr := os.ReadFile(generatedPath)
	if readErr != nil || string(raw) != bootstrapPassword+"\n" {
		t.Fatalf("admin password file changed: %q err=%v", raw, readErr)
	}
	store := openResetResultStore(t, dbPath)
	if _, err := store.AuthenticateCredentials(context.Background(), "admin", bootstrapPassword); err == nil {
		t.Fatal("the generated file password unexpectedly opens admin (fixture drift)")
	}

	passwordFile := filepath.Join(t.TempDir(), "bob-password")
	if err := os.WriteFile(passwordFile, []byte("operator-chosen-bob-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminReset(
		t, "--data-dir", dataDir, "--username", "bob", "--password-file", passwordFile,
	); err != nil {
		t.Fatalf("explicit password file for a non-admin user: %v", err)
	}
	if _, err := store.AuthenticateCredentials(
		context.Background(), "bob", "operator-chosen-bob-password",
	); err != nil {
		t.Fatalf("bob's supplied password rejected: %v", err)
	}
	raw, readErr = os.ReadFile(generatedPath)
	if readErr != nil || string(raw) != bootstrapPassword+"\n" {
		t.Fatalf("admin password file changed by bob's reset: %q err=%v", raw, readErr)
	}
}

// A deployment that supplies security.admin_password_file owns that file; the
// CLI must not generate (and later delete) a password file on its own.
func TestAdminResetPasswordRefusesGeneratedPasswordWithDeployerFile(t *testing.T) {
	t.Run("same path", func(t *testing.T) {
		dataDir, _, _ := adminResetFixture(t)
		generatedPath := filepath.Join(dataDir, meta.InitialAdminPasswordFileName)
		const deployerPassword = "deployer-supplied-password"
		if err := os.WriteFile(generatedPath, []byte(deployerPassword+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		configPath := writeAdminConfig(t, dataDir, generatedPath)
		_, err := runAdminReset(t, "--config", configPath)
		if err == nil || !strings.Contains(err.Error(), "admin_password_file") {
			t.Fatalf("error = %v, want an admin_password_file refusal", err)
		}
		raw, readErr := os.ReadFile(generatedPath)
		if readErr != nil || string(raw) != deployerPassword+"\n" {
			t.Fatalf("deployer file changed: %q err=%v", raw, readErr)
		}
	})
	t.Run("different path", func(t *testing.T) {
		dataDir, _, _ := adminResetFixture(t)
		provided := filepath.Join(t.TempDir(), "admin-password")
		const deployerPassword = "deployer-supplied-password"
		if err := os.WriteFile(provided, []byte(deployerPassword+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		configPath := writeAdminConfig(t, dataDir, provided)
		_, err := runAdminReset(t, "--config", configPath)
		if err == nil || !strings.Contains(err.Error(), "admin_password_file") {
			t.Fatalf("error = %v, want an admin_password_file refusal", err)
		}
		if _, statErr := os.Stat(filepath.Join(dataDir, meta.InitialAdminPasswordFileName)); !os.IsNotExist(statErr) {
			t.Fatalf("refused reset wrote a generated file: %v", statErr)
		}
		raw, readErr := os.ReadFile(provided)
		if readErr != nil || string(raw) != deployerPassword+"\n" {
			t.Fatalf("deployer file changed: %q err=%v", raw, readErr)
		}
	})
}

// A reset exists to get past failed sign-ins. Redis keeps the lockout where a
// second process can clear it, so the CLI clears it too.
func TestAdminResetPasswordClearsRedisLoginLockout(t *testing.T) {
	ctx := context.Background()
	dataDir, dbPath, _ := adminResetFixture(t)
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	store := openResetResultStore(t, dbPath)
	manager := account.New(store, account.NewMetadataSessions(store), account.Options{
		LoginGate:         state.NewRedis(redisClient, "cli-reset"),
		Limits:            state.NewMemoryLimiter(),
		LoginFailureDelay: func() time.Duration { return 0 },
	})
	for range 5 {
		_, _ = manager.Login(ctx, account.LoginInput{
			Username: "admin", Password: "wrong-password", SourceIP: "10.0.0.1",
		})
	}
	if _, err := manager.Login(ctx, account.LoginInput{
		Username: "admin", Password: "old-admin-password", SourceIP: "10.0.0.2",
	}); account.Code(err) != account.CodeLockedOut {
		t.Fatalf("login before reset = %v, want locked_out", err)
	}

	output, err := runAdminReset(
		t,
		"--data-dir", dataDir,
		"--redis-enabled",
		"--redis-addr", redisServer.Addr(),
		"--redis-key-prefix", "cli-reset",
	)
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}
	if !strings.Contains(output, filepath.Join(dataDir, meta.InitialAdminPasswordFileName)) {
		t.Fatalf("stdout %q does not name the generated file", output)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, meta.InitialAdminPasswordFileName))
	if err != nil {
		t.Fatal(err)
	}
	reset, err := manager.Login(ctx, account.LoginInput{
		Username: "admin", Password: strings.TrimSpace(string(raw)), SourceIP: "10.0.0.3",
	})
	if err != nil {
		t.Fatalf("login after reset = %v, want success once the lockout is cleared", err)
	}
	if reset.Token == "" {
		t.Fatal("login after reset returned no session token")
	}
}

// Password resets from the CLI are audited like the HTTP ones, with no session
// to identify the operator.
func TestAdminResetPasswordRecordsAuditEvent(t *testing.T) {
	dataDir, dbPath, adminID := adminResetFixture(t)
	if _, err := runAdminReset(t, "--data-dir", dataDir); err != nil {
		t.Fatalf("reset password: %v", err)
	}
	store := openResetResultStore(t, dbPath)
	events, err := store.ListAudit(context.Background(), meta.AuditListOptions{
		Action: "auth.password_reset", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	event := events[0]
	if !event.Success || event.ActorUsername != "cli" || event.TargetName != "admin" || event.ActorID != 0 {
		t.Fatalf("audit event = %#v", event)
	}
	if event.FailureReason != "" {
		t.Fatalf("successful reset audit failure reason = %q", event.FailureReason)
	}
	// The HTTP password paths identify the target the same way.
	if event.TargetID != adminID || event.TargetRole != "admin" {
		t.Fatalf(
			"audit target = id %d role %q, want id %d role admin",
			event.TargetID,
			event.TargetRole,
			adminID,
		)
	}
}

// The same deployer file can be reached through a symlink; identity, not just
// the path string, decides that nothing may be removed.
func TestGatewayInitialAdminPasswordFileDetectsSymlinkedDeployerFile(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.DataDir = t.TempDir()
	generated := filepath.Join(cfg.DataDir, meta.InitialAdminPasswordFileName)
	if err := os.WriteFile(generated, []byte("generated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provided := filepath.Join(t.TempDir(), "admin-password")
	if err := os.Symlink(generated, provided); err != nil {
		t.Fatal(err)
	}
	cfg.Security.AdminPasswordFile = provided
	if got := gatewayInitialAdminPasswordFile(cfg); got != "" {
		t.Fatalf("symlinked deployer file target = %q, want none", got)
	}
}

// The gateway only ever removes the generated path, never the deployer's
// security.admin_password_file: that file belongs to the operator, and HA
// deployments must keep it working across an admin password change.
func TestGatewayInitialAdminPasswordFileNeverNamesTheDeployerFile(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.DataDir = t.TempDir()
	generated := filepath.Join(cfg.DataDir, meta.InitialAdminPasswordFileName)
	if got := gatewayInitialAdminPasswordFile(cfg); got != generated {
		t.Fatalf("generated path = %q, want %q", got, generated)
	}
	if got := bootstrapAdmin(cfg).GeneratedPasswordFile; got != generated {
		t.Fatalf("bootstrap generated path = %q, want %q", got, generated)
	}

	provided := filepath.Join(t.TempDir(), "admin-password")
	cfg.Security.AdminPasswordFile = provided
	if got := gatewayInitialAdminPasswordFile(cfg); got != generated {
		t.Fatalf("removal target = %q, want the generated path %q", got, generated)
	}
	if got := bootstrapAdmin(cfg).PasswordFile; got != provided {
		t.Fatalf("bootstrap password file = %q, want %q", got, provided)
	}

	cfg.Security.AdminPasswordFile = generated
	if got := gatewayInitialAdminPasswordFile(cfg); got != "" {
		t.Fatalf("removal target = %q, want none when the deployer file is the generated path", got)
	}
}
