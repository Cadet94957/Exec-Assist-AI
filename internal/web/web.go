// Package web serves the operator dashboard.
//
// Server-rendered HTML with HTMX, no build step and no JavaScript bundle. A
// dashboard this size does not need a frontend framework, and every dependency
// here is one more thing that can break unattended.
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const sessionCookie = "eaa_session"

// Server holds everything the handlers need.
type Server struct {
	tenants   *tenant.Manager
	templates *template.Template
	secret    []byte
	secure    bool
	google    google.Config
	secrets   *secrets.Box
}

// New builds the server. secret signs session cookies; secure marks them
// Secure, which should be on everywhere except local development over HTTP.
// google and box may be zero — the calendar feature then reports itself as
// unavailable rather than offering a button that fails.
func New(tenants *tenant.Manager, secret []byte, secure bool, gcfg google.Config, box *secrets.Box) (*Server, error) {
	funcs := template.FuncMap{
		"money": func(cents int64) string { return fmt.Sprintf("$%.2f", float64(cents)/100) },
		"time":  func(t time.Time, loc *time.Location) string { return t.In(loc).Format("Mon 3:04pm") },
		"ago": func(t sql.NullTime) string {
			if !t.Valid {
				return "never"
			}
			d := time.Since(t.Time)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d hr ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%d days ago", int(d.Hours()/24))
			}
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return &Server{
		tenants:   tenants,
		templates: tmpl,
		secret:    secret,
		secure:    secure,
		google:    gcfg,
		secrets:   box,
	}, nil
}

// Routes returns the mux for the public listener.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Static assets are immutable per build, so they can be cached hard. The
	// manifest and service worker are not: a stale one strands installed apps
	// on an old start URL or an old worker.
	mux.Handle("GET /static/", cacheFor(24*time.Hour, http.FileServerFS(staticFS)))
	mux.Handle("GET /static/manifest.json", cacheFor(0, http.FileServerFS(staticFS)))
	mux.Handle("GET /static/sw.js", cacheFor(0, http.FileServerFS(staticFS)))

	// The service worker is also served from the root so its scope covers the
	// whole app; a worker under /static/ can only control /static/.
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticFS, "static/sw.js")
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon-32.png", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.Handle("GET /", s.authed(s.handleDashboard))
	mux.Handle("GET /settings", s.authed(s.handleSettings))
	mux.Handle("GET /recall", s.authed(s.handleRecall))
	// The memory used to live at /learned. Redirect rather than break a
	// bookmark or a link in an old email.
	mux.HandleFunc("GET /learned", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings#memory", http.StatusMovedPermanently)
	})
	mux.Handle("GET /connect/google", s.authed(s.handleConnectGoogle))
	mux.Handle("GET /connect/google/callback", s.authed(s.handleGoogleCallback))
	mux.Handle("POST /connect/google/sync", s.authed(s.handleSyncGoogle))
	mux.Handle("POST /connect/google/disconnect", s.authed(s.handleDisconnectGoogle))
	mux.Handle("POST /memory/confirm", s.authed(s.handleConfirmFact))
	mux.Handle("POST /memory/discard", s.authed(s.handleDiscardFact))
	mux.Handle("POST /memory/forget", s.authed(s.handleForgetFact))
	return mux
}

// handleConfirmFact accepts an inferred fact.
func (s *Server) handleConfirmFact(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	s.reviewFact(w, r, t, true)
}

// handleDiscardFact rejects one. memoryweb soft-deletes, so a mistake here is
// recoverable — which matters when the contractor is skimming.
func (s *Server) handleDiscardFact(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	s.reviewFact(w, r, t, false)
}

func (s *Server) reviewFact(w http.ResponseWriter, r *http.Request, t tenant.Tenant, keep bool) {
	if err := r.ParseForm(); err != nil {
		s.noticeAt(w, r, "/settings", "Couldn't read that.")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		s.noticeAt(w, r, "/settings", "Couldn't tell which one you meant.")
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	if keep {
		if err := stores.Memory().Confirm(id); err != nil {
			s.fail(w, "save that", err)
			return
		}
		s.noticeAt(w, r, "/settings", "Got it — I'll rely on that.")
		return
	}
	if err := stores.Memory().Reject(id, "Rejected during review by the contractor."); err != nil {
		s.fail(w, "discard that", err)
		return
	}
	s.noticeAt(w, r, "/settings", "Dropped it.")
}

// handleForgetFact removes something the assistant had been relying on. This
// is how a contractor corrects a wrong belief, so it has to be as easy as
// confirming one — and it archives rather than deletes.
func (s *Server) handleForgetFact(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if err := r.ParseForm(); err != nil {
		s.noticeAt(w, r, "/settings", "Couldn't read that.")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		s.noticeAt(w, r, "/settings", "Couldn't tell which one you meant.")
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	if err := stores.Memory().Forget(id, "Removed by the contractor from Settings."); err != nil {
		s.fail(w, "forget that", err)
		return
	}
	s.noticeAt(w, r, "/settings", "Forgotten.")
}

// noticeAt redirects to a path carrying a short message.
func (s *Server) noticeAt(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

// notice redirects home carrying a short message for the contractor. Keeping
// it in the query string avoids a flash-session store for what is one sentence.
func (s *Server) notice(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

// authed wraps a handler so it only runs for a signed-in tenant.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, tenant.Tenant)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.currentTenant(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, t)
	})
}

// cacheFor sets a max-age on a static handler. A zero duration means
// no-cache, for the two files that must never go stale.
func cacheFor(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d <= 0 {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=Could+not+read+the+form", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("tenant"))
	key := strings.TrimSpace(r.PostFormValue("key"))

	t, err := s.tenants.Authenticate(id, key)
	if err != nil {
		if !errors.Is(err, tenant.ErrNotFound) {
			log.Printf("[web] authenticate %s: %v", id, err)
		}
		http.Redirect(w, r, "/login?error=That+account+and+key+do+not+match", http.StatusSeeOther)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sign(t.ID),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	stats, err := stores.Ledger().Stats()
	if err != nil {
		s.fail(w, "read your ledger", err)
		return
	}

	loc := t.Location()
	dayStart := t.StartOfDay(time.Now())
	appts, err := stores.Ledger().Between(dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		s.fail(w, "read today's schedule", err)
		return
	}

	pending, err := stores.Memory().Pending(20)
	if err != nil {
		s.fail(w, "read your memory", err)
		return
	}

	conn, connErr := stores.Ledger().GetConnection(google.Provider)
	connected := connErr == nil
	if connErr != nil && !errors.Is(connErr, ledger.ErrNoConnection) {
		log.Printf("[web] read calendar connection for %s: %v", t.ID, connErr)
	}

	s.render(w, "dashboard.html", map[string]any{
		"Nav":             "today",
		"Tenant":          t,
		"Loc":             loc,
		"Stats":           stats,
		"Appointments":    appts,
		"PendingCount":    len(pending),
		"Today":           dayStart.Format("Monday, 2 January"),
		"Notice":          r.URL.Query().Get("notice"),
		"GoogleReady":     s.google.Configured() && s.secrets != nil,
		"GoogleConnected": connected,
		"Connection":      conn,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	domain := r.URL.Query().Get("domain")
	sig, err := stores.Memory().Learned(domain, 40)
	if err != nil {
		s.fail(w, "read what I've learned", err)
		return
	}

	// Inferred facts are held back for review rather than mixed in with what
	// the assistant already relies on.
	pending, err := stores.Memory().Pending(20)
	if err != nil {
		s.fail(w, "read what's waiting for review", err)
		return
	}
	// Inferred facts are shown in the review queue, not mixed in with what the
	// assistant already relies on.
	settled := func(in []memory.Recalled) []memory.Recalled {
		out := make([]memory.Recalled, 0, len(in))
		for _, r := range in {
			if !r.AwaitingReview() {
				out = append(out, r)
			}
		}
		return out
	}
	domains, err := stores.Memory().Domains()
	if err != nil {
		s.fail(w, "list your areas", err)
		return
	}

	conn, connErr := stores.Ledger().GetConnection(google.Provider)
	if connErr != nil && !errors.Is(connErr, ledger.ErrNoConnection) {
		log.Printf("[web] read calendar connection for %s: %v", t.ID, connErr)
	}

	important, recent := settled(sig.Important), settled(sig.Recent)
	s.render(w, "settings.html", map[string]any{
		"Nav":             "settings",
		"Tenant":          t,
		"Domains":         domains,
		"Domain":          domain,
		"Important":       important,
		"Recent":          recent,
		"Pending":         pending,
		"Notice":          r.URL.Query().Get("notice"),
		"Total":           len(important) + len(recent) + len(pending),
		"GoogleReady":     s.google.Configured() && s.secrets != nil,
		"GoogleConnected": connErr == nil,
		"Connection":      conn,
	})
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var results []memory.Recalled
	if query != "" {
		results, err = stores.Memory().Recall(query, "", 15)
		if err != nil {
			s.fail(w, "search your memory", err)
			return
		}
	}

	// HTMX swaps just the results fragment back into the page.
	s.render(w, "recall_results.html", map[string]any{"Query": query, "Results": results})
}

// --- helpers ---

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("[web] render %s: %v", name, err)
	}
}

// fail logs the real error and shows the operator a plain sentence. The
// contractor reading this is not going to debug a stack trace.
func (s *Server) fail(w http.ResponseWriter, action string, err error) {
	log.Printf("[web] failed to %s: %v", action, err)
	http.Error(w, "Sorry — I couldn't "+action+" just now. It's been logged.", http.StatusInternalServerError)
}

func (s *Server) currentTenant(r *http.Request) (tenant.Tenant, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return tenant.Tenant{}, false
	}
	id, ok := s.verify(c.Value)
	if !ok {
		return tenant.Tenant{}, false
	}
	t, err := s.tenants.Get(id)
	if err != nil {
		return tenant.Tenant{}, false
	}
	return t, true
}

func (s *Server) sign(id string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(id))
	return id + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verify(value string) (string, bool) {
	id, sig, found := strings.Cut(value, ".")
	if !found || id == "" {
		return "", false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(id))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return "", false
	}
	return id, true
}
