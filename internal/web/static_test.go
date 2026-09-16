package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
)

// routes builds a server with no tenant manager — enough for the static and
// health routes, which need no session.
func staticRoutes(t *testing.T) http.Handler {
	t.Helper()
	s, err := New(nil, []byte("a-test-secret-at-least-32-chars-long"), false, googleZero(), nil, nil, push.Keys{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.Routes()
}

func googleZero() google.Config { return google.Config{} }

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

// If the manifest stops being served or loses a required field, browsers
// quietly stop offering to install the app. Nothing errors; it just becomes a
// web page again.
func TestManifestIsInstallable(t *testing.T) {
	resp := get(t, staticRoutes(t), "/static/manifest.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200", resp.StatusCode)
	}

	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}

	if m.Name == "" || m.ShortName == "" {
		t.Error("manifest needs both name and short_name")
	}
	if m.StartURL != "/" || m.Scope != "/" {
		t.Errorf("start_url=%q scope=%q, want both /", m.StartURL, m.Scope)
	}
	// standalone is what gives the app its own window instead of a browser tab.
	if m.Display != "standalone" {
		t.Errorf("display = %q, want standalone", m.Display)
	}
	if m.ThemeColor == "" || m.BackgroundColor == "" {
		t.Error("manifest needs theme_color and background_color or the window flashes white on launch")
	}

	var has192, has512, hasMaskable bool
	for _, i := range m.Icons {
		switch {
		case i.Sizes == "192x192":
			has192 = true
		case i.Sizes == "512x512" && i.Purpose == "maskable":
			hasMaskable = true
		case i.Sizes == "512x512":
			has512 = true
		}
	}
	if !has192 || !has512 {
		t.Error("installability needs both a 192px and a 512px icon")
	}
	if !hasMaskable {
		t.Error("without a maskable icon, launchers crop the glyph")
	}
}

// Every icon the manifest promises must actually be served, or the install
// prompt never appears and the failure is invisible.
func TestManifestIconsExist(t *testing.T) {
	h := staticRoutes(t)
	for _, path := range []string{
		"/static/icon-192.png",
		"/static/icon-512.png",
		"/static/icon-maskable-512.png",
		"/static/apple-touch-icon.png",
		"/static/favicon-32.png",
	} {
		resp := get(t, h, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
			t.Errorf("%s content-type = %q, want image/png", path, ct)
		}
	}
}

// A service worker under /static/ can only control /static/. Serving it from
// the root is what lets it cover the whole app.
func TestServiceWorkerIsServedAtRootScope(t *testing.T) {
	resp := get(t, staticRoutes(t), "/sw.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/sw.js = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type = %q, want javascript", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("cache-control = %q — a cached service worker strands installed apps", cc)
	}
}

// The manifest must never be cached hard either: a stale one pins installed
// apps to an old start URL.
func TestManifestIsNotCachedHard(t *testing.T) {
	resp := get(t, staticRoutes(t), "/static/manifest.json")
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("manifest cache-control = %q, want no-cache", cc)
	}
}

func TestIconsAreCached(t *testing.T) {
	resp := get(t, staticRoutes(t), "/static/icon-192.png")
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("icon cache-control = %q, want a max-age", cc)
	}
}

func TestFaviconRedirects(t *testing.T) {
	resp := get(t, staticRoutes(t), "/favicon.ico")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("/favicon.ico = %d, want 301", resp.StatusCode)
	}
}

// The memory used to be a top-level page. It now lives in Settings, but old
// links and bookmarks must not 404.
func TestLearnedRedirectsToSettingsMemory(t *testing.T) {
	resp := get(t, staticRoutes(t), "/learned")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("/learned = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/settings#memory" {
		t.Errorf("Location = %q, want /settings#memory", loc)
	}
}

// Settings holds a tenant's memory and their calendar credentials, so it must
// never be reachable without a session.
func TestSettingsRequiresASession(t *testing.T) {
	for _, path := range []string{"/settings", "/"} {
		resp := get(t, staticRoutes(t), path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s unauthenticated = %d, want a redirect", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("%s redirected to %q, want /login", path, loc)
		}
	}
}

// A cookie's MaxAge only tells a browser when to stop sending it. If the
// signed value itself never expires, a captured session is valid forever.
func TestSessionValueExpires(t *testing.T) {
	s := testServer()

	fresh := s.sign("dave-glass")
	if id, ok := s.verify(fresh); !ok || id != "dave-glass" {
		t.Fatalf("a fresh session did not verify: %q %v", id, ok)
	}

	// Forge a correctly-signed session issued long ago, the way a captured
	// cookie would look after sitting in a log for a few months.
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	payload := "dave-glass." + strconv.FormatInt(old, 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	stale := payload + "." + hex.EncodeToString(mac.Sum(nil))

	if _, ok := s.verify(stale); ok {
		t.Error("a session issued 40 days ago still verified")
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	s := testServer()
	valid := s.sign("dave-glass")

	for _, bad := range []string{
		"",
		"dave-glass",
		"dave-glass.123",
		"kim-glazing." + valid[len("dave-glass."):], // swapped tenant, same signature
		valid + "x",
	} {
		if _, ok := s.verify(bad); ok {
			t.Errorf("malformed session %q verified", bad)
		}
	}
}
