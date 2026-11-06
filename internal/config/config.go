package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	TLS         TLSConfig         `yaml:"tls"`
	Control     ControlConfig     `yaml:"control"`
	Logging     LoggingConfig     `yaml:"logging"`
	AccessLog   AccessLogConfig   `yaml:"access_log"`
	Environment EnvironmentConfig `yaml:"environment"`
	Tunnels     []Tunnel          `yaml:"tunnels"`
}

// EnvironmentConfig is applied to every subprocess the daemon spawns
// (currently just kubectl). Useful when the daemon runs under launchd with a
// minimal PATH that doesn't include the gcloud SDK or auth plugins.
type EnvironmentConfig struct {
	PathAdditions []string          `yaml:"path_additions"`
	Extra         map[string]string `yaml:"extra"`
}

type TLSConfig struct {
	CertDir    string `yaml:"cert_dir"`
	ListenAddr string `yaml:"listen_addr"`
}

type ControlConfig struct {
	Socket string `yaml:"socket"`
}

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files"`
	MaxAgeDays int    `yaml:"max_age_days"`
	Compress   bool   `yaml:"compress"`
}

type AccessLogConfig struct {
	Enabled bool `yaml:"enabled"`
}

type Tunnel struct {
	Name        string `yaml:"name"`
	Hostname    string `yaml:"hostname"`
	KubeContext string `yaml:"kube_context"`
	Namespace   string `yaml:"namespace"`
	Target      string `yaml:"target"`
	RemotePort  int    `yaml:"remote_port"`
	LocalPort   int    `yaml:"local_port"`
	// StripPrefix is removed from the request path before forwarding. Use this
	// when an upstream proxy (e.g. Istio VirtualService) rewrites the URI
	// before requests reach the pod — kubetunnel replays the same rewrite so
	// clients can keep using the canonical public URL.
	StripPrefix string `yaml:"strip_prefix,omitempty"`
	// PathPrefix restricts this tunnel to requests whose path starts with the
	// prefix (matched on segment boundaries, so "/api" matches "/api" and
	// "/api/x" but not "/apix"). Several tunnels may share a hostname as long
	// as their prefixes differ; the longest matching prefix wins and a tunnel
	// without prefix acts as the catch-all for that hostname. Use it to
	// replay an ingress that fans one host out to many services by path.
	PathPrefix  string            `yaml:"path_prefix,omitempty"`
	HealthCheck *HealthCheck      `yaml:"health_check,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
}

type HealthCheck struct {
	Path          string        `yaml:"path"`
	Interval      time.Duration `yaml:"interval"`
	Timeout       time.Duration `yaml:"timeout"`
	FailThreshold int           `yaml:"fail_threshold"`
}

func Load(path string) (*Config, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, fmt.Errorf("expand config path: %w", err)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() error {
	if c.TLS.CertDir == "" {
		c.TLS.CertDir = "~/.local/share/kubetunnel/certs"
	}
	expanded, err := expandPath(c.TLS.CertDir)
	if err != nil {
		return err
	}
	c.TLS.CertDir = expanded

	if c.TLS.ListenAddr == "" {
		c.TLS.ListenAddr = "127.0.0.1:443"
	}
	if c.Control.Socket == "" {
		c.Control.Socket = "/var/run/kubetunnel.sock"
	}
	if c.Logging.Dir == "" {
		c.Logging.Dir = "~/Library/Logs/kubetunnel"
	}
	expandedLog, err := expandPath(c.Logging.Dir)
	if err != nil {
		return err
	}
	c.Logging.Dir = expandedLog

	if c.Logging.MaxSizeMB == 0 {
		c.Logging.MaxSizeMB = 10
	}
	if c.Logging.MaxFiles == 0 {
		c.Logging.MaxFiles = 5
	}
	if c.Logging.MaxAgeDays == 0 {
		c.Logging.MaxAgeDays = 30
	}

	for i := range c.Tunnels {
		t := &c.Tunnels[i]
		if t.RemotePort == 0 {
			t.RemotePort = 80
		}
		if t.PathPrefix != "" && t.PathPrefix != "/" {
			t.PathPrefix = strings.TrimRight(t.PathPrefix, "/")
		}
		if t.HealthCheck != nil {
			if t.HealthCheck.Interval == 0 {
				t.HealthCheck.Interval = 30 * time.Second
			}
			if t.HealthCheck.Timeout == 0 {
				t.HealthCheck.Timeout = 5 * time.Second
			}
			if t.HealthCheck.FailThreshold == 0 {
				t.HealthCheck.FailThreshold = 3
			}
		}
	}
	return nil
}

func (c *Config) Validate() error {
	if len(c.Tunnels) == 0 {
		return fmt.Errorf("no tunnels defined")
	}
	names := map[string]bool{}
	routes := map[string]string{} // hostname + path_prefix -> tunnel name
	localPorts := map[int]string{}
	for i, t := range c.Tunnels {
		prefix := fmt.Sprintf("tunnels[%d] (%q)", i, t.Name)
		if t.Name == "" {
			return fmt.Errorf("tunnels[%d]: name required", i)
		}
		if names[t.Name] {
			return fmt.Errorf("%s: duplicate tunnel name", prefix)
		}
		names[t.Name] = true

		if t.Hostname == "" {
			return fmt.Errorf("%s: hostname required", prefix)
		}
		if t.PathPrefix != "" && !strings.HasPrefix(t.PathPrefix, "/") {
			return fmt.Errorf("%s: path_prefix must start with \"/\"", prefix)
		}
		routeKey := strings.ToLower(t.Hostname) + "\x00" + t.PathPrefix
		if existing, ok := routes[routeKey]; ok {
			if t.PathPrefix == "" {
				return fmt.Errorf("%s: duplicate hostname %q (already used by %q; add a path_prefix to share a hostname)", prefix, t.Hostname, existing)
			}
			return fmt.Errorf("%s: duplicate path_prefix %q for hostname %q (already used by %q)", prefix, t.PathPrefix, t.Hostname, existing)
		}
		routes[routeKey] = t.Name

		if t.KubeContext == "" {
			return fmt.Errorf("%s: kube_context required", prefix)
		}
		if t.Namespace == "" {
			return fmt.Errorf("%s: namespace required", prefix)
		}
		if t.Target == "" {
			return fmt.Errorf("%s: target required (e.g. svc/my-service)", prefix)
		}
		if !strings.Contains(t.Target, "/") {
			return fmt.Errorf("%s: target must be of form TYPE/NAME (e.g. svc/my-service)", prefix)
		}
		if t.LocalPort <= 0 || t.LocalPort > 65535 {
			return fmt.Errorf("%s: local_port must be in 1..65535", prefix)
		}
		if existing, ok := localPorts[t.LocalPort]; ok {
			return fmt.Errorf("%s: local_port %d already used by %q", prefix, t.LocalPort, existing)
		}
		localPorts[t.LocalPort] = t.Name
	}
	return nil
}

// HostnameList returns the distinct hostnames, in config order, for
// /etc/hosts and certificate management. Tunnels that share a hostname
// (path-based routing) contribute it once.
func (c *Config) HostnameList() []string {
	out := make([]string, 0, len(c.Tunnels))
	seen := map[string]bool{}
	for _, t := range c.Tunnels {
		if seen[t.Hostname] {
			continue
		}
		seen[t.Hostname] = true
		out = append(out, t.Hostname)
	}
	return out
}

// Route is the client-facing address a tunnel answers on: the hostname plus
// its path_prefix, e.g. "eshop.dev.example.com/contentful".
func (t Tunnel) Route() string {
	return t.Hostname + t.PathPrefix
}

// FindTunnel returns the tunnel with the given name, or nil.
func (c *Config) FindTunnel(name string) *Tunnel {
	for i := range c.Tunnels {
		if c.Tunnels[i].Name == name {
			return &c.Tunnels[i]
		}
	}
	return nil
}

func expandPath(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Clean(p), nil
}
