package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/secret"
	sharedstate "github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

type serverEssentials struct {
	llmPricing *llmcost.Table
	secretBox  *secret.Box
}

func loadLLMPricing() (*llmcost.Table, error) {
	return llmcost.Load("")
}

func loadSecretBox(cfg appconfig.Config) (*secret.Box, error) {
	// The platform key protects sensitive metadata such as BYO LLM API keys.
	// It is created on first startup and then held in memory.
	keyFile := cfg.Security.SecretKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(cfg.DataDir, "secret.key")
	}
	return secret.LoadOrCreate(keyFile)
}

type serverSharedState struct {
	sessions    sharedstate.SessionStore
	limits      sharedstate.Limiter
	loginGate   sharedstate.LoginGate
	redisClient *redis.Client
}

func openServerSharedState(ctx context.Context, cfg appconfig.Config) (*serverSharedState, error) {
	state := &serverSharedState{
		limits:    sharedstate.NewMemoryLimiter(),
		loginGate: sharedstate.NewMemoryLoginGateWithClock(time.Now),
	}
	if !cfg.Redis.Enabled {
		return state, nil
	}

	client := redis.NewClient(
		&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB},
	)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := client.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect Redis: %w", err)
	}

	redisState := sharedstate.NewRedis(client, cfg.Redis.KeyPrefix)
	state.sessions = redisState
	state.limits = redisState
	state.loginGate = redisState
	state.redisClient = client
	return state, nil
}

func (state *serverSharedState) Close() {
	if state.redisClient != nil {
		_ = state.redisClient.Close()
	}
}

type serverMetadata struct {
	store       *meta.Store
	blobs       blob.Store
	selfProject meta.ProjectInfo
	selfToken   string
}

func openServerMetadata(
	ctx context.Context,
	cfg appconfig.Config,
	logger *slog.Logger,
	secretBox *secret.Box,
) (*serverMetadata, error) {
	store, err := openMetadataStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = store.Close()
		}
	}()

	blobs, err := openBlobStore(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := store.Bootstrap(ctx, logger, bootstrapAdmin(cfg)); err != nil {
		return nil, fmt.Errorf("bootstrap metadata: %w", err)
	}
	selfProject, selfToken, err := store.BootstrapSelfMonitoring(
		ctx,
		"lo_"+secretBox.Derive("self-monitoring-project-token"),
	)
	if err != nil {
		return nil, fmt.Errorf("bootstrap self-monitoring metadata: %w", err)
	}
	initialized = true
	return &serverMetadata{
		store:       store,
		blobs:       blobs,
		selfProject: selfProject,
		selfToken:   selfToken,
	}, nil
}

// openMetadataStore opens the metadata database the server uses, so CLI
// subcommands operate on exactly the same rows without the HTTP API.
func openMetadataStore(ctx context.Context, cfg appconfig.Config) (*meta.Store, error) {
	dbURL := cfg.Meta.DBURL
	if dbURL == "" {
		dbURL = filepath.Join(cfg.DataDir, "meta", "lazy-observe.db")
	}
	return meta.OpenStoreWithOptions(ctx, dbURL, meta.OpenOptions{
		MigrationLockTimeout: cfg.Meta.MigrationLockTimeout.Value(),
		ApplicationVersion:   Version,
	})
}

// bootstrapAdmin uses the deployer-provided password file when configured;
// otherwise a generated first password lands in <data_dir>/initial-admin-password.
// Only the replica that wins the bootstrap lock writes that file, so HA
// deployments should provide security.admin_password_file instead.
func bootstrapAdmin(cfg appconfig.Config) meta.BootstrapAdmin {
	if cfg.Security.AdminPasswordFile != "" {
		return meta.BootstrapAdmin{PasswordFile: cfg.Security.AdminPasswordFile}
	}
	return meta.BootstrapAdmin{GeneratedPasswordFile: initialAdminPasswordPath(cfg)}
}

// initialAdminPasswordPath is the absolute generated-password path under the
// data directory. Bootstrap and `admin reset-password` share it so the gateway
// removes the very file either of them wrote.
func initialAdminPasswordPath(cfg appconfig.Config) string {
	path := filepath.Join(cfg.DataDir, meta.InitialAdminPasswordFileName)
	if absolute, err := filepath.Abs(path); err == nil {
		// The log line must point the operator at the file without guessing the cwd.
		return absolute
	}
	return path
}

// gatewayInitialAdminPasswordFile is the file the gateway removes after the
// bootstrap admin changes a generated password, and the path the reset CLI may
// write. It is empty when security.admin_password_file is the same file (by
// path or by file identity): that file belongs to the deployer and must never
// be removed. A deployer file at a different path does not suppress the
// generated path, because the CLI can still create that file.
func gatewayInitialAdminPasswordFile(cfg appconfig.Config) string {
	generated := initialAdminPasswordPath(cfg)
	if cfg.Security.AdminPasswordFile == "" || !sameFile(cfg.Security.AdminPasswordFile, generated) {
		return generated
	}
	return ""
}

// sameFile compares two paths by file identity when both exist, so a symlink or
// hard link to the protected file is recognised, and falls back to cleaned
// absolute paths when either side is absent.
func sameFile(first, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	if firstErr == nil && secondErr == nil {
		return os.SameFile(firstInfo, secondInfo)
	}
	return absoluteClean(first) == absoluteClean(second)
}

func absoluteClean(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func openBlobStore(cfg appconfig.Config) (blob.Store, error) {
	var (
		store blob.Store
		err   error
	)
	switch cfg.Blob.Backend {
	case "local":
		store, err = blob.NewLocal(cfg.DataDir)
	case "s3":
		store, err = blob.NewS3(
			blob.S3Config{
				Bucket:    cfg.Blob.S3.Bucket,
				Region:    cfg.Blob.S3.Region,
				Endpoint:  cfg.Blob.S3.Endpoint,
				AccessKey: cfg.Blob.S3.AccessKey,
				SecretKey: cfg.Blob.S3.SecretKey,
			},
		)
	default:
		return nil, fmt.Errorf("unsupported blob store %q", cfg.Blob.Backend)
	}
	if err != nil {
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	return store, nil
}

func (metadata *serverMetadata) Close() {
	_ = metadata.store.Close()
}
