package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	defaultLiveTailMaxGlobal             = 64
	defaultLiveTailMaxPerUser            = 4
	defaultLiveTailMaxPerProject         = 16
	defaultLiveTailMaxDuration           = 30 * time.Minute
	defaultLiveTailHeartbeat             = 15 * time.Second
	defaultLiveTailResponseHeaderTimeout = 30 * time.Second
	maxLiveTailErrorBodyBytes            = 1 << 20
)

type liveTailLimiter struct {
	mu            sync.Mutex
	maxGlobal     int
	maxPerUser    int
	maxPerProject int
	global        int
	byUser        map[int]int
	byProject     map[int]int
}

func newLiveTailLimiter(maxGlobal, maxPerUser, maxPerProject int) *liveTailLimiter {
	if maxGlobal <= 0 {
		maxGlobal = defaultLiveTailMaxGlobal
	}
	if maxPerUser <= 0 {
		maxPerUser = defaultLiveTailMaxPerUser
	}
	if maxPerProject <= 0 {
		maxPerProject = defaultLiveTailMaxPerProject
	}
	return &liveTailLimiter{
		maxGlobal:     maxGlobal,
		maxPerUser:    maxPerUser,
		maxPerProject: maxPerProject,
		byUser:        make(map[int]int),
		byProject:     make(map[int]int),
	}
}

func (l *liveTailLimiter) acquire(userID, projectID int) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global >= l.maxGlobal {
		return nil, false
	}
	if userID > 0 && l.byUser[userID] >= l.maxPerUser {
		return nil, false
	}
	if projectID > 0 && l.byProject[projectID] >= l.maxPerProject {
		return nil, false
	}
	l.global++
	if userID > 0 {
		l.byUser[userID]++
	}
	if projectID > 0 {
		l.byProject[projectID]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.global > 0 {
				l.global--
			}
			if userID > 0 {
				if n := l.byUser[userID] - 1; n <= 0 {
					delete(l.byUser, userID)
				} else {
					l.byUser[userID] = n
				}
			}
			if projectID > 0 {
				if n := l.byProject[projectID] - 1; n <= 0 {
					delete(l.byProject, projectID)
				} else {
					l.byProject[projectID] = n
				}
			}
		})
	}, true
}

func (l *liveTailLimiter) activeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global
}

func newLiveTailClient(base *http.Client) *http.Client {
	client := &http.Client{
		CheckRedirect: noFollowRedirects,
	}
	if base != nil {
		*client = *base
		client.Timeout = 0
		if client.CheckRedirect == nil {
			client.CheckRedirect = noFollowRedirects
		}
	}
	switch tr := client.Transport.(type) {
	case nil:
		cloned := http.DefaultTransport.(*http.Transport).Clone()
		cloned.MaxIdleConns = defaultProxyMaxIdleConnections
		cloned.MaxIdleConnsPerHost = defaultProxyMaxIdleConnectionsPerHost
		cloned.DisableCompression = true
		cloned.ResponseHeaderTimeout = defaultLiveTailResponseHeaderTimeout
		client.Transport = cloned
	case *http.Transport:
		cloned := tr.Clone()
		cloned.DisableCompression = true
		if cloned.ResponseHeaderTimeout == 0 {
			cloned.ResponseHeaderTimeout = defaultLiveTailResponseHeaderTimeout
		}
		client.Transport = cloned
	}
	return client
}

func (g *Gateway) logsLiveTail(c fiber.Ctx) error {
	p := currentProject(c)
	if p.ID <= 0 {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	if g.shutdownCtx.Err() != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"code":  "gateway_draining",
			"error": "gateway is shutting down",
		})
	}
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	releaseSlot, ok := g.liveTail.acquire(u.ID, p.ID)
	if !ok {
		g.logger.Warn(
			"live tail concurrency limit exceeded",
			"user_id", u.ID,
			"project_id", p.ID,
		)
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"code":  "live_tail_limit_exceeded",
			"error": "too many concurrent live tail streams",
		})
	}

	query, err := url.ParseQuery(string(c.RequestCtx().QueryArgs().QueryString()))
	if err != nil {
		releaseSlot()
		return fiber.NewError(fiber.StatusBadRequest, "invalid query parameters")
	}
	target := strings.TrimRight(g.storage.LogsBase(), "/") + "/select/logsql/tail"
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}

	streamCtx, streamCancel := context.WithTimeout(g.shutdownCtx, g.liveTailMaxDuration)
	body := append([]byte(nil), requestBody(c)...)
	req, err := http.NewRequestWithContext(streamCtx, c.Method(), target, bytes.NewReader(body))
	if err != nil {
		streamCancel()
		releaseSlot()
		return fiber.NewError(fiber.StatusInternalServerError, "failed to construct upstream request")
	}
	copyProxyRequestHeaders(req.Header, &c.Request().Header, false)
	req.Header.Del("Accept-Encoding")
	g.storage.SetTenantHeaders(req.Header, p.ID)

	resp, err := g.tailClient.Do(req)
	if err != nil {
		streamCancel()
		releaseSlot()
		if g.shutdownCtx.Err() != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"code":  "gateway_draining",
				"error": "gateway is shutting down",
			})
		}
		g.logger.Warn("live tail upstream request failed", "project_id", p.ID, "error", err)
		return fiber.NewError(fiber.StatusBadGateway, "upstream storage unavailable")
	}

	copyProxyResponseHeaders(c, resp.Header)
	c.Set("X-Accel-Buffering", "no")
	c.Set("Cache-Control", "no-cache")
	c.Status(resp.StatusCode)

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified || resp.ContentLength == 0 {
		drainAndCloseBody(resp.Body)
		streamCancel()
		releaseSlot()
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxLiveTailErrorBodyBytes))
		drainAndCloseBody(resp.Body)
		streamCancel()
		releaseSlot()
		return c.Send(errBody)
	}

	c.Response().ImmediateHeaderFlush = true
	c.Response().Header.SetConnectionClose()

	// Capture only values that are safe outside the Fiber handler lifecycle;
	// SetBodyStreamWriter callbacks must never touch Ctx or RequestCtx.
	clientConn := c.RequestCtx().Conn()
	heartbeatInterval := g.liveTailHeartbeat
	statusCode := resp.StatusCode
	projectID := p.ID
	userID := u.ID

	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
		var closeBodyOnce sync.Once
		closeBody := func() {
			closeBodyOnce.Do(func() {
				_ = resp.Body.Close()
			})
		}
		defer releaseSlot()
		defer closeBody()
		defer streamCancel()

		stopWatcher := startClientDisconnectWatcher(streamCtx, clientConn, streamCancel)
		defer stopWatcher()

		type tailChunk struct {
			data []byte
			err  error
		}
		chunks := make(chan tailChunk, 1)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			defer close(chunks)
			buf := make([]byte, 32*1024)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					select {
					case chunks <- tailChunk{data: chunk}:
					case <-streamCtx.Done():
						return
					}
				}
				if readErr != nil {
					select {
					case chunks <- tailChunk{err: readErr}:
					case <-streamCtx.Done():
					}
					return
				}
			}
		}()
		defer func() {
			streamCancel()
			closeBody()
			<-readDone
		}()

		var ticker *time.Ticker
		var heartbeatCh <-chan time.Time
		if heartbeatInterval > 0 {
			ticker = time.NewTicker(heartbeatInterval)
			heartbeatCh = ticker.C
			defer ticker.Stop()
		}

		fw := flushingWriter{writer: w}
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-heartbeatCh:
				if _, writeErr := fw.Write([]byte("\n")); writeErr != nil {
					g.recordProxyStreamError("/select/logsql/tail", statusCode, writeErr)
					return
				}
			case chunk, ok := <-chunks:
				if !ok {
					return
				}
				if len(chunk.data) > 0 {
					if _, writeErr := fw.Write(chunk.data); writeErr != nil {
						g.recordProxyStreamError("/select/logsql/tail", statusCode, writeErr)
						return
					}
					if ticker != nil {
						ticker.Reset(heartbeatInterval)
					}
				}
				if chunk.err != nil {
					if !errors.Is(chunk.err, io.EOF) && streamCtx.Err() == nil {
						g.logger.Warn(
							"live tail upstream stream interrupted",
							"project_id", projectID,
							"user_id", userID,
							"status", statusCode,
							"error", chunk.err,
						)
						g.recordProxyStreamError("/select/logsql/tail", statusCode, chunk.err)
					}
					return
				}
			}
		}
	})
	return nil
}

func isWatchableConn(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	switch c := conn.(type) {
	case *net.TCPConn:
		return true
	case interface{ NetConn() net.Conn }:
		return isWatchableConn(c.NetConn())
	case syscall.Conn:
		addr := conn.RemoteAddr()
		return addr != nil && strings.HasPrefix(addr.Network(), "tcp")
	default:
		return false
	}
}

// startClientDisconnectWatcher monitors a live tail TCP/TLS connection for
// client FIN/RST while fasthttp is writing the response stream. Because live
// tail sets Connection: close, fasthttp will never read another request from
// this connection, and stop() synchronously joins the goroutine before the
// stream writer returns.
func startClientDisconnectWatcher(
	ctx context.Context,
	conn net.Conn,
	cancel context.CancelFunc,
) func() {
	if !isWatchableConn(conn) {
		return func() {}
	}
	var mu sync.Mutex
	stopped := false
	done := make(chan struct{})
	var stopOnce sync.Once

	go func() {
		defer close(done)
		var buf [1]byte
		for {
			mu.Lock()
			if stopped || ctx.Err() != nil {
				mu.Unlock()
				return
			}
			// Clear fasthttp's initial ReadTimeout so an idle stream stays open
			// until the client closes or stop() sets an immediate deadline.
			_ = conn.SetReadDeadline(time.Time{})
			mu.Unlock()

			_, err := conn.Read(buf[:])
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			cancel()
			return
		}
	}()

	return func() {
		stopOnce.Do(func() {
			mu.Lock()
			stopped = true
			cancel()
			_ = conn.SetReadDeadline(time.Now())
			mu.Unlock()
			<-done
		})
	}
}

type flushingWriter struct {
	writer *bufio.Writer
}

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		return n, err
	}
	return n, w.writer.Flush()
}
