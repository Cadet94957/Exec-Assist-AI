package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/receptionist"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

// hubForm applies a session-bound CSRF check to every hub mutation. Source
// webhooks use separate HMAC credentials and never enter these handlers.
func (s *Server) hubForm(w http.ResponseWriter, r *http.Request, t tenant.Tenant) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil || !s.verifyState(r.PostFormValue("csrf"), t.ID) {
		http.Error(w, "This form expired. Reload and try again.", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleHub(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	connections, err := stores.Ledger().HubConnections()
	if err != nil {
		s.fail(w, "read connections", err)
		return
	}
	jobs, err := stores.Ledger().HubJobs()
	if err != nil {
		s.fail(w, "read jobs", err)
		return
	}
	commitments, err := stores.Ledger().Commitments(true)
	if err != nil {
		s.fail(w, "read follow-ups", err)
		return
	}
	actions, err := stores.Ledger().HubActions()
	if err != nil {
		s.fail(w, "read decisions", err)
		return
	}
	appointments, err := stores.Ledger().Between(time.Now().AddDate(0, 0, -30), time.Now().AddDate(0, 0, 60))
	if err != nil {
		s.fail(w, "read calendar", err)
		return
	}
	s.render(w, "hub.html", map[string]any{
		"Nav": "today", "Tenant": t, "Loc": t.Location(), "CSRF": s.signState(t.ID),
		"Connections": connections, "Jobs": jobs, "Commitments": commitments,
		"Actions": actions, "Appointments": appointments, "HubReady": s.secrets != nil,
		"Notice": r.URL.Query().Get("notice"),
	})
}

func (s *Server) handleHubConnect(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if !s.hubForm(w, r, t) {
		return
	}
	if s.secrets == nil {
		http.Error(w, "Connection encryption is not configured.", http.StatusServiceUnavailable)
		return
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		s.fail(w, "create connection key", err)
		return
	}
	// Use the displayed hex string as the HMAC key, avoiding ambiguity for
	// webhook adapters about whether to decode it before signing.
	secret := hex.EncodeToString(key[:])
	sealed, err := s.secrets.Seal([]byte(secret))
	if err != nil {
		s.fail(w, "protect connection key", err)
		return
	}
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	id, err := stores.Ledger().AddHubConnection(r.PostFormValue("name"), sealed)
	if err != nil {
		s.hubError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.render(w, "hub_connected.html", map[string]any{"Tenant": t, "Secret": secret, "HookPath": "/hooks/" + t.ID + "/" + id})
}

func (s *Server) handleHubMutation(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if !s.hubForm(w, r, t) {
		return
	}
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	switch r.URL.Path {
	case "/hub/disconnect":
		err = stores.Ledger().DisableHubConnection(r.PostFormValue("id"))
	case "/hub/propose":
		err = stores.Ledger().ProposeAppointmentLink(r.PostFormValue("job"), r.PostFormValue("appointment"))
	case "/hub/decide":
		decision := r.PostFormValue("decision")
		if decision != "approve" && decision != "reject" {
			err = ledger.ErrHubInput
		} else {
			err = stores.Ledger().DecideHubAction(r.PostFormValue("id"), decision == "approve")
		}
	case "/hub/commitment":
		err = stores.Ledger().UpdateCommitment(r.PostFormValue("id"), r.PostFormValue("owner"), r.PostFormValue("waiting"))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.hubError(w, err)
		return
	}
	http.Redirect(w, r, "/hub?notice="+url.QueryEscape("Saved. The activity record has been updated."), http.StatusSeeOther)
}

func (s *Server) hubError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrHubConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ledger.ErrHubInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		s.fail(w, "update hub", err)
	}
}

func (s *Server) handleReceptionistHook(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		http.Error(w, "Webhook unavailable", http.StatusServiceUnavailable)
		return
	}
	// Manager validates tenant IDs; a URL never directly selects a file path.
	stores, err := s.tenants.Stores(r.PathValue("tenant"))
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	connection := r.PathValue("connection")
	c, err := stores.Ledger().HubConnection(connection)
	if err != nil || !c.Enabled {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	secret, err := s.secrets.Open(c.Secret)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now().UTC()
	if !receptionist.Verify(secret, r.Header.Get("X-ExecAssist-Timestamp"), r.Header.Get("X-ExecAssist-Signature"), body, now) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var e ledger.HubEvent
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		http.Error(w, "Invalid event JSON", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Expected one event", http.StatusBadRequest)
		return
	}
	result, err := stores.Ledger().IngestHubEvent(connection, e, now)
	if err != nil {
		s.hubError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}
