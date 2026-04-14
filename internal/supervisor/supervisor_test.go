package supervisor

import (
	"context"
	"fmt"
	"os/exec"
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
