package supervisor

import (
	"strings"
	"sync"
	"time"
)

// defaultAuthProbeInterval is how often a single tunnel is let through to
// check whether the credentials of a blocked kube context work again.
const defaultAuthProbeInterval = 2 * time.Minute

// authFailureMarkers are substrings of kubectl's stderr that mean the
// credentials of the kube context are rejected and a human has to log in
// again. Retrying cannot fix any of them.
var authFailureMarkers = []string{
	"getting credentials: exec:",
	"Reauthentication failed",
	"gcloud auth login",
	"invalid_grant",
}

func isAuthFailure(line string) bool {
	for _, m := range authFailureMarkers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

// authGate stops a credentials outage from turning into a respawn storm.
//
// Every tunnel of a kube context shares the same credentials, so when they
// expire all of them fail at once and keep failing until someone logs in
// again. Each attempt spawns kubectl plus its credential helper, which is
// expensive; a few dozen tunnels retrying forever can saturate the machine.
// Once a context is tripped, its tunnels wait here and only one of them at a
// time is let through, every probeInterval, to find out whether the
// credentials are back.
type authGate struct {
	probeInterval time.Duration
	// notify, when set, tells the user about a context being blocked or
	// released. It is called from its own goroutine.
	notify func(title, message string)

	mu       sync.Mutex
	contexts map[string]*authGateEntry
}

type authGateEntry struct {
	// cleared is closed when the context is unblocked.
	cleared   chan struct{}
	nextProbe time.Time
	probing   bool
}

func newAuthGate(probeInterval time.Duration) *authGate {
	if probeInterval <= 0 {
		probeInterval = defaultAuthProbeInterval
	}
	return &authGate{
		probeInterval: probeInterval,
		contexts:      make(map[string]*authGateEntry),
	}
}

// admit reports what a tunnel of kubeContext may do right now.
//
//   - open: the context is not blocked, start normally.
//   - probe: the context is blocked and the caller is the one attempt allowed
//     through; it must report the outcome with trip or clear.
//   - otherwise the caller has to wait: until cleared is closed or wait
//     elapses, whichever comes first, and then ask again.
func (g *authGate) admit(kubeContext string) (open, probe bool, cleared <-chan struct{}, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, blocked := g.contexts[kubeContext]
	if !blocked {
		return true, false, nil, 0
	}
	now := time.Now()
	if !e.probing && !now.Before(e.nextProbe) {
		e.probing = true
		return false, true, nil, 0
	}
	wait = e.nextProbe.Sub(now)
	if e.probing || wait <= 0 {
		// Someone else is probing: check back once it should have finished.
		wait = g.probeInterval
	}
	return false, false, e.cleared, wait
}

// trip blocks kubeContext after an auth failure. wasProbe marks the failure
// of the one attempt admit let through, which schedules the next probe.
// It returns true when this call is the one that blocked the context.
func (g *authGate) trip(kubeContext string, wasProbe bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, blocked := g.contexts[kubeContext]
	if !blocked {
		g.contexts[kubeContext] = &authGateEntry{
			cleared:   make(chan struct{}),
			nextProbe: time.Now().Add(g.probeInterval),
		}
		return true
	}
	if wasProbe {
		e.probing = false
		e.nextProbe = time.Now().Add(g.probeInterval)
	}
	return false
}

// clear unblocks kubeContext and wakes every tunnel waiting on it. It returns
// true when the context was blocked.
func (g *authGate) clear(kubeContext string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, blocked := g.contexts[kubeContext]
	if !blocked {
		return false
	}
	close(e.cleared)
	delete(g.contexts, kubeContext)
	return true
}

func (g *authGate) notifyUser(title, message string) {
	if g.notify != nil {
		go g.notify(title, message)
	}
}
