package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func runLoad(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("url", "http://127.0.0.1:3320", "Fiber example base URL")
	duration := flags.Duration("duration", 10*time.Second, "load generation duration")
	concurrency := flags.Int("concurrency", 4, "number of concurrent workers")
	timeout := flags.Duration("timeout", 2*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *duration <= 0 || *concurrency <= 0 || *timeout <= 0 {
		// Best-effort usage note: a failed write to stderr must not change the
		// exit status the example already decided on.
		_, _ = fmt.Fprintln(stderr, "duration, concurrency, and timeout must be greater than zero")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	client := &http.Client{Timeout: *timeout}
	url := strings.TrimRight(*baseURL, "/")
	started := time.Now()
	var sequence atomic.Int64
	var completed atomic.Int64
	var transportErrors atomic.Int64
	var status2xx atomic.Int64
	var status5xx atomic.Int64
	var workers sync.WaitGroup

	for range *concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ctx.Err() == nil {
				n := sequence.Add(1)
				path := loadPath(n)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
				if err != nil {
					transportErrors.Add(1)
					continue
				}
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						transportErrors.Add(1)
					}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				completed.Add(1)
				if resp.StatusCode >= 500 {
					status5xx.Add(1)
				} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					status2xx.Add(1)
				}
			}
		}()
	}
	workers.Wait()

	elapsed := time.Since(started)
	requests := completed.Load()
	fmt.Fprintf(stdout, "completed=%d 2xx=%d 5xx=%d transport_errors=%d elapsed=%s rps=%.1f\n",
		requests,
		status2xx.Load(),
		status5xx.Load(),
		transportErrors.Load(),
		elapsed.Round(time.Millisecond),
		float64(requests)/elapsed.Seconds(),
	)
	if transportErrors.Load() > 0 {
		return 1
	}
	return 0
}

func loadPath(sequence int64) string {
	switch sequence % 10 {
	case 0:
		return "/performance/error"
	case 1, 2, 3:
		return "/performance/slow"
	case 4:
		return "/trace/load-" + strconv.FormatInt(sequence, 10)
	default:
		return "/performance/fast"
	}
}
