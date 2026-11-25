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

// Send shows a macOS notification with a sound. It returns an error when no
// user is logged in at the console or the notification could not be posted.
func Send(title, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, err := command(ctx, os.Geteuid(), consoleUID, findTerminalNotifier(), title, message)
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
func command(ctx context.Context, euid int, consoleUID func() (int, error), notifier, title, message string) (*exec.Cmd, error) {
	var argv []string
	if notifier != "" {
		argv = []string{notifier, "-title", title, "-message", message,
			"-sound", "Basso", "-group", "kubetunnel"}
	} else {
		script := fmt.Sprintf(`display notification %s with title %s sound name "Basso"`,
			appleScriptString(message), appleScriptString(title))
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
