package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
)

// fakeExecer runs a bash script that prints the ready marker and then
// optionally sleeps or exits immediately.
type fakeExecer struct {
	script string
}

func (f fakeExecer) Command(ctx context.Context, t config.Tunnel) *exec.Cmd {
	s := f.script
	if s == "" {
		s = fmt.Sprintf("printf 'Forwarding from 127.0.0.1:%d\\n'; sleep 60", t.LocalPort)
	}
	return exec.CommandContext(ctx, "bash", "-c", s)
}

func newTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(logging.Options{Dir: t.TempDir(), BufferCap: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func testConfig(name string) *config.Config {
	return &config.Config{
		Tunnels: []config.Tunnel{{
			Name:        name,
			Hostname:    name + ".test",
			KubeContext: "fake",
			Namespace:   "ns",
			Target:      "svc/fake",
			RemotePort:  80,
			LocalPort:   19001,
		}},
	}
}

func TestSupervisorBecomesReady(t *testing.T) {
	logger := newTestLogger(t)
	s := New(logger, fakeExecer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, testConfig("foo")); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Wait for ready.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := s.StatusFor("foo"); ok && st.State == StateRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tunnel never reached Running; final=%v", s.Status())
}

func TestSupervisorRestartsOnExit(t *testing.T) {
	logger := newTestLogger(t)
	// Script exits after 100ms — supervisor must restart.
	s := New(logger, fakeExecer{script: "printf 'Forwarding from 127.0.0.1:19001\\n'; sleep 0.1; exit 1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, testConfig("foo")); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Wait long enough to observe at least one backoff+restart cycle.
	time.Sleep(2500 * time.Millisecond)
	st, _ := s.StatusFor("foo")
	if st.Restarts < 1 {
		t.Errorf("expected >=1 restart, got %d", st.Restarts)
	}
}

func TestSupervisorReloadAddsAndRemoves(t *testing.T) {
	logger := newTestLogger(t)
	s := New(logger, fakeExecer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.Start(ctx, testConfig("foo"))
	defer s.Stop()

	// Wait for initial tunnel to be running.
	time.Sleep(500 * time.Millisecond)

	cfg2 := &config.Config{
		Tunnels: []config.Tunnel{
			{Name: "bar", Hostname: "bar.test", KubeContext: "fake", Namespace: "ns", Target: "svc/x", RemotePort: 80, LocalPort: 19002},
		},
	}
	s.Reload(cfg2)
	time.Sleep(500 * time.Millisecond)

	if _, ok := s.StatusFor("foo"); ok {
		t.Error("foo should have been removed")
	}
	if _, ok := s.StatusFor("bar"); !ok {
		t.Error("bar should have been added")
	}
}

func TestSupervisorManualRestart(t *testing.T) {
	logger := newTestLogger(t)
	s := New(logger, fakeExecer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.Start(ctx, testConfig("foo"))
	defer s.Stop()

	// Wait until running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := s.StatusFor("foo"); st.State == StateRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := s.Restart("foo"); err != nil {
		t.Fatal(err)
	}
	// Give it time to restart.
	time.Sleep(1 * time.Second)
	if st, _ := s.StatusFor("foo"); st.State != StateRunning {
		t.Errorf("expected Running after restart, got %s", st.State)
	}
}

func waitForState(t *testing.T, s *Supervisor, name string, want State, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if st, ok := s.StatusFor(name); ok && st.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s; final=%v", name, want, s.Status())
}

// countingExecer runs script and counts how many times kubectl was spawned.
type countingExecer struct {
	script func() string
	spawns *atomic.Int64
}

func (c countingExecer) Command(ctx context.Context, t config.Tunnel) *exec.Cmd {
	c.spawns.Add(1)
	return exec.CommandContext(ctx, "bash", "-c", c.script())
}

const authFailScript = "echo 'Unable to connect to the server: getting credentials: exec: executable gke-gcloud-auth-plugin failed with exit code 1' >&2; exit 1"

func twoTunnelConfig() *config.Config {
	cfg := testConfig("foo")
	cfg.Tunnels = append(cfg.Tunnels, config.Tunnel{
		Name: "bar", Hostname: "bar.test", KubeContext: "fake", Namespace: "ns",
		Target: "svc/bar", RemotePort: 80, LocalPort: 19001,
	})
	return cfg
}

// Expired credentials must pause the whole kube context instead of letting
// every tunnel respawn kubectl forever.
func TestSupervisorPausesContextOnAuthFailure(t *testing.T) {
	logger := newTestLogger(t)
	var spawns atomic.Int64
	s := New(logger, countingExecer{script: func() string { return authFailScript }, spawns: &spawns})
	s.gate = newAuthGate(time.Hour) // no probe during the test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, twoTunnelConfig()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	waitForState(t, s, "foo", StateAuthRequired, 3*time.Second)
	waitForState(t, s, "bar", StateAuthRequired, 3*time.Second)

	before := spawns.Load()
	time.Sleep(1500 * time.Millisecond)
	if after := spawns.Load(); after != before {
		t.Errorf("kubectl kept spawning while the context was paused: %d -> %d", before, after)
	}
	if before > 2 {
		t.Errorf("expected one spawn per tunnel before pausing, got %d", before)
	}
	st, _ := s.StatusFor("foo")
	if !strings.Contains(st.LastError, "credentials rejected") {
		t.Errorf("last error should explain the pause, got %q", st.LastError)
	}
}

// Once credentials work again, a single probe must bring every tunnel of the
// context back without a manual restart.
func TestSupervisorResumesWhenProbeSucceeds(t *testing.T) {
	logger := newTestLogger(t)
	var spawns atomic.Int64
	var fixed atomic.Bool
	s := New(logger, countingExecer{spawns: &spawns, script: func() string {
		if fixed.Load() {
			return "printf 'Forwarding from 127.0.0.1:19001\\n'; sleep 60"
		}
		return authFailScript
	}})
	s.gate = newAuthGate(300 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, twoTunnelConfig()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	waitForState(t, s, "foo", StateAuthRequired, 3*time.Second)
	waitForState(t, s, "bar", StateAuthRequired, 3*time.Second)

	// While credentials stay broken only one tunnel probes per interval.
	before := spawns.Load()
	time.Sleep(1 * time.Second)
	if probes := spawns.Load() - before; probes < 1 || probes > 4 {
		t.Errorf("expected 1-4 probes in 1s at a 300ms interval, got %d", probes)
	}

	fixed.Store(true)
	waitForState(t, s, "foo", StateRunning, 3*time.Second)
	waitForState(t, s, "bar", StateRunning, 3*time.Second)
}

// A manual restart releases the paused context right away.
func TestSupervisorManualRestartReleasesAuthPause(t *testing.T) {
	logger := newTestLogger(t)
	var spawns atomic.Int64
	var fixed atomic.Bool
	s := New(logger, countingExecer{spawns: &spawns, script: func() string {
		if fixed.Load() {
			return "printf 'Forwarding from 127.0.0.1:19001\\n'; sleep 60"
		}
		return authFailScript
	}})
	s.gate = newAuthGate(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, twoTunnelConfig()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	waitForState(t, s, "foo", StateAuthRequired, 3*time.Second)
	waitForState(t, s, "bar", StateAuthRequired, 3*time.Second)

	fixed.Store(true)
	if err := s.Restart("foo"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, "foo", StateRunning, 3*time.Second)
	waitForState(t, s, "bar", StateRunning, 3*time.Second)
}

// The backoff must keep growing while a tunnel that once worked keeps failing.
func TestBackoffGrowsAfterTunnelWasEverRunning(t *testing.T) {
	logger := newTestLogger(t)
	logger.RegisterTunnel("foo")
	r := newTunnelRunner(testConfig("foo").Tunnels[0], logger,
		fakeExecer{script: "printf 'Forwarding from 127.0.0.1:19001\\n'; exit 1"})
	// The tunnel worked long ago; that must not reset the restart counter.
	r.startedAt = time.Now().Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if r.Status().Restarts >= 3 {
			cancel()
			<-done
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("restart counter stuck at %d: backoff is being reset on every failure", r.Status().Restarts)
}
