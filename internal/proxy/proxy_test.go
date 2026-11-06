package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/logging"
)

type readyAll struct{}

func (readyAll) IsReady(string) bool { return true }

type readyNever struct{}

func (readyNever) IsReady(string) bool { return false }

type trackedReady struct {
	mu    sync.Mutex
	ready map[string]bool
}

func (t *trackedReady) IsReady(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready[name]
}

func newTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(logging.Options{Dir: t.TempDir(), BufferCap: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestProxyRoutesByHost(t *testing.T) {
	upstreamFoo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "foo")
		w.Write([]byte("hello foo host=" + r.Host))
	}))
	defer upstreamFoo.Close()
	upstreamBar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "bar")
		w.Write([]byte("hello bar"))
	}))
	defer upstreamBar.Close()

	fooPort := portOf(t, upstreamFoo.URL)
	barPort := portOf(t, upstreamBar.URL)

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	err := srv.Update([]config.Tunnel{
		{Name: "foo", Hostname: "foo.test", LocalPort: fooPort},
		{Name: "bar", Hostname: "bar.test", LocalPort: barPort},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test via ServeHTTP directly.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "foo.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "hello foo") {
		t.Errorf("expected foo body, got %q", body)
	}
	if !strings.Contains(body, "host=foo.test") {
		t.Errorf("Host header not preserved, got %q", body)
	}

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Host = "bar.test"
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	if !strings.Contains(rr2.Body.String(), "hello bar") {
		t.Errorf("bar not routed")
	}
}

func TestProxyUnknownHostReturns404(t *testing.T) {
	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	_ = srv.Update(nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "nope.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestProxyReturns503WhenNotReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyNever{}, nil)
	_ = srv.Update([]config.Tunnel{{Name: "x", Hostname: "x.test", LocalPort: portOf(t, upstream.URL)}})

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "x.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestProxyStripPrefix(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	_ = srv.Update([]config.Tunnel{{
		Name:        "api",
		Hostname:    "api.test",
		LocalPort:   portOf(t, upstream.URL),
		StripPrefix: "/api/v1",
	}})

	req := httptest.NewRequest("GET", "/api/v1/users/abc", nil)
	req.Host = "api.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("want 200 got %d", rr.Code)
	}
	if gotPath != "/users/abc" {
		t.Errorf("expected stripped path, got %q", gotPath)
	}
}

func TestProxyStripPrefixNoMatchLeavesPath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	_ = srv.Update([]config.Tunnel{{
		Name: "api", Hostname: "api.test", LocalPort: portOf(t, upstream.URL),
		StripPrefix: "/api/v1",
	}})

	req := httptest.NewRequest("GET", "/other/path", nil)
	req.Host = "api.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if gotPath != "/other/path" {
		t.Errorf("expected untouched path, got %q", gotPath)
	}
}

func TestProxyForwardedHeaders(t *testing.T) {
	var gotProto, gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotHost = r.Header.Get("X-Forwarded-Host")
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	_ = srv.Update([]config.Tunnel{{Name: "x", Hostname: "x.test", LocalPort: portOf(t, upstream.URL)}})

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "x.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		b, _ := io.ReadAll(rr.Body)
		t.Fatalf("want 200 got %d: %s", rr.Code, b)
	}
	if gotProto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotHost != "x.test" {
		t.Errorf("X-Forwarded-Host = %q, want x.test", gotHost)
	}
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	p := 0
	for _, c := range u.Port() {
		p = p*10 + int(c-'0')
	}
	return p
}

func TestProxyRoutesByPathPrefix(t *testing.T) {
	newUpstream := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(label + " " + r.URL.Path))
		}))
	}
	contentful := newUpstream("contentful")
	defer contentful.Close()
	stores := newUpstream("stores")
	defer stores.Close()
	storesAdmin := newUpstream("stores-admin")
	defer storesAdmin.Close()
	fallback := newUpstream("fallback")
	defer fallback.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	err := srv.Update([]config.Tunnel{
		// Catch-all declared first on purpose: ordering in config must not matter.
		{Name: "fallback", Hostname: "eshop.test", LocalPort: portOf(t, fallback.URL)},
		{Name: "contentful", Hostname: "eshop.test", PathPrefix: "/contentful", StripPrefix: "/contentful", LocalPort: portOf(t, contentful.URL)},
		{Name: "stores", Hostname: "eshop.test", PathPrefix: "/stores", LocalPort: portOf(t, stores.URL)},
		{Name: "stores-admin", Hostname: "eshop.test", PathPrefix: "/stores/admin", LocalPort: portOf(t, storesAdmin.URL)},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want string
	}{
		{"/contentful/api/TELECABLE/spaces/x", "contentful /api/TELECABLE/spaces/x"},
		{"/contentful", "contentful /"},
		{"/stores/api/stores", "stores /stores/api/stores"},
		{"/stores/admin/x", "stores-admin /stores/admin/x"},
		{"/storesx/y", "fallback /storesx/y"}, // segment boundary: not the /stores tunnel
		{"/", "fallback /"},
		{"/catalog/api", "fallback /catalog/api"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		req.Host = "eshop.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Errorf("%s: expected 200, got %d", c.path, rr.Code)
			continue
		}
		if got := rr.Body.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestProxyNoCatchAllReturns404ForUnmatchedPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("contentful"))
	}))
	defer upstream.Close()

	srv := New("127.0.0.1:0", newTestLogger(t), readyAll{}, nil)
	if err := srv.Update([]config.Tunnel{
		{Name: "contentful", Hostname: "eshop.test", PathPrefix: "/contentful", LocalPort: portOf(t, upstream.URL)},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/stores/api/stores", nil)
	req.Host = "eshop.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no tunnel for eshop.test/stores/api/stores") {
		t.Errorf("unexpected body: %q", rr.Body.String())
	}

	req = httptest.NewRequest("GET", "/contentful/x", nil)
	req.Host = "eshop.test"
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200 for matching prefix, got %d", rr.Code)
	}
}

func TestProxyPathPrefixReadinessIsPerTunnel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	ready := &trackedReady{ready: map[string]bool{"stores": true}}
	srv := New("127.0.0.1:0", newTestLogger(t), ready, nil)
	if err := srv.Update([]config.Tunnel{
		{Name: "contentful", Hostname: "eshop.test", PathPrefix: "/contentful", LocalPort: portOf(t, upstream.URL)},
		{Name: "stores", Hostname: "eshop.test", PathPrefix: "/stores", LocalPort: portOf(t, upstream.URL)},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/contentful/x", nil)
	req.Host = "eshop.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Errorf("expected 503 for not-ready contentful tunnel, got %d", rr.Code)
	}

	req = httptest.NewRequest("GET", "/stores/x", nil)
	req.Host = "eshop.test"
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("expected 200 for ready stores tunnel, got %d", rr.Code)
	}
}
