package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tryHandoff issues a request to the handoff route as if it came from the given
// address, and reports the status and whether a session cookie came back.
func tryHandoff(s *Server, token, from string) (int, bool) {
	r := httptest.NewRequest(http.MethodGet, "/start?t="+token, nil)
	r.RemoteAddr = from
	w := httptest.NewRecorder()
	s.handleLocalHandoff(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return w.Code, true
		}
	}
	return w.Code, false
}

// The whole point of the handoff is that it is off unless something asked for
// it. A server built the ordinary way must behave as though the route does not
// exist — this is the test that would fail if the feature ever leaked into a
// deployment.
func TestHandoffIsOffByDefault(t *testing.T) {
	s := testServer()

	code, signedIn := tryHandoff(s, "any-token", "127.0.0.1:5000")
	if signedIn {
		t.Fatal("a server with no handoff enabled signed a request in")
	}
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}

	// An empty token must not match the zero-value empty token either.
	if _, signedIn := tryHandoff(s, "", "127.0.0.1:5000"); signedIn {
		t.Error("an empty token matched the disabled server's empty token")
	}
}

func TestHandoffSignsInOnce(t *testing.T) {
	s := testServer()
	s.EnableLocalHandoff("good-token", "my-business")

	code, signedIn := tryHandoff(s, "good-token", "127.0.0.1:5000")
	if !signedIn {
		t.Fatalf("the first use did not sign in (status %d)", code)
	}
	if code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", code)
	}

	// Single use is what keeps a link left in a terminal, a shell history or a
	// screen share from being worth anything later.
	if code, signedIn := tryHandoff(s, "good-token", "127.0.0.1:5000"); signedIn {
		t.Errorf("the same token signed in twice (status %d)", code)
	}
}

func TestHandoffRejections(t *testing.T) {
	cases := []struct {
		name, token, from string
		expire            bool
	}{
		{name: "wrong token", token: "nope", from: "127.0.0.1:5000"},
		{name: "empty token", token: "", from: "127.0.0.1:5000"},
		// Anything arriving through a proxy or from another machine fails
		// this, which is the entire security argument for the feature.
		{name: "remote address", token: "good-token", from: "203.0.113.7:5000"},
		{name: "private network", token: "good-token", from: "192.168.1.20:5000"},
		{name: "expired", token: "good-token", from: "127.0.0.1:5000", expire: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testServer()
			s.EnableLocalHandoff("good-token", "my-business")
			if c.expire {
				s.handoff.expires = time.Now().Add(-time.Second)
			}

			code, signedIn := tryHandoff(s, c.token, c.from)
			if signedIn {
				t.Error("signed in when it should not have")
			}
			// Always a plain 404: a different response for "wrong token" than
			// for "already used" would confirm the feature is switched on.
			if code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", code)
			}
		})
	}
}

// IPv6 loopback is still this machine, and is what a browser hitting
// "localhost" often resolves to.
func TestHandoffAcceptsIPv6Loopback(t *testing.T) {
	s := testServer()
	s.EnableLocalHandoff("good-token", "my-business")

	if _, signedIn := tryHandoff(s, "good-token", "[::1]:5000"); !signedIn {
		t.Error("a request from ::1 was rejected")
	}
}
