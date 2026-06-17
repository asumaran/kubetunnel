package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/asumaran/kubetunnel/internal/certs"
	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/control"
	"github.com/asumaran/kubetunnel/internal/hostsfile"
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/tui"
	"github.com/spf13/cobra"
)

const defaultSocket = "/var/run/kubetunnel.sock"

var (
	configPath string
	socketPath string
)

func main() {
	root := &cobra.Command{
		Use:   "tunnelctl",
		Short: "Control kubetunneld — manage kubectl port-forward tunnels with a local HTTPS reverse proxy.",
	}
	root.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath(), "config file")
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "daemon control socket")

	root.AddCommand(statusCmd())
	root.AddCommand(logsCmd())
	root.AddCommand(dashboardCmd())
	root.AddCommand(restartCmd())
	root.AddCommand(reloadCmd())
	root.AddCommand(shutdownCmd())
	root.AddCommand(certInstallCmd())
	root.AddCommand(installCmd())
	root.AddCommand(uninstallCmd())
	root.AddCommand(debugCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kubetunnel", "config.yaml")
}

func client() *control.Client { return control.NewClient(socketPath) }

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show tunnel status",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client().Status()
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tUPTIME\tRESTARTS\tHEALTH\tHOSTNAME\tLOCAL\tTARGET")
			for _, t := range resp.Tunnels {
				health := "OK"
				if !t.HealthOK {
					health = "FAIL"
				}
				uptime := t.Uptime
				if uptime == "" {
					uptime = "—"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t:%d\t%s\n",
					t.Name, t.State, uptime, t.Restarts, health, t.Hostname, t.LocalPort, t.InternalTarget())
			}
			return w.Flush()
		},
	}
}

func logsCmd() *cobra.Command {
	var (
		name   string
		filter string
		tail   int
		follow bool
		jsonOut bool
	)
	c := &cobra.Command{
		Use:   "logs",
		Short: "Show tunnel logs",
		Long:  "Show logs. Supports DSL filters (e.g. 'level:error AND tunnel:api').",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli := client()
			if !follow {
				resp, err := cli.Logs(name, filter, tail)
				if err != nil {
					return err
				}
				for _, e := range resp.Entries {
					printEntry(e, jsonOut)
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := cli.StreamLogs(ctx, name, filter)
			if err != nil {
				return err
			}
			for e := range ch {
				printEntry(e, jsonOut)
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "tunnel name (default: all)")
	c.Flags().StringVar(&filter, "filter", "", "DSL filter")
	c.Flags().IntVar(&tail, "tail", 200, "number of entries")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow new entries")
	c.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	return c
}

func printEntry(e logging.Entry, asJSON bool) {
	if asJSON {
		data, _ := json.Marshal(e)
		fmt.Println(string(data))
		return
	}
	ts := e.Time.Local().Format("15:04:05")
	level := strings.ToUpper(e.Level)
	if level == "" {
		level = "    "
	}
	tname := e.Tunnel
	if tname == "" {
		tname = "-"
	}
	fmt.Printf("%s %-5s [%s] %s %s\n", ts, level, tname, e.Event, e.Msg)
}

func dashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Launch the TUI dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(socketPath)
		},
	}
}

func restartCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart a tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			return client().Restart(name)
		},
	}
	c.Flags().StringVar(&name, "name", "", "tunnel name")
	return c
}

func reloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Hot-reload config",
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Reload()
		},
	}
}

func shutdownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Shutdown the daemon (launchd will relaunch it)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Shutdown()
		},
	}
}

func certInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cert-install",
		Short: "Install the local CA (runs `mkcert -install`) and generate certs for configured hostnames",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := certs.Install(); err != nil {
				return fmt.Errorf("mkcert -install: %w", err)
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			for _, t := range cfg.Tunnels {
				certPath, keyPath, err := certs.EnsureCert(cfg.TLS.CertDir, t.Hostname)
				if err != nil {
					fmt.Printf("! %s: %v\n", t.Hostname, err)
					continue
				}
				fmt.Printf("✓ %s\n  cert: %s\n  key:  %s\n", t.Hostname, certPath, keyPath)
			}
			return nil
		},
	}
}

func installCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install /etc/hosts entries and the LaunchDaemon plist (requires sudo)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("must run as root: sudo tunnelctl install")
			}
			if err := hostsfile.Install("", cfg.HostnameList()); err != nil {
				return fmt.Errorf("install /etc/hosts: %w", err)
			}
			fmt.Println("✓ /etc/hosts updated")
			if err := installPlist(configPath); err != nil {
				return fmt.Errorf("install plist: %w", err)
			}
			fmt.Println("✓ LaunchDaemon installed — kubetunneld is running")
			return nil
		},
	}
}

func uninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove /etc/hosts entries and the LaunchDaemon plist (requires sudo)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Geteuid() != 0 {
				return fmt.Errorf("must run as root: sudo tunnelctl uninstall")
			}
			if err := uninstallPlist(); err != nil {
				fmt.Printf("! plist: %v\n", err)
			} else {
				fmt.Println("✓ LaunchDaemon removed")
			}
			if err := hostsfile.Uninstall(""); err != nil {
				return fmt.Errorf("remove /etc/hosts entries: %w", err)
			}
			fmt.Println("✓ /etc/hosts cleaned")
			return nil
		},
	}
}

func debugCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "debug",
		Short: "Dump diagnostic info for a tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			fmt.Printf("# Debug dump for %s (%s)\n\n", name, time.Now().Format(time.RFC3339))
			// Minimal implementation: pull status + recent entries via client.
			st, err := client().Status()
			if err != nil {
				return err
			}
			for _, t := range st.Tunnels {
				if t.Name != name {
					continue
				}
				data, _ := json.MarshalIndent(t, "", "  ")
				fmt.Println(string(data))
			}
			logs, err := client().Logs(name, "", 500)
			if err != nil {
				return err
			}
			fmt.Println("\n## Recent log entries")
			for _, e := range logs.Entries {
				printEntry(e, false)
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "tunnel name")
	return c
}
