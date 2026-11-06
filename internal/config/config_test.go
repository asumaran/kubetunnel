package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
tls:
  listen_addr: "127.0.0.1:443"
control:
  socket: /tmp/kubetunnel.sock
tunnels:
  - name: api
    hostname: api.example.com
    kube_context: gke_foo
    namespace: staging
    target: svc/api-gateway
    remote_port: 80
    local_port: 9001
    health_check:
      path: /health
  - name: web
    hostname: web.example.com
    kube_context: gke_foo
    namespace: staging
    target: svc/web-frontend
    remote_port: 80
    local_port: 9002
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeTempConfig(t, validYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tunnels) != 2 {
		t.Fatalf("expected 2 tunnels, got %d", len(cfg.Tunnels))
	}
	if cfg.Tunnels[0].HealthCheck.Interval != 30*time.Second {
		t.Errorf("expected default interval 30s, got %v", cfg.Tunnels[0].HealthCheck.Interval)
	}
	if cfg.Tunnels[0].HealthCheck.FailThreshold != 3 {
		t.Errorf("expected default fail_threshold 3, got %d", cfg.Tunnels[0].HealthCheck.FailThreshold)
	}
	hosts := cfg.HostnameList()
	if len(hosts) != 2 || hosts[0] != "api.example.com" {
		t.Errorf("unexpected hostnames: %v", hosts)
	}
	if cfg.FindTunnel("web") == nil {
		t.Error("FindTunnel failed")
	}
	if cfg.FindTunnel("missing") != nil {
		t.Error("FindTunnel returned non-nil for missing")
	}
}

func TestValidateDuplicateName(t *testing.T) {
	y := strings.ReplaceAll(validYAML, "name: web", "name: api")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "duplicate tunnel name") {
		t.Fatalf("expected duplicate name error, got: %v", err)
	}
}

func TestValidateDuplicateHostname(t *testing.T) {
	y := strings.ReplaceAll(validYAML, "web.example.com", "api.example.com")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "duplicate hostname") {
		t.Fatalf("expected duplicate hostname error, got: %v", err)
	}
}

func TestValidateDuplicateLocalPort(t *testing.T) {
	y := strings.ReplaceAll(validYAML, "local_port: 9002", "local_port: 9001")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "local_port 9001 already used") {
		t.Fatalf("expected duplicate port error, got: %v", err)
	}
}

func TestValidateMissingTarget(t *testing.T) {
	y := strings.ReplaceAll(validYAML, "target: svc/api-gateway", "target: \"\"")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "target required") {
		t.Fatalf("expected target required error, got: %v", err)
	}
}

func TestValidateTargetWithoutSlash(t *testing.T) {
	y := strings.ReplaceAll(validYAML, "svc/api-gateway", "api-gateway")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "TYPE/NAME") {
		t.Fatalf("expected target format error, got: %v", err)
	}
}

func TestValidateNoTunnels(t *testing.T) {
	p := writeTempConfig(t, "tunnels: []")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "no tunnels") {
		t.Fatalf("expected no tunnels error, got: %v", err)
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := expandPath("~/foo")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "foo")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

const sharedHostYAML = `
tunnels:
  - name: contentful
    hostname: eshop.example.com
    kube_context: gke_foo
    namespace: eshop
    target: svc/contentful-api
    local_port: 9101
    path_prefix: /contentful/
    strip_prefix: /contentful
  - name: stores
    hostname: eshop.example.com
    kube_context: gke_foo
    namespace: eshop
    target: svc/stores
    local_port: 9102
    path_prefix: /stores
    strip_prefix: /stores
  - name: fallback
    hostname: eshop.example.com
    kube_context: gke_foo
    namespace: eshop
    target: svc/gateway
    local_port: 9103
  - name: other
    hostname: other.example.com
    kube_context: gke_foo
    namespace: eshop
    target: svc/other
    local_port: 9104
`

func TestSharedHostnameWithPathPrefixes(t *testing.T) {
	p := writeTempConfig(t, sharedHostYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Tunnels[0].PathPrefix; got != "/contentful" {
		t.Errorf("expected trailing slash trimmed, got %q", got)
	}
	if got := cfg.Tunnels[0].Route(); got != "eshop.example.com/contentful" {
		t.Errorf("unexpected route %q", got)
	}
	if got := cfg.Tunnels[2].Route(); got != "eshop.example.com" {
		t.Errorf("unexpected catch-all route %q", got)
	}
	hosts := cfg.HostnameList()
	if len(hosts) != 2 || hosts[0] != "eshop.example.com" || hosts[1] != "other.example.com" {
		t.Errorf("expected distinct hostnames in config order, got %v", hosts)
	}
}

func TestValidateDuplicatePathPrefix(t *testing.T) {
	y := strings.ReplaceAll(sharedHostYAML, "path_prefix: /stores", "path_prefix: /contentful")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `duplicate path_prefix "/contentful"`) {
		t.Fatalf("expected duplicate path_prefix error, got: %v", err)
	}
}

func TestValidateDuplicateCatchAll(t *testing.T) {
	y := strings.ReplaceAll(sharedHostYAML, "    path_prefix: /stores\n", "")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "duplicate hostname") {
		t.Fatalf("expected duplicate hostname error, got: %v", err)
	}
}

func TestValidatePathPrefixMustStartWithSlash(t *testing.T) {
	y := strings.ReplaceAll(sharedHostYAML, "path_prefix: /stores", "path_prefix: stores")
	p := writeTempConfig(t, y)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "path_prefix must start with") {
		t.Fatalf("expected path_prefix format error, got: %v", err)
	}
}
