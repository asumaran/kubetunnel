package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// stubLaunchctl replaces the launchctl and launchctlDelay injection points
// for the duration of one test. Each call to fn receives the launchctl args
// and the 0-based call index; delays are recorded instead of sleeping.
func stubLaunchctl(t *testing.T, fn func(call int, args []string) ([]byte, error)) (calls *[][]string, delays *[]time.Duration) {
	t.Helper()
	origExec, origDelay := launchctl, launchctlDelay
	t.Cleanup(func() { launchctl, launchctlDelay = origExec, origDelay })

	calls = &[][]string{}
	delays = &[]time.Duration{}
	launchctl = func(args ...string) ([]byte, error) {
		n := len(*calls)
		*calls = append(*calls, args)
		return fn(n, args)
	}
	launchctlDelay = func(d time.Duration) { *delays = append(*delays, d) }
	return calls, delays
}

func TestRetryLaunchctlFirstAttemptSucceeds(t *testing.T) {
	calls, delays := stubLaunchctl(t, func(int, []string) ([]byte, error) {
		return nil, nil
	})
	if err := retryLaunchctl(5, "bootstrap", "system", plistPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("expected 1 call, got %d", len(*calls))
	}
	if len(*delays) != 0 {
		t.Errorf("expected no delay before the first attempt, got %v", *delays)
	}
}

func TestRetryLaunchctlRecoversFromTransientFailure(t *testing.T) {
	// Reproduces the bootout/bootstrap race: launchd rejects the first two
	// bootstraps with EIO while teardown finishes, then accepts.
	calls, delays := stubLaunchctl(t, func(call int, _ []string) ([]byte, error) {
		if call < 2 {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return nil, nil
	})
	if err := retryLaunchctl(5, "bootstrap", "system", plistPath); err != nil {
		t.Fatalf("expected recovery after retries, got: %v", err)
	}
	if len(*calls) != 3 {
		t.Errorf("expected 3 attempts, got %d", len(*calls))
	}
	if len(*delays) != 2 {
		t.Errorf("expected a delay before each retry (2), got %d", len(*delays))
	}
}

func TestRetryLaunchctlExhaustsAttempts(t *testing.T) {
	calls, _ := stubLaunchctl(t, func(int, []string) ([]byte, error) {
		return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
	})
	err := retryLaunchctl(3, "bootstrap", "system", plistPath)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if len(*calls) != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", len(*calls))
	}
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("error should include launchctl output, got: %v", err)
	}
}

func TestWaitForBootoutReturnsOnceServiceIsGone(t *testing.T) {
	// launchctl print succeeds while the service still exists, then fails
	// once launchd has finished the asynchronous teardown.
	calls, _ := stubLaunchctl(t, func(call int, _ []string) ([]byte, error) {
		if call < 3 {
			return nil, nil // still loaded
		}
		return nil, errors.New("Could not find service") // gone
	})
	waitForBootout()
	if len(*calls) != 4 {
		t.Errorf("expected polling to stop right after the service disappears (4 calls), got %d", len(*calls))
	}
	for _, c := range *calls {
		if c[0] != "print" {
			t.Errorf("waitForBootout should only call launchctl print, got %v", c)
		}
	}
}

func TestWaitForBootoutGivesUpOnStuckService(t *testing.T) {
	calls, _ := stubLaunchctl(t, func(int, []string) ([]byte, error) {
		return nil, nil // service never goes away
	})
	waitForBootout()
	if len(*calls) != 50 {
		t.Errorf("expected to give up after 50 polls, got %d", len(*calls))
	}
}
