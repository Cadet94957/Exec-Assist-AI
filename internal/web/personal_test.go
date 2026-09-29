package web

import (
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPersonalScreensAndTenantIsolation(t *testing.T) {
	s, _, _ := hubServer(t)
	store, _ := s.tenants.Stores("dave-glass")
	if err := store.Ledger().ReplaceMailSnapshot("gmail", []ledger.MailMessage{{ExternalID: "1", Subject: "<script>alert(1)</script>", Body: "Private invoice", ReceivedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/settings", "/inbox"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sign("dave-glass")})
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive page cached")
		}
		if strings.Contains(w.Body.String(), "<script>alert") {
			t.Fatal("remote HTML executed")
		}
		if path == "/settings" && !strings.Contains(w.Body.String(), "Apple Mail (iCloud)") {
			t.Fatal("missing connector controls")
		}
	}
	r := httptest.NewRequest("GET", "/inbox", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sign("kim-glazing")})
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Private invoice") {
		t.Fatal("mail crossed tenants")
	}
	for _, token := range []string{"", s.signState("kim-glazing")} {
		f := url.Values{"csrf": {token}}
		r := httptest.NewRequest("POST", "/connect/personal/gmail/disconnect", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sign("dave-glass")})
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("mutation without tenant CSRF token")
		}
	}
}
