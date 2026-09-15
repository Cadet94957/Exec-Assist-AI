package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/brief"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
	"github.com/Cadet94957/Exec-Assist-AI/internal/web"
)

func TestMain(m *testing.M) {
	os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	os.Exit(m.Run())
}

func tenantStore(t *testing.T) tenant.TenantStore {
	t.Helper()
	m, err := tenant.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "America/Los_Angeles", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	s, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	return s
}

func vapid(t *testing.T) push.Keys {
	t.Helper()
	k, err := push.GenerateKeys("mailto:ops@example.com")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	return k
}

func subscribe(t *testing.T, s tenant.TenantStore, endpoint string) {
	t.Helper()
	if err := s.Ledger().SavePushSubscription(ledger.PushSubscription{
		Endpoint: endpoint,
		P256dh:   "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
		Auth:     "tBHItJI5svbpez7KI4CCXg",
	}); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
}

// Delivery has to be recorded, not assumed. A brief written and never received
// looks identical to a working product until churn says otherwise.
func TestDeliverSendsAndRecords(t *testing.T) {
	s := tenantStore(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	subscribe(t, s, srv.URL)

	day := s.Tenant().StartOfDay(time.Now())
	if err := s.Ledger().SaveBrief(day, "3 stops today.", "8:00 Harborview..."); err != nil {
		t.Fatalf("save brief: %v", err)
	}

	n, err := web.Deliver(context.Background(), push.NewSender(vapid(t)), s,
		brief.Brief{Headline: "3 stops today."}, day)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 1 || hits != 1 {
		t.Fatalf("delivered %d, push service saw %d requests; want 1 and 1", n, hits)
	}

	stored, ok, err := s.Ledger().GetBrief(day)
	if err != nil || !ok {
		t.Fatalf("get brief: %v (found %v)", err, ok)
	}
	if !stored.DeliveredAt.Valid {
		t.Error("the brief was sent but not recorded as delivered")
	}
}

// A browser that forgot us must be forgotten back, or the loop retries a dead
// endpoint every morning forever.
func TestDeliverForgetsDeadSubscriptions(t *testing.T) {
	s := tenantStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()
	subscribe(t, s, srv.URL)

	day := s.Tenant().StartOfDay(time.Now())
	n, err := web.Deliver(context.Background(), push.NewSender(vapid(t)), s,
		brief.Brief{Headline: "x"}, day)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0", n)
	}

	subs, err := s.Ledger().PushSubscriptions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("%d dead subscriptions survived", len(subs))
	}
}

// The lock-screen payload has to carry the headline and a place to land.
func TestDeliveredPayloadIsUsable(t *testing.T) {
	s := tenantStore(t)

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	subscribe(t, s, srv.URL)

	day := s.Tenant().StartOfDay(time.Now())
	if _, err := web.Deliver(context.Background(), push.NewSender(vapid(t)), s,
		brief.Brief{Headline: "3 stops — Marriott is tight."}, day); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// The body is encrypted end-to-end, so the assertion is that something
	// substantial was sent rather than an empty ping.
	if len(body) < 50 {
		t.Errorf("payload is %d bytes, which is too small to carry a headline", len(body))
	}
}

// With no devices subscribed, delivery is a no-op rather than an error — most
// tenants will not have installed the app.
func TestDeliverWithNoSubscriptions(t *testing.T) {
	s := tenantStore(t)
	n, err := web.Deliver(context.Background(), push.NewSender(vapid(t)), s,
		brief.Brief{Headline: "x"}, time.Now())
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d with no subscriptions", n)
	}
}

var _ = strings.Contains
var _ = json.Marshal
