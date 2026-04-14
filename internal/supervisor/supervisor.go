package supervisor

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
)

// Supervisor manages one tunnelRunner per tunnel.
type Supervisor struct {
	logger *logging.Logger
	execer Execer

	mu      sync.RWMutex
	runners map[string]*tunnelRunner
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
	ctx     context.Context
}

func New(logger *logging.Logger, execer Execer) *Supervisor {
	return &Supervisor{
		logger:  logger,
		execer:  execer,
		runners: make(map[string]*tunnelRunner),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Start launches all runners defined in cfg. Safe to call once.
func (s *Supervisor) Start(ctx context.Context, cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return fmt.Errorf("supervisor already started")
	}
	s.ctx = ctx
	for _, t := range cfg.Tunnels {
		s.logger.RegisterTunnel(t.Name)
		s.startTunnelLocked(t)
	}
	return nil
}

func (s *Supervisor) startTunnelLocked(t config.Tunnel) {
	runner := newTunnelRunner(t, s.logger, s.execer)
	childCtx, cancel := context.WithCancel(s.ctx)
	s.runners[t.Name] = runner
	s.cancels[t.Name] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runner.Run(childCtx)
	}()
}

// Reload applies a new config: stops removed/changed tunnels, starts new ones,
// leaves unchanged tunnels alone.
func (s *Supervisor) Reload(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]config.Tunnel)
	for _, t := range cfg.Tunnels {
		want[t.Name] = t
	}
	// Stop removed or changed tunnels.
	for name, runner := range s.runners {
		if newT, ok := want[name]; !ok {
			s.stopTunnelLocked(name)
		} else if !tunnelEqual(runner.cfg, newT) {
			s.stopTunnelLocked(name)
		}
	}
	// Start new or changed tunnels.
	for name, t := range want {
		if _, ok := s.runners[name]; !ok {
			s.logger.RegisterTunnel(name)
			s.startTunnelLocked(t)
		}
	}
}

func (s *Supervisor) stopTunnelLocked(name string) {
	runner, ok := s.runners[name]
	if !ok {
		return
	}
	runner.Stop()
	if cancel, ok := s.cancels[name]; ok {
		cancel()
	}
	delete(s.runners, name)
	delete(s.cancels, name)
}

// Stop stops all tunnels and waits for their runners to exit.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	for name := range s.runners {
		s.stopTunnelLocked(name)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Restart forces a specific tunnel to restart.
func (s *Supervisor) Restart(name string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runners[name]
	if !ok {
		return fmt.Errorf("tunnel %q not found", name)
	}
	r.Restart()
	return nil
}

// Status returns a snapshot of all tunnels, sorted by name for a stable
// display order.
func (s *Supervisor) Status() []Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Status, 0, len(s.runners))
	for _, r := range s.runners {
		out = append(out, r.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StatusFor returns a single tunnel's status.
func (s *Supervisor) StatusFor(name string) (Status, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runners[name]
	if !ok {
		return Status{}, false
	}
	return r.Status(), true
}

// IsReady reports whether the given tunnel is in Running state (for the proxy).
func (s *Supervisor) IsReady(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runners[name]
	if !ok {
		return false
	}
	return r.IsReady()
}

func tunnelEqual(a, b config.Tunnel) bool {
	if a.Name != b.Name || a.Hostname != b.Hostname ||
		a.KubeContext != b.KubeContext || a.Namespace != b.Namespace ||
		a.Target != b.Target || a.RemotePort != b.RemotePort ||
		a.LocalPort != b.LocalPort {
		return false
	}
	return true
}
