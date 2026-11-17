# Embedded release build

`mise run dist` builds the Vite UI, copies `web/dist` into the `internal/webui`
embed root, prepares the current platform's four managed executables, and then
builds `bin/lazy-observe` with `CGO_ENABLED=0`. `mise run dist-cross` repeats the
storage preparation and Go build for linux × amd64/arm64.

The storage release versions and archive SHA-256 values remain locked in
`scripts/download-storage-bins.sh`. `scripts/prepare-embedded-storage.sh`
compresses each executable deterministically with `gzip -9 -n`, writes a
manifest containing the uncompressed size and SHA-256, and places only the
target platform under `internal/embed/bins/assets/<goos>-<goarch>`. Build tags
ensure that a release contains one platform, never all supported targets.

At startup, an empty `--storage-bin-dir` selects the embedded payloads. They
are expanded atomically into `<data-dir>/bin`, verified against the manifest,
made executable, and reused only after another size and SHA-256 check. An
explicit `--storage-bin-dir` remains the higher-priority development/offline
override and does not extract embedded files.

## Packaging decision and size gate

The current payload contains VictoriaMetrics, VictoriaLogs, VictoriaTraces,
and vmagent. Each release uses a platform-tagged deterministic gzip embed so it
remains offline on first start without carrying binaries for other platforms.
`mise run perf-release` calculates the exact artifact and payload sizes for both
targets and enforces the repository's 150 MB limit; measured values belong
to the generated gate result rather than this document so they cannot become
stale when a managed executable is added or removed.

Generated web and storage payloads are gitignored. Checked-in `.gitkeep` files
let ordinary `go test` and non-release development builds compile without
generated assets; such a build reports that embedded storage is unavailable
unless `--storage-bin-dir` is supplied. Production/default distribution builds
must use `mise run dist` or `mise run dist-cross`.
