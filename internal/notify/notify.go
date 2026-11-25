// Package notify posts desktop notifications to the user logged in at the
// console. The daemon runs as root under launchd, outside any GUI session, so
// a notification has to be handed to the console user's session explicitly.
package notify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Notification is what Send shows.
type Notification struct {
	Title   string
	Message string
	// OnClick is a shell command run in the user's login shell when they click
	// the notification. It needs terminal-notifier; without it the click does
	// whatever the fallback's click does.
	OnClick string
}

// TerminalNotifier returns the path of terminal-notifier, or "" when it is
// not installed.
func TerminalNotifier() string { return findTerminalNotifier() }

// Send shows a macOS notification with a sound. It returns an error when no
// user is logged in at the console or the notification could not be posted.
func Send(n Notification) error {
	notifier := findTerminalNotifier()
	err := send(notifier, n)
	if err != nil && notifier != "" {
		// terminal-notifier is installed but unusable (typically its
		// notifications are not allowed yet in System Settings): a
		// notification with an awkward click beats no notification.
		if fallbackErr := send("", n); fallbackErr != nil {
			return fmt.Errorf("%w; osascript fallback: %v", err, fallbackErr)
		}
		return nil
	}
	return err
}

func send(notifier string, n Notification) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, err := command(ctx, os.Geteuid(), consoleUID, notifier, n)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("post notification: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// terminalNotifierPaths are checked after PATH: launchd gives the daemon a
// minimal PATH that may not include Homebrew.
var terminalNotifierPaths = []string{
	"/opt/homebrew/bin/terminal-notifier",
	"/usr/local/bin/terminal-notifier",
}

// findTerminalNotifier returns the path of terminal-notifier, or "" when it
// is not installed.
func findTerminalNotifier() string {
	if p, err := exec.LookPath("terminal-notifier"); err == nil {
		return p
	}
	for _, p := range terminalNotifierPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// command builds the invocation that posts the notification. As root it runs
// inside the console user's launchd session and as that user, which is what
// makes the notification reach their Notification Center.
//
// terminal-notifier is preferred when installed (notifier is its path): a
// notification posted through osascript belongs to Script Editor, so clicking
// it launches Script Editor and its open-file dialog. One from
// terminal-notifier is simply dismissed on click.
func command(ctx context.Context, euid int, consoleUID func() (int, error), notifier string, n Notification) (*exec.Cmd, error) {
	var argv []string
	if notifier != "" {
		argv = []string{notifier, "-title", n.Title, "-message", n.Message,
			"-sound", "Basso", "-group", "kubetunnel"}
		if n.OnClick != "" {
			// The click is handled from the GUI session, whose PATH is the
			// bare system one: a login shell gives the command the user's.
			argv = append(argv, "-execute", "/bin/zsh -lc "+shellQuote(n.OnClick))
		}
	} else {
		script := fmt.Sprintf(`display notification %s with title %s sound name "Basso"`,
			appleScriptString(n.Message), appleScriptString(n.Title))
		argv = []string{"/usr/bin/osascript", "-e", script}
	}
	if euid != 0 {
		return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
	}
	uid, err := consoleUID()
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		return nil, fmt.Errorf("no user logged in at the console")
	}
	id := strconv.Itoa(uid)
	args := append([]string{"asuser", id, "/usr/bin/sudo", "-u", "#" + id}, argv...)
	return exec.CommandContext(ctx, "/bin/launchctl", args...), nil
}

// consoleUID returns the uid of the user that owns the console, i.e. the one
// whose GUI session is active.
func consoleUID() (int, error) {
	info, err := os.Stat("/dev/console")
	if err != nil {
		return 0, fmt.Errorf("stat /dev/console: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot read the owner of /dev/console")
	}
	return int(st.Uid), nil
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	return `"` + s + `"`
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
