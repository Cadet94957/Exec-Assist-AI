package push_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
)

func keys(t *testing.T) push.Keys {
	t.Helper()
	k, err := push.GenerateKeys("mailto:ops@example.com")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !k.Configured() {
		t.Fatal("generated keys report themselves unconfigured")
	}
	return k
}

func subscription(endpoint string) push.Subscription {
	s := push.Subscription{Endpoint: endpoint}
	// Valid base64url test material of the right shapes.
	s.Keys.P256dh = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"
	s.Keys.Auth = "tBHItJI5svbpez7KI4CCXg"
	return s
}

func TestSendReachesThePushService(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := push.NewSender(keys(t)).Send(context.Background(), subscription(srv.URL),
		push.Message{Title: "Your brief", Body: "3 stops today.", URL: "/"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// VAPID identifies this server to the push service; without it the
	// service rejects the send and nothing is delivered.
	if !strings.Contains(strings.ToLower(gotAuth), "vapid") {
		t.Errorf("Authorization = %q, want a VAPID credential", gotAuth)
	}
}

// Browsers expire subscriptions routinely. Retrying a dead one forever is how
// a delivery loop turns into a slow leak.
func TestGoneSubscriptionIsIdentifiable(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))

		err := push.NewSender(keys(t)).Send(context.Background(), subscription(srv.URL),
			push.Message{Title: "x", Body: "y"})
		srv.Close()

		if !errors.Is(err, push.ErrGone) {
			t.Errorf("status %d: got %v, want ErrGone", code, err)
		}
	}
}

func TestSendRefusesWithoutConfiguration(t *testing.T) {
	err := push.NewSender(push.Keys{}).Send(context.Background(), subscription("https://example.test"),
		push.Message{Title: "x"})
	if err == nil {
		t.Error("sending with no VAPID keys was allowed")
	}
}

func TestIncompleteSubscriptionIsRejected(t *testing.T) {
	var bare push.Subscription
	bare.Endpoint = "https://example.test"
	if bare.Valid() {
		t.Error("a subscription with no keys reported itself valid")
	}
	if err := push.NewSender(keys(t)).Send(context.Background(), bare, push.Message{}); err == nil {
		t.Error("an incomplete subscription was accepted")
	}
}
