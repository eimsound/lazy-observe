package gateway

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultLiveTailMaxGlobal             = 64
	defaultLiveTailMaxPerUser            = 4
	defaultLiveTailMaxPerProject         = 16
	defaultLiveTailMaxDuration           = 30 * time.Minute
	defaultLiveTailHeartbeat             = 15 * time.Second
	defaultLiveTailResponseHeaderTimeout = 30 * time.Second
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

// startClientDisconnectWatcher monitors a streaming TCP/TLS connection for
// client FIN/RST while fasthttp is writing the response stream. Because
// streamProxy sets Connection: close, fasthttp will never read another request
// from this connection, and stop() synchronously joins the goroutine before the
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
