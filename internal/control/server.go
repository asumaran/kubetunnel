package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/logquery"
	"github.com/asumaran/kubetunnel/internal/supervisor"
)

// DaemonAPI is the minimal interface the control server needs from the daemon.
type DaemonAPI interface {
	Config() *config.Config
	Supervisor() *supervisor.Supervisor
	Logger() *logging.Logger
	Reload() error
	Shutdown()
}

type Server struct {
	api      DaemonAPI
	socket   string
	listener net.Listener
	http     *http.Server
	mu       sync.Mutex
	closed   bool
}

func NewServer(api DaemonAPI, socketPath string) *Server {
	return &Server{api: api, socket: socketPath}
}

func (s *Server) Start() error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// Remove stale socket file.
	if _, err := os.Stat(s.socket); err == nil {
		_ = os.Remove(s.socket)
	}
	l, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.socket, err)
	}
	// World-writable so tunnelctl run as the user can talk to a root daemon.
	_ = os.Chmod(s.socket, 0o666)
	s.listener = l
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/reload", s.handleReload)
	mux.HandleFunc("/restart", s.handleRestart)
	mux.HandleFunc("/stop", s.handleStop)
	mux.HandleFunc("/shutdown", s.handleShutdown)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/logs/stream", s.handleLogsStream)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/debug", s.handleDebug)
	s.http = &http.Server{
		Handler:     mux,
		ReadTimeout: 0, // Streaming endpoints need no read timeout.
	}
	go func() {
		_ = s.http.Serve(l)
	}()
	return nil
}

func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.http != nil {
		_ = s.http.Shutdown(ctx)
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socket)
}

// --- handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, StatusResponse{Tunnels: s.api.Supervisor().Status()})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := s.api.Reload(); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "reloaded"})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, 400, "name required")
		return
	}
	if err := s.api.Supervisor().Restart(name); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "restarting", "name": name})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "stop not implemented per-tunnel"})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "shutting down"})
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.api.Shutdown()
	}()
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	filter := q.Get("filter")
	tailStr := q.Get("tail")
	tail := 200
	if tailStr != "" {
		if n, err := strconv.Atoi(tailStr); err == nil && n > 0 {
			tail = n
		}
	}
	pred, err := logquery.Parse(filter)
	if err != nil {
		writeError(w, 400, "invalid filter: "+err.Error())
		return
	}
	logger := s.api.Logger()
	var entries []logging.Entry
	if name != "" {
		entries = logger.TunnelRing(name).Snapshot(0)
	} else {
		entries = logger.GlobalRing().Snapshot(0)
	}
	// Apply filter, then tail.
	filtered := make([]logging.Entry, 0, len(entries))
	for _, e := range entries {
		if pred(e) {
			filtered = append(filtered, e)
		}
	}
	if tail > 0 && len(filtered) > tail {
		filtered = filtered[len(filtered)-tail:]
	}
	writeJSON(w, 200, LogResponse{Entries: filtered})
}

// handleLogsStream serves a live SSE stream of log entries.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	filter := q.Get("filter")
	pred, err := logquery.Parse(filter)
	if err != nil {
		writeError(w, 400, "invalid filter: "+err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher.Flush()

	logger := s.api.Logger()
	var rb *logging.RingBuffer
	if name != "" {
		rb = logger.TunnelRing(name)
	} else {
		rb = logger.GlobalRing()
	}
	// Replay last 100 entries before starting the live tail.
	for _, e := range rb.Snapshot(100) {
		if pred(e) {
			writeSSE(w, flusher, "log", e)
		}
	}
	ch, unsub := rb.Subscribe()
	defer unsub()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if pred(e) {
				writeSSE(w, flusher, "log", e)
			}
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleEvents emits tunnel state-change events via SSE. For simplicity, it
// polls supervisor status every 500ms and emits diffs.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher.Flush()

	sup := s.api.Supervisor()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	// Send initial state.
	writeSSE(w, flusher, "status", StatusResponse{Tunnels: sup.Status()})

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			writeSSE(w, flusher, "status", StatusResponse{Tunnels: sup.Status()})
		}
	}
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, 400, "name required")
		return
	}
	st, ok := s.api.Supervisor().StatusFor(name)
	if !ok {
		writeError(w, 404, "tunnel not found")
		return
	}
	t := s.api.Config().FindTunnel(name)
	entries := s.api.Logger().TunnelRing(name).Snapshot(500)
	resp := map[string]any{
		"name":    name,
		"status":  st,
		"config":  t,
		"entries": entries,
	}
	writeJSON(w, 200, resp)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, Error{Error: msg})
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flusher.Flush()
}
