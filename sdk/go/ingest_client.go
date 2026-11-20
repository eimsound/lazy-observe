package lazyobserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ingestDefaultTimeout    = 15 * time.Second
	ingestMaxErrorBodyBytes = 4096
	ingestMaxDrainBytes     = 64 << 10
	ingestMaxRedirects      = 10
)

type ingestClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type ingestRequest struct {
	Path          string
	ContentType   string
	ContentLength int64
	Body          func() io.ReadCloser
	Timeout       time.Duration
	Op            string
}

func newIngestClient(baseURL, token string, httpClient *http.Client) *ingestClient {
	if httpClient == nil {
		httpClient = newDefaultIngestHTTPClient(ingestDefaultTimeout)
	}
	return &ingestClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
	}
}

func newDefaultIngestHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = ingestDefaultTimeout
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: ingestCheckRedirect,
	}
}

func ingestCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= ingestMaxRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && !sameOrigin(via[0], req) {
		req.Header.Del("Authorization")
	}
	return nil
}

func sameOrigin(a, b *http.Request) bool {
	if a == nil || b == nil || a.URL == nil || b.URL == nil {
		return false
	}
	return strings.EqualFold(a.URL.Scheme, b.URL.Scheme) && strings.EqualFold(a.URL.Host, b.URL.Host)
}

func (c *ingestClient) endpointURL(path string) string {
	if path == "" {
		return c.baseURL
	}
	if strings.HasPrefix(path, "/") {
		return c.baseURL + path
	}
	return c.baseURL + "/" + path
}

func (c *ingestClient) post(parent context.Context, req ingestRequest) error {
	op := req.Op
	if op == "" {
		op = "ingest request"
	}
	ctx := parent
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	var body io.Reader
	if req.Body != nil {
		body = req.Body()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(req.Path), body)
	if err != nil {
		return fmt.Errorf("%s: create request: %w", op, err)
	}
	if req.Body != nil {
		bodyFn := req.Body
		httpReq.GetBody = func() (io.ReadCloser, error) {
			return bodyFn(), nil
		}
	}
	if req.ContentLength >= 0 {
		httpReq.ContentLength = req.ContentLength
	}
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}

	client := c.http
	if client == nil {
		client = newDefaultIngestHTTPClient(ingestDefaultTimeout)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ingestMaxDrainBytes))
		return nil
	}

	response, _ := io.ReadAll(io.LimitReader(resp.Body, ingestMaxErrorBodyBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ingestMaxDrainBytes))
	msg := strings.TrimSpace(string(response))
	errMsg := fmt.Errorf("%s: HTTP %d", op, resp.StatusCode)
	if msg != "" {
		errMsg = fmt.Errorf("%s: HTTP %d: %s", op, resp.StatusCode, msg)
	}
	return &deliveryError{
		statusCode: resp.StatusCode,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		permanent:  isPermanentHTTPStatus(resp.StatusCode),
		err:        errMsg,
	}
}
