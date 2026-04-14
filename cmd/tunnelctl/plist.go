package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const plistLabel = "dev.kubetunnel"
const plistPath = "/Library/LaunchDaemons/dev.kubetunnel.plist"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.kubetunnel</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
    <key>Crashed</key>
    <true/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>%s/daemon.out.log</string>
  <key>StandardErrorPath</key>
  <string>%s/daemon.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>KUBECONFIG</key>
    <string>%s</string>
    <key>PATH</key>
    <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
    <key>HOME</key>
    <string>%s</string>
  </dict>
</dict>
</plist>
`

func installPlist(cfgPath string) error {
	daemonBin, err := exec.LookPath("kubetunneld")
	if err != nil {
		return fmt.Errorf("kubetunneld not found on PATH — run install.sh first")
	}
	absCfg, _ := filepath.Abs(cfgPath)
	// User's home for KUBECONFIG/HOME — read from SUDO_USER if present.
	home := os.Getenv("HOME")
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := os.UserHomeDir(); err == nil && u == "/var/root" {
			home = "/Users/" + sudoUser
		}
	}
	kubeconfig := filepath.Join(home, ".kube", "config")
	logDir := filepath.Join(home, "Library", "Logs", "kubetunnel")
	_ = os.MkdirAll(logDir, 0o755)

	body := fmt.Sprintf(plistTemplate, daemonBin, absCfg, logDir, logDir, kubeconfig, home)
	if err := os.WriteFile(plistPath, []byte(body), 0o644); err != nil {
		return err
	}
	// bootstrap + enable + kickstart.
	cmds := [][]string{
		{"launchctl", "bootout", "system/" + plistLabel}, // ignore errors
		{"launchctl", "bootstrap", "system", plistPath},
		{"launchctl", "kickstart", "-k", "system/" + plistLabel},
	}
	for i, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil && i != 0 {
			return fmt.Errorf("launchctl %v: %w — %s", c, err, out)
		}
	}
	return nil
}

func uninstallPlist() error {
	cmd := exec.Command("launchctl", "bootout", "system/"+plistLabel)
	_, _ = cmd.CombinedOutput()
	return os.Remove(plistPath)
}
