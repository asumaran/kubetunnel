// Package proxy implements the local HTTPS reverse proxy that terminates TLS
// using mkcert-issued certs and routes to the appropriate kubectl port-forward
// by Host header.
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asumaran/kubetunnel/internal/certs"
	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
)

// ReadinessChecker reports whether a named tunnel is currently ready.
type ReadinessChecker interface {
	IsReady(name string) bool
}

// Backend ties a hostname to a tunnel.
type backend struct {
	tunnelName string
	hostname   string
	proxy      *httputil.ReverseProxy
}

// Server is the HTTPS reverse proxy. It is safe to Update() the backend map at
// runtime (hot reload).
type Server struct {
	logger   *logging.Logger
	ready    ReadinessChecker
	srv      *http.Server
	listener string

	mu       sync.RWMutex
	backends map[string]*backend // keyed by lowercase hostname
}

func New(listenAddr string, logger *logging.Logger, ready ReadinessChecker, loader *certs.Loader) *Server {
	s := &Server{
		logger:   logger,
		ready:    ready,
		listener: listenAddr,
		backends: make(map[string]*backend),
	}
	tlsCfg := &tls.Config{
		GetCertificate: loader.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	s.srv = &http.Server{
		Addr:         listenAddr,
		Handler:      s,
		TLSConfig:    tlsCfg,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// Update rebuilds the backend routing map from the given tunnels.
func (s *Server) Update(tunnels []config.Tunnel) error {
	nb := make(map[string]*backend, len(tunnels))
	for _, t := range tunnels {
		targetURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", t.LocalPort))
		if err != nil {
			return fmt.Errorf("bad local url for %s: %w", t.Name, err)
		}
		b := &backend{
			tunnelName: t.Name,
			hostname:   t.Hostname,
		}
		p := &httputil.ReverseProxy{
			Director: s.director(t, targetURL),
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				s.logger.Daemon("error", t.Name, "proxy_error", err.Error(), map[string]any{
					"path": r.URL.Path,
				})
				writeBadGateway(w, t.Name, err.Error())
			},
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
		b.proxy = p
		nb[toKey(t.Hostname)] = b
	}
	s.mu.Lock()
	s.backends = nb
	s.mu.Unlock()
	return nil
}

func (s *Server) director(t config.Tunnel, target *url.URL) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		// Strip a leading path prefix to mirror what an upstream Istio
		// VirtualService would rewrite before reaching the pod.
		if t.StripPrefix != "" {
			trimmed := strings.TrimPrefix(req.URL.Path, t.StripPrefix)
			if trimmed == req.URL.Path {
				// Prefix didn't match — leave path alone so 404s look normal.
			} else {
				if trimmed == "" || trimmed[0] != '/' {
					trimmed = "/" + trimmed
				}
				req.URL.Path = trimmed
				if req.URL.RawPath != "" {
					req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, t.StripPrefix)
					if req.URL.RawPath == "" || req.URL.RawPath[0] != '/' {
						req.URL.RawPath = "/" + req.URL.RawPath
					}
				}
			}
		}
		// Preserve the original Host header so the pod sees the real hostname.
		// req.Host remains as the client set it.
		for k, v := range t.Headers {
			req.Header.Set(k, v)
		}
		// Simulate what Istio/Envoy would send.
		if req.Header.Get("X-Forwarded-Proto") == "" {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
		if req.Header.Get("X-Forwarded-Host") == "" {
			req.Header.Set("X-Forwarded-Host", req.Host)
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	host := toKey(stripPort(r.Host))
	s.mu.RLock()
	b, ok := s.backends[host]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown host %q", r.Host), http.StatusNotFound)
		s.logger.Access("", map[string]any{
			"method":      r.Method,
			"host":        r.Host,
			"path":        r.URL.Path,
			"status":      404,
			"duration_ms": time.Since(start).Milliseconds(),
		})
		return
	}
	if s.ready != nil && !s.ready.IsReady(b.tunnelName) {
		writeUnavailable(w, b.tunnelName)
		s.logger.Access(b.tunnelName, map[string]any{
			"method":      r.Method,
			"host":        r.Host,
			"path":        r.URL.Path,
			"status":      503,
			"duration_ms": time.Since(start).Milliseconds(),
			"reason":      "tunnel_not_ready",
		})
		return
	}
	rw := &recordingWriter{ResponseWriter: w, status: 200}
	b.proxy.ServeHTTP(rw, r)
	s.logger.Access(b.tunnelName, map[string]any{
		"method":      r.Method,
		"host":        r.Host,
		"path":        r.URL.Path,
		"status":      rw.status,
		"bytes":       rw.bytes.Load(),
		"duration_ms": time.Since(start).Milliseconds(),
		"user_agent":  r.Header.Get("User-Agent"),
	})
}

// Start begins listening. Blocks until the server exits.
func (s *Server) Start() error {
	return s.srv.ListenAndServeTLS("", "")
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  atomic.Int64
}

func (rw *recordingWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(p []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(p)
	rw.bytes.Add(int64(n))
	return n, err
}

func writeBadGateway(w http.ResponseWriter, tunnel, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `<!doctype html><html><body style="font-family:system-ui;padding:2em">
<h1>502 Bad Gateway</h1>
<p>kubetunnel could not reach <b>%s</b>.</p>
<pre>%s</pre>
<p>Run <code>tunnelctl status</code> or <code>tunnelctl logs --name %s</code> for details.</p>
</body></html>`, tunnel, reason, tunnel)
}

func writeUnavailable(w http.ResponseWriter, tunnel string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `<!doctype html><html><body style="font-family:system-ui;padding:2em">
<h1>503 Service Unavailable</h1>
<p>Tunnel <b>%s</b> is reconnecting. Retry in a few seconds.</p>
</body></html>`, tunnel)
}

func toKey(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func stripPort(hostHeader string) string {
	for i := 0; i < len(hostHeader); i++ {
		if hostHeader[i] == ':' {
			return hostHeader[:i]
		}
	}
	return hostHeader
}
