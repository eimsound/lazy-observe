package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestNativeTLSLoopbackE2E covers the gateway's own loopback clients when it
// terminates TLS itself: MCP tool calls and self-monitoring export both reach
// the gateway again from inside the process and must not speak plaintext HTTP
// to the TLS listener.
func TestNativeTLSLoopbackE2E(t *testing.T) {
	binary, binDir := os.Getenv("LO_E2E_BINARY"), os.Getenv("LO_E2E_STORAGE_BIN_DIR")
	if binary == "" || binDir == "" {
		t.Skip("LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR are required")
	}
	dataDir := t.TempDir()
	certFile, keyFile, pool := writeLoopbackCertificate(t, t.TempDir())
	metaDBURL := filepath.Join(dataDir, "meta", "lazy-observe.db")
	if err := os.MkdirAll(filepath.Dir(metaDBURL), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := meta.OpenStore(context.Background(), metaDBURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), "admin", "tls-e2e-password", "admin"); err != nil {
		t.Fatal(err)
	}
	readerToken := userQueryToken(t, store, "tls-reader")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	base := "https://" + addr
	var output lockedBuffer
	cmd := exec.Command(binary, "server", "--listen", addr, "--storage-bin-dir", binDir, "--data-dir", dataDir,
		"--storage-startup-timeout", "30s", "--tls-cert", certFile, "--tls-key", keyFile)
	cmd.Env = append(os.Environ(), "LO_META_DB_URL="+metaDBURL)
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server did not exit cleanly after SIGTERM: %v", err)
			}
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("server did not exit within 20s after SIGTERM")
		}
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	waitFor(t, 40*time.Second, func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	ctx := context.Background()
	mcpHTTPClient := *client
	mcpHTTPClient.Transport = bearerTokenTransport{base: client.Transport, token: readerToken, project: "default"}
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "lazy-observe-tls-e2e", Version: "1.0.0"}, nil).
		Connect(ctx, &sdkmcp.StreamableClientTransport{
			Endpoint: base + "/mcp", HTTPClient: &mcpHTTPClient, DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect MCP over TLS: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	callMCPToolE2E(t, ctx, session, "list_issues", map[string]any{})

	// Self-monitoring exports gateway HTTP metrics to the built-in project; the
	// healthz polling above produced some.
	selfMetrics := base + "/api/v1/query?" + url.Values{"query": {`count({__name__=~".+"})`}}.Encode()
	self := e2eReader{token: readerToken, project: meta.SelfMonitoringProjectSlug}
	waitFor(t, 45*time.Second, func() bool {
		_, ok := metricScalar(authorizedGet(t, client, selfMetrics, self))
		return ok
	})
}

func writeLoopbackCertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "lazy-observe-e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(
		certFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return certFile, keyFile, pool
}
