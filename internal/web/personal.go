package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/personal"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

type personalView struct {
	Provider, Name, Account, Error string
	Connected                      bool
	LastSync                       string
}

func (s *Server) personalViews(store *ledger.Store) []personalView {
	var out []personalView
	for _, p := range personal.Providers {
		v := personalView{Provider: p, Name: personal.Name(p), LastSync: "never"}
		c, err := store.GetConnection(p)
		if err == nil {
			v.Connected = true
			v.Account = c.Account
			v.Error = c.LastError.String
			if c.LastSync.Valid {
				v.LastSync = c.LastSync.Time.UTC().Format("Jan 2 15:04 UTC")
			}
		}
		out = append(out, v)
	}
	return out
}
func (s *Server) handlePersonal(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.hubForm(w, r, t) {
		return
	}
	p := r.PathValue("provider")
	if !personal.Valid(p) {
		http.NotFound(w, r)
		return
	}
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	action := r.PathValue("action")
	switch action {
	case "connect":
		err = s.personal.Connect(ctx, stores.Ledger(), p, personal.Credential{Account: r.PostFormValue("account"), Password: r.PostFormValue("password")}, t.Location())
	case "sync":
		err = s.personal.Sync(ctx, stores.Ledger(), p, t.Location())
	case "disconnect":
		err = s.personal.Disconnect(stores.Ledger(), p)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.noticeAt(w, r, "/settings", err.Error())
		return
	}
	message := personal.Name(p) + " is up to date."
	if action == "disconnect" {
		message = personal.Name(p) + " disconnected. Its local calendar or inbox copies were removed. Revoke the app password in your account settings to invalidate it at the provider."
	}
	s.noticeAt(w, r, "/settings", message)
}
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	w.Header().Set("Cache-Control", "no-store")
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your inbox", err)
		return
	}
	messages, err := stores.Ledger().RecentMail(time.Now().AddDate(0, 0, -30), 400)
	if err != nil {
		s.fail(w, "read your inbox", err)
		return
	}
	s.render(w, "inbox.html", map[string]any{"Nav": "inbox", "Tenant": t, "Loc": t.Location(), "Messages": messages, "PersonalConnections": s.personalViews(stores.Ledger())})
}

// SyncPersonal runs on the same service as Settings operations, so disconnect
// waits for in-flight sync before removing credentials and local copies.
func (s *Server) SyncPersonal(ctx context.Context) {
	if !s.personal.Ready() {
		return
	}
	all, err := s.tenants.List()
	if err != nil {
		return
	}
	for _, t := range all {
		stores, err := s.tenants.Stores(t.ID)
		if err != nil {
			continue
		}
		for _, p := range personal.Providers {
			if ctx.Err() != nil {
				return
			}
			call, cancel := context.WithTimeout(ctx, 90*time.Second)
			err = s.personal.Sync(call, stores.Ledger(), p, t.Location())
			cancel()
			if err != nil && !errors.Is(err, ledger.ErrNoConnection) {
				log.Printf("[personal] %s sync failed for tenant %s", p, t.ID)
			}
		}
	}
}
