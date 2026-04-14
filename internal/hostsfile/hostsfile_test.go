package hostsfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleHosts = `##
# Host Database
#
127.0.0.1	localhost
255.255.255.255	broadcasthost
::1             localhost
`

func newTempHosts(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallAppendsBlock(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	if err := Install(p, []string{"foo.test", "bar.test"}); err != nil {
		t.Fatal(err)
	}
	out := read(t, p)
	if !strings.Contains(out, "127.0.0.1\tfoo.test") {
		t.Errorf("missing foo.test in:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1\tbar.test") {
		t.Errorf("missing bar.test in:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1\tlocalhost") {
		t.Errorf("pre-existing localhost clobbered")
	}
	backup := p + ".kubetunnel.bak"
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("expected backup to exist: %v", err)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	if err := Install(p, []string{"foo.test"}); err != nil {
		t.Fatal(err)
	}
	first := read(t, p)
	if err := Install(p, []string{"foo.test"}); err != nil {
		t.Fatal(err)
	}
	second := read(t, p)
	if first != second {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestInstallReplacesBlock(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	if err := Install(p, []string{"foo.test"}); err != nil {
		t.Fatal(err)
	}
	if err := Install(p, []string{"bar.test", "baz.test"}); err != nil {
		t.Fatal(err)
	}
	out := read(t, p)
	if strings.Contains(out, "foo.test") {
		t.Error("foo.test should be gone")
	}
	if !strings.Contains(out, "bar.test") || !strings.Contains(out, "baz.test") {
		t.Errorf("missing new entries:\n%s", out)
	}
	// Only one begin marker.
	if strings.Count(out, BeginMarker) != 1 {
		t.Errorf("expected 1 BEGIN marker, got %d", strings.Count(out, BeginMarker))
	}
}

func TestUninstallRemovesBlockOnly(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	if err := Install(p, []string{"foo.test"}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(p); err != nil {
		t.Fatal(err)
	}
	out := read(t, p)
	if strings.Contains(out, BeginMarker) || strings.Contains(out, "foo.test") {
		t.Errorf("block not removed:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1\tlocalhost") {
		t.Errorf("original content lost:\n%s", out)
	}
}

func TestUninstallNoOp(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	if err := Uninstall(p); err != nil {
		t.Fatal(err)
	}
	if read(t, p) != sampleHosts {
		t.Error("file modified unexpectedly")
	}
}

func TestContains(t *testing.T) {
	p := newTempHosts(t, sampleHosts)
	ok, _ := Contains(p)
	if ok {
		t.Error("should not contain block initially")
	}
	_ = Install(p, []string{"foo.test"})
	ok, _ = Contains(p)
	if !ok {
		t.Error("should contain block after install")
	}
}
