package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCommandAsUserRunsOsascriptDirectly(t *testing.T) {
	cmd, err := command(context.Background(), 501, nil, "", "kubetunnel", `say "hi"`)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "/usr/bin/osascript" {
		t.Fatalf("unexpected command: %v", cmd.Args)
	}
	script := cmd.Args[len(cmd.Args)-1]
	if !strings.Contains(script, `"say \"hi\""`) || !strings.Contains(script, `with title "kubetunnel"`) {
		t.Errorf("script not quoted as expected: %s", script)
	}
}

func TestCommandAsRootTargetsConsoleUser(t *testing.T) {
	cmd, err := command(context.Background(), 0, func() (int, error) { return 501, nil }, "", "t", "m")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd.Args[:6], " ")
	want := "/bin/launchctl asuser 501 /usr/bin/sudo -u #501"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCommandAsRootWithoutConsoleUser(t *testing.T) {
	if _, err := command(context.Background(), 0, func() (int, error) { return 0, nil }, "", "t", "m"); err == nil {
		t.Error("expected an error when nobody is logged in")
	}
	if _, err := command(context.Background(), 0, func() (int, error) { return 0, fmt.Errorf("boom") }, "", "t", "m"); err == nil {
		t.Error("expected the console lookup error to surface")
	}
}

func TestCommandPrefersTerminalNotifier(t *testing.T) {
	const tn = "/opt/homebrew/bin/terminal-notifier"
	cmd, err := command(context.Background(), 0, func() (int, error) { return 501, nil }, tn, "the title", "the message")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd.Args, " ")
	want := "/bin/launchctl asuser 501 /usr/bin/sudo -u #501 " + tn +
		" -title the title -message the message -sound Basso -group kubetunnel"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "osascript") {
		t.Error("osascript must not be used when terminal-notifier is available")
	}
}
