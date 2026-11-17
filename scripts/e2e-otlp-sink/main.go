// Command e2e-otlp-sink is a controlled, high-headroom OTLP/HTTP upstream for
// gateway scaling tests. It validates protobuf payloads but intentionally does
// not persist them, keeping the upstream out of the measured bottleneck.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxRequestBodyBytes int64 = 64 << 20
const validationCacheEntries = 256
const sinkHeader = "X-Lazy-Observe-Perf-Sink"
const receiptHeader = "X-Lazy-Observe-Perf-Receipt"
const requestHeader = "X-Lazy-Observe-Perf-Request"

type signalKind uint8

const (
	signalMetrics signalKind = iota + 1
	signalLogs
)

func main() {
	listen := flag.String("listen", "", "HTTP listen address")
	expectedBatch := flag.Int("expected-batch", 0, "required data points or log records per request")
	flag.Parse()
	if strings.TrimSpace(*listen) == "" {
		fatal("-listen is required")
	}
	if *expectedBatch <= 0 {
		fatal("-expected-batch must be a positive integer")
	}
	nonce, err := generateNonce()
	if err != nil {
		fatal("generate instance nonce: %v", err)
	}

	server := &http.Server{
		Addr: *listen,
		Handler: newHandler(
			*expectedBatch,
			nonce,
			maxRequestBodyBytes,
			newValidationCache(validationCacheEntries),
		),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve: %v", err)
	}
}

func newHandler(expectedBatch int, nonce string, bodyLimit int64, cache *validationCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/-/healthy") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set(sinkHeader, nonce)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "OK\n")
			return
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		kind, ok := signalForPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		requestID, ok := performanceRequestID(r.Header)
		if !ok {
			http.Error(w, "missing or invalid performance request proof", http.StatusBadRequest)
			return
		}
		body, err := readBoundedBody(w, r, bodyLimit)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}

		key := validationKey{kind: kind, digest: sha256.Sum256(body)}
		if err := cache.validate(key, func() error {
			return validateOTLPBatch(kind, body, expectedBatch)
		}); err != nil {
			http.Error(w, "invalid OTLP performance batch: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Empty ExportMetricsServiceResponse and ExportLogsServiceResponse both
		// encode to a zero-length protobuf body.
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set(sinkHeader, nonce)
		w.Header().Set(receiptHeader, hex.EncodeToString(key.digest[:]))
		w.Header().Set(requestHeader, requestID)
		w.WriteHeader(http.StatusOK)
	})
}

func performanceRequestID(header http.Header) (string, bool) {
	values := header.Values(requestHeader)
	if len(values) != 1 {
		return "", false
	}
	const prefix = "loreq_"
	const randomLength = 24
	value := values[0]
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(value, prefix)
	if len(remainder) < randomLength+2 || remainder[randomLength] != '_' {
		return "", false
	}
	for _, character := range remainder[:randomLength] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", false
		}
	}
	sequence := remainder[randomLength+1:]
	if sequence == "" || sequence[0] == '0' {
		return "", false
	}
	number, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != sequence {
		return "", false
	}
	return value, true
}

func readBoundedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return io.ReadAll(r.Body)
}

func signalForPath(path string) (signalKind, bool) {
	switch {
	case path == "/v1/metrics", path == "/opentelemetry/v1/metrics", isClusterMetricsPath(path):
		return signalMetrics, true
	case path == "/v1/logs", path == "/insert/opentelemetry/v1/logs":
		return signalLogs, true
	default:
		return 0, false
	}
}

func isClusterMetricsPath(path string) bool {
	const prefix = "/insert/"
	const suffix = "/opentelemetry/v1/metrics"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return false
	}
	tenant := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if tenant == "" || strings.Contains(tenant, "/") {
		return false
	}
	_, err := strconv.ParseUint(tenant, 10, 32)
	return err == nil
}

func generateNonce() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "losink_" + hex.EncodeToString(value), nil
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "e2e-otlp-sink: "+format+"\n", args...)
	os.Exit(2)
}
