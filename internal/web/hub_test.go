package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

func hubServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	t.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	m, err := tenant.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	for _, id := range []string{"dave-glass", "kim-glazing"} {
		if _, _, err := m.Create(id, id, "glazier", "UTC", ""); err != nil {
			t.Fatal(err)
		}
	}
	box, err := secrets.NewBox(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(m, []byte(strings.Repeat("x", 32)), false, googleZero(), box, nil, push.Keys{})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 64)
	sealed, err := box.Seal([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	stores, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatal(err)
	}
	id, err := stores.Ledger().AddHubConnection("Receptionist", sealed)
	if err != nil {
		t.Fatal(err)
	}
	return s, id, key
}

func signedHook(path, key string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	r.Header.Set("X-ExecAssist-Timestamp", ts)
	r.Header.Set("X-ExecAssist-Signature", hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestWebhookTenantBoundaryAndReplay(t *testing.T) {
	s, connection, key := hubServer(t)
	e := ledger.HubEvent{ID: "e1", Type: "measurement.completed", OccurredAt: time.Now().UTC(), JobID: "j1", Title: "Measure", CustomerID: "c1", CustomerName: "Ferguson", PropertyID: "p1", Address: "41 Cedar Lane", Evidence: "Source measurement receipt"}
	body, _ := json.Marshal(e)
	h := s.Routes()
	for _, test := range []struct {
		tenant string
		key    string
		want   int
	}{
		{"dave-glass", "wrong", http.StatusUnauthorized},
		{"kim-glazing", key, http.StatusUnauthorized},
		{"dave-glass", key, http.StatusOK},
		{"dave-glass", key, http.StatusOK},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedHook("/hooks/"+test.tenant+"/"+connection, test.key, body))
		if w.Code != test.want {
			t.Fatalf("tenant=%s got %d: %s", test.tenant, w.Code, w.Body)
		}
	}
	stores, _ := s.tenants.Stores("dave-glass")
	tasks, err := stores.Ledger().Commitments(false)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks: %+v %v", tasks, err)
	}
	other, _ := s.tenants.Stores("kim-glazing")
	tasks, err = other.Ledger().Commitments(false)
	if err != nil || len(tasks) != 0 {
		t.Fatal("webhook crossed tenant boundary")
	}
}

func TestHubFormsRequireSessionBoundApproval(t *testing.T) {
	s, connection, _ := hubServer(t)
	for _, token := range []string{"", s.signState("kim-glazing")} {
		form := url.Values{"csrf": {token}, "id": {connection}}
		r := httptest.NewRequest(http.MethodPost, "/hub/disconnect", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sign("dave-glass")})
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("unapproved mutation status=%d", w.Code)
		}
	}
	stores, _ := s.tenants.Stores("dave-glass")
	c, err := stores.Ledger().HubConnection(connection)
	if err != nil || !c.Enabled {
		t.Fatal("rejected request changed connection")
	}
}

func TestHubPagesRenderAndEscapeSourceText(t *testing.T) {
	s, connection, _ := hubServer(t)
	stores, _ := s.tenants.Stores("dave-glass")
	now := time.Now().UTC()
	_, err := stores.Ledger().IngestHubEvent(connection, ledger.HubEvent{ID: "e1", Type: "measurement.completed", OccurredAt: now, JobID: "j1", Title: "<script>alert(1)</script>", CustomerID: "c1", CustomerName: "Ferguson", PropertyID: "p1", Address: "41 Cedar", Evidence: "Measured"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hub", "/", "/recall?q=Ferguson"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sign("dave-glass")})
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<script>alert(1)</script>") || !strings.Contains(w.Body.String(), "Ferguson") {
			t.Fatalf("page %s failed: %d %s", path, w.Code, w.Body)
		}
	}
}
