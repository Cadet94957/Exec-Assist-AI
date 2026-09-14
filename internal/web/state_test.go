package web

import (
	"strings"
	"testing"
	"time"
)

func testServer() *Server {
	return &Server{secret: []byte("a-test-secret-at-least-32-chars-long")}
}

// The OAuth state ties the consent round trip to one signed-in tenant. Without
// this check, an attacker could complete a consent flow and attach their own
// Google calendar to somebody else's account.
func TestStateRoundTrips(t *testing.T) {
	s := testServer()
	state := s.signState("dave-glass")

	if !s.verifyState(state, "dave-glass") {
		t.Error("a freshly signed state did not verify")
	}
	if s.verifyState(state, "kim-glazing") {
		t.Error("a state issued for one tenant verified for another")
	}
}

func TestStateRejectsForgery(t *testing.T) {
	s := testServer()

	other := &Server{secret: []byte("a-different-secret-at-least-32-chars")}
	forged := other.signState("dave-glass")
	if s.verifyState(forged, "dave-glass") {
		t.Error("a state signed with a different secret verified")
	}

	for _, bad := range []string{
		"",
		"not-base64!!",
		"YWJj",                          // valid base64, wrong shape
		s.signState("dave-glass") + "x", // corrupted tail
	} {
		if s.verifyState(bad, "dave-glass") {
			t.Errorf("malformed state %q verified", bad)
		}
	}
}

func TestStateRejectsExpired(t *testing.T) {
	s := testServer()
	// Rebuild a state with an expiry in the past, signed correctly.
	expired := s.signStateAt("dave-glass", -time.Minute)
	if s.verifyState(expired, "dave-glass") {
		t.Error("an expired state verified")
	}
}

func TestStateIsOpaque(t *testing.T) {
	s := testServer()
	state := s.signState("dave-glass")
	// The tenant id travels in the state, but it should not be trivially
	// readable in the URL bar as a bare string.
	if strings.Contains(state, "dave-glass") {
		t.Error("the state exposes the tenant id verbatim")
	}
}
