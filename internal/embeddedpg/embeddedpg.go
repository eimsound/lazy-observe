// Package embeddedpg holds the shared plumbing for the tests that start an embedded Postgres.
// It is imported only by _test.go files, so it never reaches the shipped binaries.
package embeddedpg

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// CacheEnv names the directory that keeps the downloaded Postgres archive.
// The tests used to point the cache at a fresh temporary directory, which made every run
// re-download the archive from Maven Central. A stable directory lets CI cache the archive
// and lets a local rerun work offline.
const CacheEnv = "LO_TEST_PG_CACHE"

// binaryRepository is the Maven Central prefix embedded-postgres downloads from
// (Config.binaryRepositoryURL in embeddedpostgres.DefaultConfig).
const binaryRepository = "https://repo1.maven.org/maven2"

// probeTimeout bounds the diagnostic HEAD request; it must not outlive a test run.
const probeTimeout = 10 * time.Second

// CachePath returns the directory for the downloaded Postgres archive: CacheEnv when it is
// set, otherwise fallback (a per-run temporary directory).
func CachePath(fallback string) string {
	if dir := os.Getenv(CacheEnv); dir != "" {
		return dir
	}

	return fallback
}

// DownloadProbe reports what Maven Central currently answers for the archive that
// embedded-postgres fetches, for example "HTTP 429". embedded-postgres collapses every
// non-200 download into "no version found matching <version>" (remote_fetch.go), so the
// status code is the only way to tell a missing version from rate limiting or an outage.
// version is the Postgres version the test configured, normally embeddedpostgres.V18.
func DownloadProbe(version embeddedpostgres.PostgresVersion) string {
	url := archiveURL(version)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Sprintf("probe HEAD %s: %v", url, err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Sprintf("probe HEAD %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	return fmt.Sprintf("probe HEAD %s: %s", url, response.Status)
}

// archiveURL rebuilds the jar URL that embedded-postgres builds from runtime.GOOS/GOARCH and
// the configured version (defaultVersionStrategy in version_strategy.go for the platforms
// this repo tests on: linux amd64/arm64, darwin amd64/arm64).
func archiveURL(version embeddedpostgres.PostgresVersion) string {
	operatingSystem, architecture := archivePlatform()

	return fmt.Sprintf(
		"%s/io/zonky/test/postgres/embedded-postgres-binaries-%s-%s/%s/embedded-postgres-binaries-%s-%s-%s.jar",
		binaryRepository,
		operatingSystem,
		architecture,
		version,
		operatingSystem,
		architecture,
		version,
	)
}

func archivePlatform() (string, string) {
	operatingSystem, architecture := runtime.GOOS, runtime.GOARCH

	switch {
	case operatingSystem == "linux" && architecture == "arm64":
		architecture = "arm64v8"
	case operatingSystem == "darwin" && architecture == "arm64":
		architecture = "arm64v8"
	}

	// The zonky project publishes musl builds under a separate classifier; embedded-postgres
	// appends it when the host is Alpine.
	if operatingSystem == "linux" {
		if _, err := os.Stat("/etc/alpine-release"); err == nil {
			architecture += "-alpine"
		}
	}

	return operatingSystem, architecture
}
