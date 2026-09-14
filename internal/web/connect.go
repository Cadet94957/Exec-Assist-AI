package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

// oauthStateTTL bounds how long a consent round trip may take. Long enough for
// someone to read the Google consent screen, short enough that a leaked link
// is not useful later.
const oauthStateTTL = 15 * time.Minute

// handleConnectGoogle sends the contractor to Google's consent screen.
func (s *Server) handleConnectGoogle(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if !s.google.Configured() {
		s.notice(w, r, "Calendar connection isn't set up on this server yet.")
		return
	}
	http.Redirect(w, r, s.google.AuthURL(s.signState(t.ID)), http.StatusSeeOther)
}

// handleGoogleCallback completes the consent round trip.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		// access_denied is someone pressing Cancel, not a fault.
		if errParam == "access_denied" {
			s.notice(w, r, "Calendar not connected — you cancelled at the Google screen.")
			return
		}
		log.Printf("[google] consent returned %s for %s", errParam, t.ID)
		s.notice(w, r, "Google refused the connection. Nothing has changed.")
		return
	}

	// The state must be one we issued, unexpired, and for the tenant whose
	// session is making this request — otherwise someone else's calendar could
	// be attached to this account.
	if !s.verifyState(r.URL.Query().Get("state"), t.ID) {
		s.notice(w, r, "That connection link expired. Please try connecting again.")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.notice(w, r, "Google didn't send an authorization code. Please try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tok, err := s.google.Exchange(ctx, code)
	if err != nil {
		log.Printf("[google] exchange for %s: %v", t.ID, err)
		s.notice(w, r, "Couldn't complete the connection with Google. Please try again.")
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	if s.secrets == nil {
		log.Printf("[google] refusing to store a token for %s: TOKEN_KEY is not set", t.ID)
		s.notice(w, r, "This server can't store calendar credentials safely yet. Nothing was saved.")
		return
	}

	if err := google.Connect(stores.Ledger, s.secrets, emailFromToken(tok), tok); err != nil {
		log.Printf("[google] store token for %s: %v", t.ID, err)
		s.fail(w, "save the calendar connection", err)
		return
	}

	// Sync immediately so the dashboard is populated when they land back on
	// it. An empty screen right after connecting reads as a failure.
	if _, err := google.Sync(ctx, s.google, stores.Ledger, s.secrets, time.Now()); err != nil {
		log.Printf("[google] first sync for %s: %v", t.ID, err)
		s.notice(w, r, "Calendar connected. The first sync had trouble — it'll retry shortly.")
		return
	}

	s.notice(w, r, "Calendar connected.")
}

// handleSyncGoogle runs a sync on demand.
func (s *Server) handleSyncGoogle(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := google.Sync(ctx, s.google, stores.Ledger, s.secrets, time.Now())
	if err != nil {
		if errors.Is(err, ledger.ErrNoConnection) {
			s.notice(w, r, "No calendar is connected yet.")
			return
		}
		log.Printf("[google] manual sync for %s: %v", t.ID, err)
		s.notice(w, r, "Couldn't reach Google just now. Your existing schedule is unchanged.")
		return
	}
	s.notice(w, r, fmt.Sprintf("Calendar up to date — %d events.", res.Fetched))
}

// handleDisconnectGoogle removes the connection and everything it brought in.
func (s *Server) handleDisconnectGoogle(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	if err := stores.Ledger.DeleteConnection(google.Provider); err != nil {
		s.fail(w, "disconnect the calendar", err)
		return
	}
	s.notice(w, r, "Calendar disconnected and its appointments removed.")
}

// --- OAuth state ---

// signState produces a state parameter bound to this tenant and to a clock,
// signed with the server secret so it cannot be forged.
func (s *Server) signState(tenantID string) string {
	return s.signStateAt(tenantID, oauthStateTTL)
}

// signStateAt is signState with an explicit lifetime, so expiry is testable.
func (s *Server) signStateAt(tenantID string, ttl time.Duration) string {
	expiry := time.Now().Add(ttl).Unix()
	payload := tenantID + "|" + strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(
		[]byte(payload + "|" + hex.EncodeToString(mac.Sum(nil))))
}

// verifyState checks the signature, the expiry, and that the state belongs to
// the tenant currently signed in.
func (s *Server) verifyState(state, tenantID string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return false
	}
	gotTenant, expiryStr, sig := parts[0], parts[1], parts[2]

	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(gotTenant + "|" + expiryStr))
	want, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(want, mac.Sum(nil)) {
		return false
	}

	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || time.Now().After(time.Unix(expiry, 0)) {
		return false
	}
	return gotTenant == tenantID
}

// emailFromToken reads the account address from the id_token Google returns
// alongside the access token. The token came straight from Google's token
// endpoint over TLS in a direct server-to-server exchange, so its claims can be
// read without verifying the signature. Best effort — it is only a label.
func emailFromToken(tok interface{ Extra(string) any }) string {
	raw, _ := tok.Extra("id_token").(string)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Email
}
