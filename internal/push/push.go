// Package push delivers a brief to an installed app.
//
// Delivery is separated from generation on purpose: §29 moves SMS to Phase 2,
// and when it arrives it should be another Deliverer rather than a rewrite of
// the brief.
//
// A caution worth keeping in view: web push only reaches people who actually
// installed the app, and on iOS only when installed to the home screen. That
// is why every attempt is recorded. If delivery numbers come back low during
// the pilot, the pilot is measuring installation rather than the brief, and
// the answer is to add another channel — not to assume nobody wanted it.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Subscription is one browser's push endpoint.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Valid reports whether a subscription is usable and safe to send to.
//
// The endpoint arrives from the browser, which means an authenticated tenant
// controls it entirely. Without this check they could point it at an internal
// address and have the server make a POST there every morning — a
// server-side request forgery with a scheduler attached. Push services are
// always public HTTPS endpoints, so anything else is rejected.
func (s Subscription) Valid() bool {
	if s.Keys.P256dh == "" || s.Keys.Auth == "" {
		return false
	}
	return ValidEndpoint(s.Endpoint) == nil
}

// ErrUnsafeEndpoint reports an endpoint the server must not be made to call.
var ErrUnsafeEndpoint = errors.New("push: endpoint is not a public https address")

// ValidEndpoint checks a push endpoint before the server will send to it.
//
// Both the literal host and everything it resolves to are checked: a hostname
// under the attacker's control can point at a link-local metadata address just
// as easily as a bare IP can.
func ValidEndpoint(raw string) error {
	if raw == "" {
		return ErrUnsafeEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrUnsafeEndpoint
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrUnsafeEndpoint, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return ErrUnsafeEndpoint
	}

	// A literal IP is checked directly; a name is checked by what it answers.
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return fmt.Errorf("%w: %s is not routable on the public internet", ErrUnsafeEndpoint, ip)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve %s", ErrUnsafeEndpoint, host)
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s", ErrUnsafeEndpoint, host, ip)
		}
	}
	return nil
}

// publicIP reports whether an address is one the server may be directed at.
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate but is
	// just as much someone else's internal network.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// Keys is the VAPID key pair identifying this server to push services.
type Keys struct {
	Public  string
	Private string
	// Subject is a mailto: or https: URL push services can use to contact the
	// operator. Required by the spec; some services reject without it.
	Subject string
}

// GenerateKeys makes a new VAPID pair. Run once — rotating invalidates every
// existing subscription and silently stops all delivery.
func GenerateKeys(subject string) (Keys, error) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return Keys{}, fmt.Errorf("push: generate vapid keys: %w", err)
	}
	return Keys{Public: pub, Private: priv, Subject: subject}, nil
}

// Configured reports whether push can be sent.
func (k Keys) Configured() bool { return k.Public != "" && k.Private != "" }

// Message is what appears on a lock screen.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is where tapping the notification lands.
	URL string `json:"url"`
	Tag string `json:"tag,omitempty"`
}

// ErrGone reports that a subscription is dead and should be deleted. Browsers
// expire them routinely, so this is housekeeping, not an incident.
var ErrGone = errors.New("push: subscription is no longer valid")

// Sender delivers messages to a browser.
type Sender struct {
	keys   Keys
	client *http.Client
	// allowAnyEndpoint disables the public-address check. Only NewTestSender
	// sets it; the server has no way to.
	allowAnyEndpoint bool
}

// NewSender builds a sender. Endpoints are checked before every send.
func NewSender(keys Keys) *Sender {
	return &Sender{keys: keys, client: &http.Client{Timeout: 20 * time.Second}}
}

// NewTestSender builds a sender that will deliver to a loopback address.
//
// It exists so the delivery path can be tested against a local stub. It must
// never be used by the server: the endpoint comes from the browser, and
// skipping the check turns a subscription into a server-side request forgery.
func NewTestSender(keys Keys) *Sender {
	s := NewSender(keys)
	s.allowAnyEndpoint = true
	return s
}

// Send delivers one message. A dead subscription returns ErrGone so the caller
// can forget it rather than retry forever.
func (s *Sender) Send(ctx context.Context, sub Subscription, msg Message) error {
	if !s.keys.Configured() {
		return errors.New("push: no VAPID keys configured")
	}
	if !s.allowAnyEndpoint {
		if err := ValidEndpoint(sub.Endpoint); err != nil {
			return err
		}
	}
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return errors.New("push: subscription is incomplete")
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("push: encode message: %w", err)
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.keys.Subject,
		VAPIDPublicKey:  s.keys.Public,
		VAPIDPrivateKey: s.keys.Private,
		TTL:             int((6 * time.Hour).Seconds()),
		Urgency:         webpush.UrgencyNormal,
	})
	if err != nil {
		return fmt.Errorf("push: send: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 300:
		return fmt.Errorf("push: service returned %d", resp.StatusCode)
	}
	return nil
}
