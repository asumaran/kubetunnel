package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
)

// tunnelRunner owns a single tunnel's lifecycle. Its state machine loops
// inside Run() until ctx is canceled.
type tunnelRunner struct {
	cfg    config.Tunnel
	logger *logging.Logger
	execer Execer

	mu         sync.RWMutex
	state      State
	pid        int
	startedAt  time.Time
	restarts   int
	lastError  string
	healthOK   bool
	lastHealth time.Time
	// Restart history (unix seconds) used for loop protection.
	recentRestarts []int64

	stopCh    chan struct{}
	restartCh chan struct{}
	stopped   atomic.Bool
}

// Execer lets tests inject a fake kubectl command factory.
type Execer interface {
	// Command returns an *exec.Cmd configured for `kubectl port-forward ...`.
	Command(ctx context.Context, t config.Tunnel) *exec.Cmd
}

// KubectlExecer builds real kubectl port-forward commands. Env is merged with
// os.Environ when non-nil.
type KubectlExecer struct {
	Env config.EnvironmentConfig
}

func (k KubectlExecer) Command(ctx context.Context, t config.Tunnel) *exec.Cmd {
	portArg := fmt.Sprintf("%d:%d", t.LocalPort, t.RemotePort)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+t.KubeContext,
		"port-forward",
		"-n", t.Namespace,
		t.Target,
		portArg,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = buildEnv(k.Env)
	return cmd
}

func buildEnv(ec config.EnvironmentConfig) []string {
	base := os.Environ()
	env := make(map[string]string, len(base)+len(ec.Extra))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	if len(ec.PathAdditions) > 0 {
		path := env["PATH"]
		for _, p := range ec.PathAdditions {
			if path == "" {
				path = p
			} else if !strings.Contains(path, p) {
				path = p + ":" + path
			}
		}
		env["PATH"] = path
	}
	for k, v := range ec.Extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func newTunnelRunner(cfg config.Tunnel, logger *logging.Logger, execer Execer) *tunnelRunner {
	if execer == nil {
		execer = KubectlExecer{}
	}
	return &tunnelRunner{
		cfg:       cfg,
		logger:    logger,
		execer:    execer,
		state:     StateStopped,
		stopCh:    make(chan struct{}),
		restartCh: make(chan struct{}, 1),
	}
}

// Run drives the state machine until ctx is canceled. Never returns an error —
// it logs everything instead so one broken tunnel never takes down the daemon.
func (t *tunnelRunner) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			t.logger.Daemon("error", t.cfg.Name, "panic",
				fmt.Sprintf("tunnel runner panicked: %v", r), nil)
			t.setState(StateFailing)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		default:
		}
		t.runOnce(ctx)
	}
}

// runOnce tries to start kubectl once and blocks until it exits. Then applies
// backoff or failing state as needed.
func (t *tunnelRunner) runOnce(ctx context.Context) {
	t.setState(StateStarting)
	t.logger.Daemon("info", t.cfg.Name, "starting", "launching kubectl port-forward",
		map[string]any{"local_port": t.cfg.LocalPort, "target": t.cfg.Target})

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := t.execer.Command(childCtx, t.cfg)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.recordError(fmt.Errorf("stdout pipe: %w", err))
		t.backoff(ctx)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.recordError(fmt.Errorf("stderr pipe: %w", err))
		t.backoff(ctx)
		return
	}
	if err := cmd.Start(); err != nil {
		t.recordError(fmt.Errorf("start: %w", err))
		t.backoff(ctx)
		return
	}
	t.mu.Lock()
	t.pid = cmd.Process.Pid
	t.mu.Unlock()

	readyCh := make(chan struct{}, 1)
	go t.drainStdout(stdout, readyCh)
	go t.drainStderr(stderr)

	// Wait for either: readiness, restart request, stop, or process exit.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var readinessTimeout <-chan time.Time = time.After(30 * time.Second)
	healthTicker := t.healthTicker()
	if healthTicker != nil {
		defer healthTicker.Stop()
	}
	var healthTickCh <-chan time.Time
	consecutiveHealthFails := 0

waitLoop:
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-waitCh
			t.setState(StateStopped)
			return
		case <-t.stopCh:
			cancel()
			<-waitCh
			t.setState(StateStopped)
			return
		case <-t.restartCh:
			t.logger.Daemon("info", t.cfg.Name, "restart_requested", "manual restart", nil)
			cancel()
			<-waitCh
			// Reset backoff for manual restart.
			t.mu.Lock()
			t.restarts = 0
			t.recentRestarts = nil
			t.mu.Unlock()
			return
		case <-readyCh:
			t.setState(StateRunning)
			t.mu.Lock()
			t.startedAt = time.Now()
			t.healthOK = true
			t.mu.Unlock()
			t.logger.Daemon("info", t.cfg.Name, "ready", "tunnel ready",
				map[string]any{"pid": t.pid})
			readyCh = nil
			readinessTimeout = nil
			if healthTicker != nil {
				healthTickCh = healthTicker.C
			}
		case <-readinessTimeout:
			t.logger.Daemon("warn", t.cfg.Name, "readiness_timeout",
				"kubectl did not report 'Forwarding from' within 30s", nil)
			cancel()
			<-waitCh
			t.recordError(fmt.Errorf("readiness timeout"))
			break waitLoop
		case <-healthTickCh:
			if !t.checkHealth() {
				consecutiveHealthFails++
				t.mu.Lock()
				t.healthOK = false
				t.lastHealth = time.Now()
				t.mu.Unlock()
				t.logger.Daemon("warn", t.cfg.Name, "health_check_failed",
					"health check failed",
					map[string]any{"consecutive": consecutiveHealthFails})
				threshold := 3
				if t.cfg.HealthCheck != nil && t.cfg.HealthCheck.FailThreshold > 0 {
					threshold = t.cfg.HealthCheck.FailThreshold
				}
				if consecutiveHealthFails >= threshold {
					t.logger.Daemon("warn", t.cfg.Name, "health_threshold_exceeded",
						"killing kubectl due to failed health checks", nil)
					cancel()
					<-waitCh
					t.recordError(fmt.Errorf("health check threshold exceeded"))
					break waitLoop
				}
			} else {
				consecutiveHealthFails = 0
				t.mu.Lock()
				t.healthOK = true
				t.lastHealth = time.Now()
				t.mu.Unlock()
			}
		case err := <-waitCh:
			if err != nil {
				t.recordError(fmt.Errorf("kubectl exited: %w", err))
			} else {
				t.recordError(fmt.Errorf("kubectl exited cleanly"))
			}
			break waitLoop
		}
	}

	// If the tunnel had been Running long enough we reset backoff.
	t.mu.Lock()
	if !t.startedAt.IsZero() && time.Since(t.startedAt) > 5*time.Minute {
		t.restarts = 0
	}
	t.mu.Unlock()

	if t.tooManyRestarts() {
		t.setState(StateFailing)
		t.logger.Daemon("error", t.cfg.Name, "failing",
			"too many restarts — pausing 5m before retry", nil)
		select {
		case <-ctx.Done():
		case <-t.stopCh:
		case <-time.After(5 * time.Minute):
		}
		t.mu.Lock()
		t.recentRestarts = nil
		t.restarts = 0
		t.mu.Unlock()
		return
	}
	t.backoff(ctx)
}

func (t *tunnelRunner) drainStdout(r io.Reader, readyCh chan struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	needle := fmt.Sprintf("Forwarding from 127.0.0.1:%d", t.cfg.LocalPort)
	readyFired := false
	for scanner.Scan() {
		line := scanner.Text()
		t.logger.Kubectl(t.cfg.Name, line)
		if !readyFired && strings.Contains(line, needle) {
			readyFired = true
			select {
			case readyCh <- struct{}{}:
			default:
			}
		}
	}
}

func (t *tunnelRunner) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Text()
		t.logger.Kubectl(t.cfg.Name, "[stderr] "+line)
	}
}

func (t *tunnelRunner) healthTicker() *time.Ticker {
	if t.cfg.HealthCheck == nil || t.cfg.HealthCheck.Path == "" {
		return nil
	}
	return time.NewTicker(t.cfg.HealthCheck.Interval)
}

func (t *tunnelRunner) checkHealth() bool {
	hc := t.cfg.HealthCheck
	if hc == nil || hc.Path == "" {
		return true
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", t.cfg.LocalPort, hc.Path)
	client := &http.Client{Timeout: hc.Timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	if t.cfg.Hostname != "" {
		req.Host = t.cfg.Hostname
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

func (t *tunnelRunner) backoff(ctx context.Context) {
	t.mu.Lock()
	t.restarts++
	attempt := t.restarts
	t.recentRestarts = append(t.recentRestarts, time.Now().Unix())
	t.mu.Unlock()

	t.setState(StateBackoff)
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	base := float64(int(1) << shift) // 1,2,4,8,16,32,64
	if base > 60 {
		base = 60
	}
	// Jitter: ±25%
	jitter := base * (0.75 + rand.Float64()*0.5)
	d := time.Duration(jitter * float64(time.Second))
	t.logger.Daemon("info", t.cfg.Name, "backoff",
		"waiting before restart",
		map[string]any{"delay_s": int(d.Seconds()), "attempt": attempt})
	select {
	case <-ctx.Done():
	case <-t.stopCh:
	case <-t.restartCh:
	case <-time.After(d):
	}
}

func (t *tunnelRunner) tooManyRestarts() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-60 * time.Second).Unix()
	kept := t.recentRestarts[:0]
	for _, ts := range t.recentRestarts {
		if ts >= cutoff {
			kept = append(kept, ts)
		}
	}
	t.recentRestarts = kept
	return len(t.recentRestarts) > 10
}

func (t *tunnelRunner) setState(s State) {
	t.mu.Lock()
	t.state = s
	if s != StateRunning {
		t.pid = 0
	}
	if s == StateStopped {
		t.startedAt = time.Time{}
	}
	t.mu.Unlock()
}

func (t *tunnelRunner) recordError(err error) {
	t.mu.Lock()
	t.lastError = err.Error()
	t.mu.Unlock()
	t.logger.Daemon("error", t.cfg.Name, "error", err.Error(), nil)
}

func (t *tunnelRunner) Status() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := Status{
		Name:       t.cfg.Name,
		Hostname:   t.cfg.Hostname,
		LocalPort:  t.cfg.LocalPort,
		State:      t.state,
		PID:        t.pid,
		StartedAt:  t.startedAt,
		Restarts:   t.restarts,
		LastError:  t.lastError,
		HealthOK:   t.healthOK,
		LastHealth: t.lastHealth,
	}
	if !t.startedAt.IsZero() && t.state == StateRunning {
		s.Uptime = time.Since(t.startedAt).Round(time.Second).String()
	}
	return s
}

func (t *tunnelRunner) Restart() {
	select {
	case t.restartCh <- struct{}{}:
	default:
	}
}

func (t *tunnelRunner) Stop() {
	if t.stopped.Swap(true) {
		return
	}
	close(t.stopCh)
}

func (t *tunnelRunner) IsReady() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == StateRunning
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
