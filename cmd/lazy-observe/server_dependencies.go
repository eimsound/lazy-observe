package main

import (
	"context"
	"fmt"
	"log/slog"
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
	dbURL := cfg.Meta.DBURL
	if dbURL == "" {
		dbURL = filepath.Join(cfg.DataDir, "meta", "lazy-observe.db")
	}
	store, err := meta.OpenStoreWithOptions(ctx, dbURL, meta.OpenOptions{
		MigrationLockTimeout: cfg.Meta.MigrationLockTimeout.Value(),
		ApplicationVersion:   Version,
	})
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
	if _, err := store.Bootstrap(ctx, logger); err != nil {
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
