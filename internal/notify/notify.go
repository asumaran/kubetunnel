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
	cmd, err := command(ctx, os.Geteuid(), consoleUID, title, message)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("post notification: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// command builds the osascript invocation. As root it runs inside the console
// user's launchd session and as that user, which is what makes the
// notification reach their Notification Center.
func command(ctx context.Context, euid int, consoleUID func() (int, error), title, message string) (*exec.Cmd, error) {
	script := fmt.Sprintf(`display notification %s with title %s sound name "Basso"`,
		appleScriptString(message), appleScriptString(title))
	if euid != 0 {
		return exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script), nil
	}
	uid, err := consoleUID()
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		return nil, fmt.Errorf("no user logged in at the console")
	}
	id := strconv.Itoa(uid)
	return exec.CommandContext(ctx, "/bin/launchctl", "asuser", id,
		"/usr/bin/sudo", "-u", "#"+id, "/usr/bin/osascript", "-e", script), nil
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
