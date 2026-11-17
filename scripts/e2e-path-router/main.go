// Command e2e-path-router models the path-aware internal load balancer used in
// front of split VictoriaLogs and VictoriaTraces cluster frontends.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "", "HTTP listen address")
	insertURL := flag.String("insert-url", "", "base URL for /insert/*")
	selectURL := flag.String("select-url", "", "base URL for select and health requests")
	selectDelay := flag.Duration("select-delay", 0, "optional delay before GET select requests")
	flag.Parse()
	if *listen == "" || *insertURL == "" || *selectURL == "" || *selectDelay < 0 {
		fatal("-listen, -insert-url and -select-url are required; -select-delay must not be negative")
	}

	insert := reverseProxy(*insertURL)
	selects := reverseProxy(*selectURL)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/insert/") {
			insert.ServeHTTP(w, r)
			return
		}
		if *selectDelay > 0 && r.Method == http.MethodGet {
			time.Sleep(*selectDelay)
		}
		selects.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("serve: %v", err)
	}
}

func reverseProxy(raw string) *httputil.ReverseProxy {
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		fatal("invalid upstream URL %q", raw)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
	}
	return proxy
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "e2e-path-router: "+format+"\n", args...)
	os.Exit(2)
}
