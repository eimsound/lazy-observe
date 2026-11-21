package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const testNonce = "losink_test_instance"
const testRequestID = "loreq_0123456789abcdef01234567_1"

func TestGenerateNonce(t *testing.T) {
	first, err := generateNonce()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateNonce()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "losink_") || len(first) != len("losink_")+24 {
		t.Fatalf("nonce has unexpected shape: %q", first)
	}
	if first == second {
		t.Fatal("two generated instance nonces are equal")
	}
}

func TestHealthEndpointsIncludeInstanceNonce(t *testing.T) {
	handler, _ := testHandler(3)
	for _, path := range []string{"/health", "/-/healthy"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || response.Body.String() != "OK\n" {
				t.Fatalf("GET %s status=%d body=%q", path, response.Code, response.Body.String())
			}
			if got := response.Header().Get(sinkHeader); got != testNonce {
				t.Fatalf("GET %s sink header=%q", path, got)
			}
			if got := response.Header().Get(receiptHeader); got != "" {
				t.Fatalf("GET %s unexpectedly returned receipt=%q", path, got)
			}
		})
	}
}

func TestAcceptsCompleteGatewayBatchesAndCachesThem(t *testing.T) {
	const expected = 3
	handler, cache := testHandler(expected)
	metricsBody := metricsPayload(t, []int{0, 1, 2}, []string{"run-a", "run-a", "run-a"})
	logsBody := logsPayload(t, []int{0, 1, 2}, []string{"run-b", "run-b", "run-b"})
	tests := []struct {
		name string
		path string
		body []byte
	}{
		{"public metrics", "/v1/metrics", metricsBody},
		{"standalone metrics", "/opentelemetry/v1/metrics", metricsBody},
		{"cluster metrics", "/insert/42/opentelemetry/v1/metrics", metricsBody},
		{"public logs", "/v1/logs", logsBody},
		{"upstream logs", "/insert/opentelemetry/v1/logs", logsBody},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for attempt := 0; attempt < 2; attempt++ {
				requestID := fmt.Sprintf("loreq_0123456789abcdef01234567_%d", attempt+1)
				response := postWithRequestID(t, handler, test.path, test.body, requestID)
				if response.Code != http.StatusOK {
					t.Fatalf("POST %s status=%d body=%q", test.path, response.Code, response.Body.String())
				}
				if got := response.Header().Get("Content-Type"); got != "application/x-protobuf" {
					t.Fatalf("POST %s content-type=%q", test.path, got)
				}
				if got := response.Header().Get(sinkHeader); got != testNonce {
					t.Fatalf("POST %s sink header=%q", test.path, got)
				}
				if got, want := response.Header().Get(receiptHeader), bodyDigest(test.body); got != want {
					t.Fatalf("POST %s receipt=%q want=%q", test.path, got, want)
				}
				if got := response.Header().Get(requestHeader); got != requestID {
					t.Fatalf("POST %s request proof=%q want=%q", test.path, got, requestID)
				}
				if response.Body.Len() != 0 {
					t.Fatalf("POST %s returned non-empty export response: %x", test.path, response.Body.Bytes())
				}
			}
		})
	}
	// Reusing the same metrics or logs bytes through multiple supported paths
	// must hit one kind-scoped entry rather than expanding the cache per path.
	if got := cache.len(); got != 2 {
		t.Fatalf("cache entries=%d want=2", got)
	}
}

func TestRejectsMissingOrMalformedRequestProof(t *testing.T) {
	handler, _ := testHandler(3)
	body := metricsPayload(t, []int{0, 1, 2}, []string{"run", "run", "run"})
	tests := []struct {
		name      string
		requestID string
	}{
		{"missing", ""},
		{"wrong prefix", "request_0123456789abcdef01234567_1"},
		{"short random", "loreq_0123456789abcdef0123456_1"},
		{"uppercase hex", "loreq_0123456789abcdef0123456A_1"},
		{"non hex", "loreq_0123456789abcdef0123456g_1"},
		{"zero sequence", "loreq_0123456789abcdef01234567_0"},
		{"leading zero sequence", "loreq_0123456789abcdef01234567_01"},
		{"non decimal sequence", "loreq_0123456789abcdef01234567_1a"},
		{"extra segment", "loreq_0123456789abcdef01234567_1_extra"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postWithRequestID(t, handler, "/v1/metrics", body, test.requestID)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if got := response.Header().Get(requestHeader); got != "" {
				t.Fatalf("rejected request echoed proof %q", got)
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	request.Header.Add(requestHeader, testRequestID)
	request.Header.Add(requestHeader, "loreq_0123456789abcdef01234567_2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate proof headers status=%d", response.Code)
	}
}

func TestReceiptChangesWithBodyWhileNonceStaysFixed(t *testing.T) {
	handler, _ := testHandler(3)
	first := post(t, handler, "/v1/metrics", metricsPayload(t,
		[]int{0, 1, 2}, []string{"run-a", "run-a", "run-a"},
	))
	second := post(t, handler, "/v1/metrics", metricsPayload(t,
		[]int{0, 1, 2}, []string{"run-b", "run-b", "run-b"},
	))
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses first=%d second=%d", first.Code, second.Code)
	}
	if first.Header().Get(sinkHeader) != testNonce || second.Header().Get(sinkHeader) != testNonce {
		t.Fatalf(
			"sink nonce changed: first=%q second=%q",
			first.Header().Get(sinkHeader),
			second.Header().Get(sinkHeader),
		)
	}
	firstReceipt := first.Header().Get(receiptHeader)
	secondReceipt := second.Header().Get(receiptHeader)
	if firstReceipt == "" || secondReceipt == "" || firstReceipt == secondReceipt {
		t.Fatalf("receipts first=%q second=%q", firstReceipt, secondReceipt)
	}
}

func TestRejectsMalformedAndIncompleteBatches(t *testing.T) {
	const expected = 3
	handler, _ := testHandler(expected)
	tests := []struct {
		name string
		path string
		body []byte
	}{
		{"invalid metrics protobuf", "/v1/metrics", []byte{0xff}},
		{"invalid logs protobuf", "/v1/logs", []byte{0xff}},
		{"logs payload on metrics path", "/v1/metrics", logsPayload(t, []int{0, 1, 2}, []string{"run", "run", "run"})},
		{"metrics payload on logs path", "/v1/logs", metricsPayload(t, []int{0, 1, 2}, []string{"run", "run", "run"})},
		{"partial metrics", "/v1/metrics", metricsPayload(t, []int{0, 1}, []string{"run", "run"})},
		{"partial logs", "/v1/logs", logsPayload(t, []int{0, 1}, []string{"run", "run"})},
		{
			"metrics marker mismatch",
			"/v1/metrics",
			metricsPayload(t, []int{0, 1, 2}, []string{"run-a", "run-b", "run-a"}),
		},
		{"logs marker mismatch", "/v1/logs", logsPayload(t, []int{0, 1, 2}, []string{"run-a", "run-b", "run-a"})},
		{"metrics duplicate sequence", "/v1/metrics", metricsPayload(t, []int{0, 1, 1}, []string{"run", "run", "run"})},
		{"logs duplicate sequence", "/v1/logs", logsPayload(t, []int{0, 1, 1}, []string{"run", "run", "run"})},
		{"metrics missing marker", "/v1/metrics", metricsPayload(t, []int{0, 1, 2}, []string{"run", "", "run"})},
		{"logs missing marker", "/v1/logs", logsPayload(t, []int{0, 1, 2}, []string{"run", "", "run"})},
		{"metrics missing series", "/v1/metrics", metricsPayload(t, []int{0, -1, 2}, []string{"run", "run", "run"})},
		{"logs missing sequence", "/v1/logs", logsPayload(t, []int{0, -1, 2}, []string{"run", "run", "run"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := post(t, handler, test.path, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("POST %s status=%d body=%q", test.path, response.Code, response.Body.String())
			}
			if got := response.Header().Get(sinkHeader); got != "" {
				t.Fatalf("rejected request exposed success header %q", got)
			}
			if got := response.Header().Get(receiptHeader); got != "" {
				t.Fatalf("rejected request exposed receipt %q", got)
			}
			if got := response.Header().Get(requestHeader); got != "" {
				t.Fatalf("rejected request echoed proof %q", got)
			}
		})
	}
}

func TestRejectsOversizeBody(t *testing.T) {
	const limit = 8
	handler := newHandler(3, testNonce, limit, newValidationCache(4))
	response := post(t, handler, "/v1/metrics", []byte(strings.Repeat("x", limit+1)))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestReturnsNotFoundForUnsupportedRequests(t *testing.T) {
	handler, _ := testHandler(3)
	tests := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/traces", nil),
		httptest.NewRequest(http.MethodPost, "/insert/not-a-tenant/opentelemetry/v1/metrics", nil),
		httptest.NewRequest(http.MethodGet, "/v1/metrics", nil),
		httptest.NewRequest(http.MethodPost, "/health", nil),
	}
	for _, request := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d", request.Method, request.URL.Path, response.Code)
		}
	}
}

func TestValidationCacheCoalescesConcurrentRequests(t *testing.T) {
	cache := newValidationCache(4)
	key := validationKey{kind: signalMetrics, digest: sha256.Sum256([]byte("same body"))}
	var validations atomic.Int32
	const workers = 32
	start := make(chan struct{})
	errorsByWorker := make([]error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer wait.Done()
			<-start
			errorsByWorker[index] = cache.validate(key, func() error {
				validations.Add(1)
				time.Sleep(20 * time.Millisecond)
				return nil
			})
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("full validations=%d want=1", got)
	}
	if got := cache.len(); got != 1 {
		t.Fatalf("cache entries=%d want=1", got)
	}
}

func TestValidationCacheIsBoundedAndKindScoped(t *testing.T) {
	cache := newValidationCache(2)
	digest := sha256.Sum256([]byte("same bytes"))
	var validations atomic.Int32
	validate := func() error {
		validations.Add(1)
		return nil
	}
	if err := cache.validate(validationKey{kind: signalMetrics, digest: digest}, validate); err != nil {
		t.Fatal(err)
	}
	if err := cache.validate(validationKey{kind: signalLogs, digest: digest}, validate); err != nil {
		t.Fatal(err)
	}
	if got := validations.Load(); got != 2 {
		t.Fatalf("kind-scoped validations=%d want=2", got)
	}
	if err := cache.validate(
		validationKey{kind: signalMetrics, digest: sha256.Sum256([]byte("third"))},
		validate,
	); err != nil {
		t.Fatal(err)
	}
	if got := cache.len(); got != 2 {
		t.Fatalf("bounded cache entries=%d want=2", got)
	}

	validationError := errors.New("invalid")
	badKey := validationKey{kind: signalLogs, digest: sha256.Sum256([]byte("bad"))}
	if err := cache.validate(badKey, func() error { return validationError }); !errors.Is(err, validationError) {
		t.Fatalf("validation error=%v", err)
	}
	if got := cache.len(); got != 2 {
		t.Fatalf("failed validation entered cache; entries=%d", got)
	}
}

func testHandler(expected int) (http.Handler, *validationCache) {
	cache := newValidationCache(8)
	return newHandler(expected, testNonce, 1<<20, cache), cache
}

func post(t *testing.T, handler http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	return postWithRequestID(t, handler, path, body, testRequestID)
}

func postWithRequestID(
	t *testing.T,
	handler http.Handler,
	path string,
	body []byte,
	requestID string,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-protobuf")
	if requestID != "" {
		request.Header.Set(requestHeader, requestID)
	}
	handler.ServeHTTP(response, request)
	return response
}

func metricsPayload(t *testing.T, sequences []int, markers []string) []byte {
	t.Helper()
	if len(sequences) != len(markers) {
		t.Fatal("metrics fixture shape mismatch")
	}
	points := make([]*metricsdata.NumberDataPoint, len(sequences))
	for index, sequence := range sequences {
		attributes := []*commonpb.KeyValue{{
			Key: metricsMarkerAttribute, Value: stringValueForTest(markers[index]),
		}}
		if sequence >= 0 {
			attributes = append(attributes, &commonpb.KeyValue{
				Key: metricsSeriesAttribute, Value: stringValueForTest(strconv.Itoa(sequence)),
			})
		}
		points[index] = &metricsdata.NumberDataPoint{
			Attributes: attributes,
			Value:      &metricsdata.NumberDataPoint_AsInt{AsInt: int64(index)},
		}
	}
	return marshal(t, &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsdata.ResourceMetrics{{
			ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{
				Name: "lazy_observe_scale_gate_samples",
				Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{
					DataPoints: points,
				}},
			}}}},
		}},
	})
}

func logsPayload(t *testing.T, sequences []int, markers []string) []byte {
	t.Helper()
	if len(sequences) != len(markers) {
		t.Fatal("logs fixture shape mismatch")
	}
	records := make([]*logdata.LogRecord, len(sequences))
	for index, sequence := range sequences {
		attributes := []*commonpb.KeyValue{}
		if sequence >= 0 {
			attributes = append(attributes, &commonpb.KeyValue{
				Key: logsSequenceAttribute, Value: stringValueForTest(strconv.Itoa(sequence)),
			})
		}
		records[index] = &logdata.LogRecord{
			Body:       stringValueForTest(logsMessagePrefix + markers[index]),
			Attributes: attributes,
		}
	}
	return marshal(t, &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logdata.ResourceLogs{{
			ScopeLogs: []*logdata.ScopeLogs{{LogRecords: records}},
		}},
	})
}

func stringValueForTest(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

func marshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func bodyDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest)
}

func TestReadBoundedBodyHandlesKnownChunkedAndMismatchedContentLength(t *testing.T) {
	largePayload := bytes.Repeat([]byte("a"), 24<<10)
	exactPayload := []byte("0123456789abcdef")

	tests := []struct {
		name          string
		payload       []byte
		contentLength int64
		limit         int64
		wantTooLarge  bool
	}{
		{
			name:          "known content length above pool initial cap",
			payload:       largePayload,
			contentLength: int64(len(largePayload)),
			limit:         int64(len(largePayload)),
		},
		{
			name:          "known content length exact limit",
			payload:       exactPayload,
			contentLength: int64(len(exactPayload)),
			limit:         int64(len(exactPayload)),
		},
		{
			name:          "unknown chunked length within limit",
			payload:       largePayload,
			contentLength: -1,
			limit:         int64(len(largePayload)) + 1024,
		},
		{
			name:          "unknown chunked length exceeds limit",
			payload:       largePayload,
			contentLength: -1,
			limit:         int64(len(largePayload)) - 1,
			wantTooLarge:  true,
		},
		{
			name:          "understated content length within limit",
			payload:       bytes.Repeat([]byte("u"), 200),
			contentLength: 10,
			limit:         256,
		},
		{
			name:          "understated content length exceeds limit",
			payload:       bytes.Repeat([]byte("v"), 100),
			contentLength: 10,
			limit:         64,
			wantTooLarge:  true,
		},
		{
			name:          "overstated huge content length within limit",
			payload:       []byte("small-body"),
			contentLength: maxPreallocBodyBytes * 4,
			limit:         maxPreallocBodyBytes * 8,
		},
		{
			name:          "overstated content length exceeds limit",
			payload:       bytes.Repeat([]byte("z"), 65),
			contentLength: 1024,
			limit:         64,
			wantTooLarge:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, pooled := range []bool{false, true} {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(
					http.MethodPost,
					"/v1/metrics",
					io.NopCloser(bytes.NewReader(test.payload)),
				)
				request.ContentLength = test.contentLength

				var (
					got     []byte
					release = func() {}
					err     error
				)
				if pooled {
					got, release, err = readBoundedBodyPooled(recorder, request, test.limit)
				} else {
					got, err = readBoundedBody(recorder, request, test.limit)
				}
				if test.wantTooLarge {
					release()
					var tooLarge *http.MaxBytesError
					if !errors.As(err, &tooLarge) {
						t.Fatalf("pooled=%v err = %v, want *http.MaxBytesError", pooled, err)
					}
					continue
				}
				if err != nil {
					release()
					t.Fatalf("pooled=%v unexpected err = %v", pooled, err)
				}
				if !bytes.Equal(got, test.payload) {
					release()
					t.Fatalf("pooled=%v len(got)=%d want=%d", pooled, len(got), len(test.payload))
				}
				release()
			}
		})
	}
}

func TestHandlerPooledBodyBuffersConcurrentReceipts(t *testing.T) {
	const batchSize = 100
	cache := newValidationCache(32)
	handler := newHandler(batchSize, testNonce, maxRequestBodyBytes, cache)

	sequences := make([]int, batchSize)
	for i := range sequences {
		sequences[i] = i
	}

	type sampleCase struct {
		path    string
		payload []byte
		digest  string
	}
	cases := make([]sampleCase, 0, 16)
	for i := 0; i < 8; i++ {
		marker := fmt.Sprintf("pool-run-%d", i)
		markers := make([]string, batchSize)
		for j := range markers {
			markers[j] = marker
		}
		mBody := metricsPayload(t, sequences, markers)
		lBody := logsPayload(t, sequences, markers)
		cases = append(
			cases,
			sampleCase{path: "/v1/metrics", payload: mBody, digest: bodyDigest(mBody)},
			sampleCase{path: "/v1/logs", payload: lBody, digest: bodyDigest(lBody)},
		)
	}

	var wg sync.WaitGroup
	var seq atomic.Uint64
	errCh := make(chan error, len(cases)*8)
	for _, tc := range cases {
		for round := 0; round < 8; round++ {
			wg.Add(1)
			go func(tc sampleCase) {
				defer wg.Done()
				requestID := fmt.Sprintf("loreq_0123456789abcdef01234567_%d", seq.Add(1))
				resp := postWithRequestID(t, handler, tc.path, tc.payload, requestID)
				if resp.Code != http.StatusOK {
					errCh <- fmt.Errorf("%s status=%d body=%q", tc.path, resp.Code, resp.Body.String())
					return
				}
				if got := resp.Header().Get(receiptHeader); got != tc.digest {
					errCh <- fmt.Errorf("%s receipt=%q want=%q", tc.path, got, tc.digest)
					return
				}
				if got := resp.Header().Get(requestHeader); got != requestID {
					errCh <- fmt.Errorf("%s request proof=%q want=%q", tc.path, got, requestID)
				}
			}(tc)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := cache.len(); got != len(cases) {
		t.Fatalf("cache entries=%d want=%d", got, len(cases))
	}
}
