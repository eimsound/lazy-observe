package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var releasePayloadNames = []string{
	"victoria-metrics-prod",
	"victoria-logs-prod",
	"victoria-traces-prod",
	"vmagent-prod",
}

type releaseSizeResult struct {
	Version        int    `json:"version"`
	GitCommit      string `json:"git_commit,omitempty"`
	GeneratedAt    string `json:"generated_at,omitempty"`
	Binary         string `json:"binary"`
	SizeBytes      int64  `json:"size_bytes"`
	MaxBinaryBytes int64  `json:"max_binary_bytes"`
	Platform       string `json:"platform"`
	EmbeddedFiles  int    `json:"embedded_storage_files"`
	GatePassed     bool   `json:"gate_passed"`
}

func runReleaseSize(args []string) error {
	flags := flag.NewFlagSet("release-size", flag.ContinueOnError)
	configPath := flags.String("config", defaultGateConfigPath, "performance gate config")
	binary := flags.String("binary", "bin/lazy-observe", "production embedded binary")
	platform := flags.String("platform", runtime.GOOS+"-"+runtime.GOARCH, "embedded storage asset platform")
	assetsRoot := flags.String("assets-root", "internal/embed/bins/assets", "staged embedded storage assets")
	output := flags.String("output", "", "optional JSON result path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadGateConfig(*configPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(*binary)
	if err != nil {
		return fmt.Errorf("stat production binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("production binary is not a regular file: %s", *binary)
	}
	embeddedFiles, err := verifyEmbeddedStorage(*binary, filepath.Join(*assetsRoot, *platform))
	if err != nil {
		return err
	}
	gitCommit, generatedAt := resultMetadata()
	result := releaseSizeResult{
		Version: 1, GitCommit: gitCommit, GeneratedAt: generatedAt,
		Binary: *binary, SizeBytes: info.Size(),
		MaxBinaryBytes: cfg.Release.MaxBinaryBytes,
		Platform:       *platform,
		EmbeddedFiles:  embeddedFiles,
		GatePassed:     info.Size() < cfg.Release.MaxBinaryBytes && embeddedFiles == len(releasePayloadNames),
	}
	if err := writeResult(*output, result); err != nil {
		return err
	}
	if !result.GatePassed {
		return failResult(
			fmt.Sprintf("release binary %d bytes is not below %d bytes", result.SizeBytes, result.MaxBinaryBytes),
		)
	}
	fmt.Printf(
		"PASS: production binary embeds %d storage payloads and is %d bytes (< %d)\n",
		result.EmbeddedFiles,
		result.SizeBytes,
		result.MaxBinaryBytes,
	)
	return nil
}

func verifyEmbeddedStorage(binaryPath, assetDir string) (int, error) {
	manifestPath := filepath.Join(assetDir, "manifest.tsv")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return 0, fmt.Errorf("read staged embedded manifest: %w", err)
	}
	if len(bytes.TrimSpace(manifest)) == 0 {
		return 0, fmt.Errorf("staged embedded manifest is empty: run the production dist task first")
	}
	wanted := make(map[string]bool, len(releasePayloadNames))
	for _, name := range releasePayloadNames {
		wanted[name] = true
	}
	type manifestEntry struct {
		size int64
		hash string
	}
	seen := make(map[string]manifestEntry, len(releasePayloadNames))
	scanner := bufio.NewScanner(bytes.NewReader(manifest))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return 0, fmt.Errorf("invalid staged embedded manifest line %q", scanner.Text())
		}
		_, duplicate := seen[fields[0]]
		if !wanted[fields[0]] || duplicate {
			return 0, fmt.Errorf("invalid staged embedded manifest line %q", scanner.Text())
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		decoded, hashErr := hex.DecodeString(fields[2])
		if err != nil || size <= 0 || hashErr != nil || len(decoded) != sha256.Size ||
			fields[2] != strings.ToLower(fields[2]) {
			return 0, fmt.Errorf("invalid staged embedded manifest line %q", scanner.Text())
		}
		seen[fields[0]] = manifestEntry{size: size, hash: fields[2]}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan staged embedded manifest: %w", err)
	}
	if len(seen) != len(releasePayloadNames) {
		return 0, fmt.Errorf(
			"staged embedded manifest has %d required payloads, want %d",
			len(seen),
			len(releasePayloadNames),
		)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return 0, fmt.Errorf("read production binary: %w", err)
	}
	if !bytes.Contains(binary, manifest) {
		return 0, fmt.Errorf(
			"production binary does not contain the staged %s storage manifest",
			filepath.Base(assetDir),
		)
	}
	for _, name := range releasePayloadNames {
		payload, err := os.ReadFile(filepath.Join(assetDir, name+".gz"))
		if err != nil {
			return 0, fmt.Errorf("read staged embedded %s: %w", name, err)
		}
		if len(payload) < 128 {
			return 0, fmt.Errorf("staged embedded %s payload is unexpectedly small", name)
		}
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return 0, fmt.Errorf("open staged embedded %s gzip: %w", name, err)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return 0, fmt.Errorf("verify staged embedded %s gzip: %w", name, copyErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("close staged embedded %s gzip: %w", name, closeErr)
		}
		entry := seen[name]
		if size != entry.size || hex.EncodeToString(hash.Sum(nil)) != entry.hash {
			return 0, fmt.Errorf("staged embedded %s does not match its uncompressed size/SHA-256 manifest", name)
		}
		if !bytes.Contains(binary, payload) {
			return 0, fmt.Errorf("production binary does not contain the complete staged embedded %s gzip", name)
		}
	}
	return len(seen), nil
}
