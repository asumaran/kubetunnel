package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Logger is the central logging facility for the daemon. It fans out each
// Entry to:
//  1. The on-disk rotating file for its Stream (daemon/access/kubectl).
//  2. A global ring buffer (for dashboard's "all streams" view).
//  3. A per-tunnel ring buffer (for filtered views).
//  4. Per-stream global subscribers via the returned channels (not used
//     directly; consumers subscribe via the ring buffers).
type Logger struct {
	dir string

	daemonFile  io.Writer
	accessFile  io.Writer
	kubectlFile io.Writer

	mu          sync.RWMutex
	globalRing  *RingBuffer
	tunnelRings map[string]*RingBuffer
	tunnelCap   int
}

type Options struct {
	Dir        string
	MaxSizeMB  int
	MaxFiles   int
	MaxAgeDays int
	Compress   bool
	BufferCap  int
}

func New(opts Options) (*Logger, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("logging dir required")
	}
	if opts.BufferCap == 0 {
		opts.BufferCap = 1000
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	mk := func(name string) io.Writer {
		return &lumberjack.Logger{
			Filename:   filepath.Join(opts.Dir, name),
			MaxSize:    nonZero(opts.MaxSizeMB, 10),
			MaxBackups: nonZero(opts.MaxFiles, 5),
			MaxAge:     nonZero(opts.MaxAgeDays, 30),
			Compress:   opts.Compress,
		}
	}
	return &Logger{
		dir:         opts.Dir,
		daemonFile:  mk("daemon.log"),
		accessFile:  mk("access.log"),
		kubectlFile: mk("kubectl.log"),
		globalRing:  NewRingBuffer(opts.BufferCap * 4),
		tunnelRings: make(map[string]*RingBuffer),
		tunnelCap:   opts.BufferCap,
	}, nil
}

func nonZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// RegisterTunnel ensures a per-tunnel ring buffer exists. Safe to call
// multiple times.
func (l *Logger) RegisterTunnel(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.tunnelRings[name]; !ok {
		l.tunnelRings[name] = NewRingBuffer(l.tunnelCap)
	}
}

// TunnelRing returns the ring buffer for the given tunnel, creating it if
// necessary. Always non-nil.
func (l *Logger) TunnelRing(name string) *RingBuffer {
	l.mu.RLock()
	rb, ok := l.tunnelRings[name]
	l.mu.RUnlock()
	if ok {
		return rb
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rb, ok := l.tunnelRings[name]; ok {
		return rb
	}
	rb = NewRingBuffer(l.tunnelCap)
	l.tunnelRings[name] = rb
	return rb
}

// GlobalRing returns the ring that captures entries for all tunnels/streams.
func (l *Logger) GlobalRing() *RingBuffer {
	return l.globalRing
}

// Write dispatches an entry to its stream file and ring buffers.
func (l *Logger) Write(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// File output
	var w io.Writer
	switch e.Stream {
	case StreamAccess:
		w = l.accessFile
	case StreamKubectl:
		w = l.kubectlFile
	default:
		e.Stream = StreamDaemon
		w = l.daemonFile
	}
	if data, err := e.MarshalJSON(); err == nil {
		data = append(data, '\n')
		_, _ = w.Write(data)
	}
	// Ring buffers
	l.globalRing.Append(e)
	if e.Tunnel != "" {
		l.TunnelRing(e.Tunnel).Append(e)
	}
}

// Convenience helpers for the supervisor/proxy.

func (l *Logger) Daemon(level, tunnel, event, msg string, fields map[string]any) {
	l.Write(Entry{
		Time:   time.Now(),
		Level:  level,
		Stream: StreamDaemon,
		Tunnel: tunnel,
		Event:  event,
		Msg:    msg,
		Fields: fields,
	})
}

func (l *Logger) Access(tunnel string, fields map[string]any) {
	l.Write(Entry{
		Time:   time.Now(),
		Level:  "info",
		Stream: StreamAccess,
		Tunnel: tunnel,
		Fields: fields,
	})
}

func (l *Logger) Kubectl(tunnel, line string) {
	l.Write(Entry{
		Time:    time.Now(),
		Stream:  StreamKubectl,
		Tunnel:  tunnel,
		Msg:     line,
		RawLine: line,
	})
}

// TunnelNames returns the list of registered tunnel names.
func (l *Logger) TunnelNames() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.tunnelRings))
	for k := range l.tunnelRings {
		out = append(out, k)
	}
	return out
}
