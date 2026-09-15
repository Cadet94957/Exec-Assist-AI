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
	"net/http"
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

// Valid reports whether a subscription is usable.
func (s Subscription) Valid() bool {
	return s.Endpoint != "" && s.Keys.P256dh != "" && s.Keys.Auth != ""
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
}

// NewSender builds a sender.
func NewSender(keys Keys) *Sender {
	return &Sender{keys: keys, client: &http.Client{Timeout: 20 * time.Second}}
}

// Send delivers one message. A dead subscription returns ErrGone so the caller
// can forget it rather than retry forever.
func (s *Sender) Send(ctx context.Context, sub Subscription, msg Message) error {
	if !s.keys.Configured() {
		return errors.New("push: no VAPID keys configured")
	}
	if !sub.Valid() {
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
