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
	if !s.google.Configured() || s.secrets == nil {
		s.noticeAt(w, r, "/settings", "Calendar connection isn't available yet. Please contact support.")
		return
	}
	http.Redirect(w, r, s.google.AuthURL(s.signState(t.ID)), http.StatusSeeOther)
}

// handleGoogleCallback completes the consent round trip.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		if errParam == "access_denied" {
			s.noticeAt(w, r, "/settings", "Calendar not connected — no changes were made.")
			return
		}
		log.Printf("[google] consent returned %s for %s", errParam, t.ID)
		s.noticeAt(w, r, "/settings", "Google couldn't approve the connection. Please try again.")
		return
	}

	if !s.verifyState(r.URL.Query().Get("state"), t.ID) {
		s.noticeAt(w, r, "/settings", "That connection link expired. Please start again.")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.noticeAt(w, r, "/settings", "Google didn't send an authorization code. Please try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tok, err := s.google.Exchange(ctx, code)
	if err != nil {
		log.Printf("[google] exchange for %s: %v", t.ID, err)
		s.noticeAt(w, r, "/settings", "Couldn't complete the connection with Google. Please try again.")
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	if err := google.Connect(stores.Ledger(), s.secrets, emailFromToken(tok), tok); err != nil {
		log.Printf("[google] store token for %s: %v", t.ID, err)
		s.fail(w, "save the calendar connection", err)
		return
	}

	// Sync immediately so the settings page is populated when they land back
	// on it. An empty screen right after connecting reads as a failure.
	if _, err := google.Sync(ctx, s.google, stores.Ledger(), s.secrets, time.Now()); err != nil {
		log.Printf("[google] first sync for %s: %v", t.ID, err)
		s.noticeAt(w, r, "/settings", "Calendar connected, but the first sync needs attention. We'll keep retrying.")
		return
	}

	s.noticeAt(w, r, "/settings", "Calendar connected and up to date.")
}

// handleSyncGoogle runs a sync on demand.
func (s *Server) handleSyncGoogle(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	if s.secrets == nil {
		s.noticeAt(w, r, "/settings", "Calendar connection isn't available yet. Please contact support.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := google.Sync(ctx, s.google, stores.Ledger(), s.secrets, time.Now())
	if err != nil {
		if errors.Is(err, ledger.ErrNoConnection) {
			s.noticeAt(w, r, "/settings", "No calendar is connected yet.")
			return
		}
		log.Printf("[google] manual sync for %s: %v", t.ID, err)
		s.noticeAt(w, r, "/settings", "Couldn't update your calendar just now. Your existing schedule is unchanged.")
		return
	}
	s.noticeAt(w, r, "/settings", fmt.Sprintf("Calendar up to date — %d events checked.", res.Fetched))
}

// handleDisconnectGoogle removes the local connection and asks Google to
// revoke the refresh token. Local deletion is authoritative for this product:
// even if Google's revoke endpoint is temporarily unavailable, Exec Assist no
// longer has a usable credential after this handler returns.
func (s *Server) handleDisconnectGoogle(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	revokeErr := google.RevokeConnection(ctx, s.google, stores.Ledger(), s.secrets)

	if err := stores.Ledger().DeleteConnection(google.Provider); err != nil {
		s.fail(w, "disconnect the calendar", err)
		return
	}

	if revokeErr != nil && !errors.Is(revokeErr, ledger.ErrNoConnection) {
		log.Printf("[google] revoke during disconnect for %s: %v", t.ID, revokeErr)
		s.noticeAt(w, r, "/settings", "Calendar disconnected from Exec Assist. Google could not confirm remote access was revoked; if needed, remove Exec Assist from your Google Account's connected apps.")
		return
	}
	s.noticeAt(w, r, "/settings", "Calendar disconnected and its appointments removed.")
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
// alongside the access token. The value is display metadata only; the OAuth
// state and authenticated tenant session, not this label, determine which
// tenant receives the connection.
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
