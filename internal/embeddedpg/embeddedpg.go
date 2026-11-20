// Package embeddedpg holds the shared plumbing for the tests that start an embedded Postgres.
// It is imported only by _test.go files, so it never reaches the shipped binaries.
package embeddedpg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres/v2"
)

// CacheEnv names the directory that keeps the downloaded Postgres distribution.
// The tests used to point the cache at a fresh temporary directory, which made every run
// re-download the distribution. A stable directory lets CI cache it and lets a local rerun
// work offline.
const CacheEnv = "LO_TEST_PG_CACHE"

// Version is the Postgres distribution the tests run; the CI cache key in ci.yml names it too.
const Version = embeddedpostgres.V18

// prepareTimeout bounds one prepare. The library tries a download three times and every try is
// bounded by clientTimeout, so the worst case is 3 x 2 minutes plus backoff; the cap keeps that
// below go test's default 10-minute -timeout, so the caller still reports the last HTTP status
// instead of a test-timeout panic.
const prepareTimeout = 7 * time.Minute

// clientTimeout bounds one download try, including every retry inside it and the body read.
const clientTimeout = 2 * time.Minute

// buildTimeout bounds compiling the supervisor; with a warm build cache it only links.
const buildTimeout = 2 * time.Minute

// supervisorPackage is the companion supervisor's main package inside the embedded-postgres
// module, the same source its GitHub release binaries are built from.
const supervisorPackage = "github.com/fergusstrange/embedded-postgres/v2/cmd/embedded-postgres"

// CachePath returns the directory for the downloaded Postgres distribution: CacheEnv when it
// is set, otherwise fallback (a per-run temporary directory).
func CachePath(fallback string) string {
	if dir := os.Getenv(CacheEnv); dir != "" {
		return dir
	}

	return fallback
}

// NewConfig readies everything Start needs and returns a config whose Start makes no network
// request: the Postgres distribution is in cacheDir, the supervisor is built from source into
// root, and the work and data directories are under root. root is per run and the caller
// removes it; cacheDir is shared and may be reused. Callers add Port, Database and so on.
//
// A failure carries the real HTTP status, the SHA-256 mismatch or the compiler output, and is
// final: it never skips the download or falls back to an unverified binary.
func NewConfig(cacheDir, root string) (embeddedpostgres.Config, error) {
	provider := retryingProvider(cacheDir)
	if err := prepare(provider, Version); err != nil {
		return embeddedpostgres.Config{}, err
	}
	supervisor, err := buildSupervisor(root)
	if err != nil {
		return embeddedpostgres.Config{}, err
	}

	return embeddedpostgres.DefaultConfig().
		Version(Version).
		Provider(provider).
		Supervisor(supervisor).
		Storage(embeddedpostgres.Storage{
			CacheDir: cacheDir,
			WorkDir:  filepath.Join(root, "runtime"),
			DataDir:  filepath.Join(root, "data"),
		}).
		StartTimeout(2 * time.Minute).
		Logger(io.Discard), nil
}

// retryingProvider returns embedded-postgres's own DownloadProvider rooted at cacheDir, with an
// HTTP client that retries transient failures.
//
// The library pins the SHA-256 of every distribution in its manifest and rejects a download
// that does not match, so a failure is never fail-open. What it does not do well is retry:
// three tries 200 ms apart, and only for 429, 5xx and network errors. A rate limited or
// briefly unavailable CI egress IP therefore failed whole runs; the client here retries 403,
// 404, 408, 429, 5xx and network errors with exponential backoff and honours Retry-After.
func retryingProvider(cacheDir string) embeddedpostgres.DownloadProvider {
	return newProvider(cacheDir, newRetryTransport())
}

func newProvider(cacheDir string, transport http.RoundTripper) embeddedpostgres.DownloadProvider {
	return embeddedpostgres.DownloadProvider{
		CacheDir: cacheDir,
		Client:   &http.Client{Timeout: clientTimeout, Transport: transport},
	}
}

// prepare makes sure the Postgres distribution for version is in the provider's cache, so
// Start finds a cache hit and never downloads on its own. The file name, URL, platform mapping
// and checksum all come from provider.Acquire, the same call Start makes.
func prepare(provider embeddedpostgres.DownloadProvider, version embeddedpostgres.PostgresVersion) error {
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()

	installation, err := provider.Acquire(ctx, embeddedpostgres.BinaryRequest{
		Version: version,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("acquire postgres %s: %w", version, err)
	}
	if installation.Release != nil {
		return installation.Release()
	}

	return nil
}

// buildSupervisor compiles the supervisor that embedded-postgres runs Postgres under into dir
// and returns its path, for Config.Supervisor.
//
// Left alone, the library downloads a prebuilt supervisor from its GitHub release and checks
// it only against checksums.txt from the same release. Release assets are mutable, so that
// catches corruption but not a replaced binary, and the request is not retried. Building the
// module source go.sum already pins removes the download: the only native executables the
// tests fetch are the Postgres distributions, pinned by the library's manifest. The go test
// command puts its own GOROOT/bin first in PATH, so "go" is the toolchain running the test.
func buildSupervisor(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()

	output := filepath.Join(dir, "embedded-postgres-supervisor")
	if runtime.GOOS == "windows" {
		output += ".exe"
	}
	command := exec.CommandContext(ctx, "go", "build", "-o", output, supervisorPackage)
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if combined, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build embedded-postgres supervisor: %w\n%s", err, combined)
	}

	return output, nil
}
