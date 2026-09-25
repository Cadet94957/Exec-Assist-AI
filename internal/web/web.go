// Package web serves the operator dashboard.
//
// Server-rendered HTML with HTMX, no build step and no JavaScript bundle. A
// dashboard this size does not need a frontend framework, and every dependency
// here is one more thing that can break unattended.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/brief"
	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/obs"
	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const sessionCookie = "eaa_session"

// sessionLifetime bounds how long a signed session value stays valid.
//
// The cookie's MaxAge only tells a browser when to stop sending it, which is
// no protection at all if the value is ever captured. Baking the issue time
// into what is signed means a stolen session expires on its own.
const sessionLifetime = 30 * 24 * time.Hour

// Server holds everything the handlers need.
type Server struct {
	tenants   *tenant.Manager
	templates *template.Template
	secret    []byte
	secure    bool
	google    google.Config
	secrets   *secrets.Box
	brain     *llm.Client
	vapid     push.Keys

	// Set only by EnableLocalHandoff, and therefore zero in every deployment.
	handoff handoff
}

// handoff is the single-use local sign-in described on EnableLocalHandoff.
type handoff struct {
	mu       sync.Mutex
	token    string
	tenantID string
	expires  time.Time
	used     bool
}

// handoffLifetime bounds the window in which the link works. It only has to
// survive a browser launching, so it is short on purpose: a token that stays
// valid all day is a token still sitting in a shell scrollback at 5pm.
const handoffLifetime = 5 * time.Minute

// EnableLocalHandoff allows one sign-in with token, as tenantID, from loopback.
//
// It exists so that running this on your own computer does not require copying
// a 48-character key out of a terminal into a browser form, which is the worst
// moment in the whole first-run experience and is entirely avoidable locally.
//
// It is an authentication bypass, so it is fenced four ways: it must be turned
// on by an explicit call (there is no environment variable that enables it, so
// no deployment can switch it on by accident), the token is compared in
// constant time, the request must come from loopback, and it works exactly
// once. A server built by New and left alone does not serve the route at all.
func (s *Server) EnableLocalHandoff(token, tenantID string) {
	s.handoff.mu.Lock()
	defer s.handoff.mu.Unlock()
	s.handoff.token = token
	s.handoff.tenantID = tenantID
	s.handoff.expires = time.Now().Add(handoffLifetime)
	s.handoff.used = false
}

// New builds the server. secret signs session cookies; secure marks them
// Secure, which should be on everywhere except local development over HTTP.
// google and box may be zero — the calendar feature then reports itself as
// unavailable rather than offering a button that fails.
func New(tenants *tenant.Manager, secret []byte, secure bool, gcfg google.Config, box *secrets.Box, brain *llm.Client, vapid push.Keys) (*Server, error) {
	funcs := template.FuncMap{
		"money": func(cents int64) string { return fmt.Sprintf("$%.2f", float64(cents)/100) },
		"time":  func(t time.Time, loc *time.Location) string { return t.In(loc).Format("Mon 3:04pm") },
		"date":  func(t time.Time, loc *time.Location) string { return t.In(loc).Format("2 Jan 2006") },
		"dur":   humanDuration,
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
		brain:     brain,
		vapid:     vapid,
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
	// Registered unconditionally, but it 404s unless EnableLocalHandoff was
	// called — the guard belongs with the secret it protects, not with the
	// routing table, so that reading the handler tells you the whole story.
	mux.HandleFunc("GET /start", s.handleLocalHandoff)
	mux.Handle("GET /", s.authed(s.handleDashboard))
	mux.Handle("GET /settings", s.authed(s.handleSettings))
	mux.Handle("GET /hub", s.authed(s.handleHub))
	mux.Handle("POST /hub/connect", s.authed(s.handleHubConnect))
	for _, path := range []string{"/hub/disconnect", "/hub/propose", "/hub/decide", "/hub/commitment"} {
		mux.Handle("POST "+path, s.authed(s.handleHubMutation))
	}
	mux.HandleFunc("POST /hooks/{tenant}/{connection}", s.handleReceptionistHook)
	mux.Handle("POST /brief", s.authed(s.handleGenerateBrief))
	mux.Handle("GET /push/key", s.authed(s.handlePushKey))
	mux.Handle("POST /push/subscribe", s.authed(s.handlePushSubscribe))
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
		stores.Metrics().Count(obs.MemoryConfirmed)
		s.noticeAt(w, r, "/settings", "Got it — I'll rely on that.")
		return
	}
	if err := stores.Memory().Reject(id, "Rejected during review by the contractor."); err != nil {
		s.fail(w, "discard that", err)
		return
	}
	stores.Metrics().Count(obs.MemoryRejected)
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

	s.setSession(w, t.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setSession issues the signed session cookie. Both ways of signing in end
// here, so the cookie's shape is decided in exactly one place.
func (s *Server) setSession(w http.ResponseWriter, tenantID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sign(tenantID),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionLifetime.Seconds()),
	})
}

// handleLocalHandoff spends the single-use token minted by quickstart.
//
// Every rejection is a plain 404 rather than an explanation. There is nothing
// useful to tell a caller who should not be here, and a distinct error for
// "wrong token" versus "already used" would confirm to someone probing that
// the feature is switched on at all.
func (s *Server) handleLocalHandoff(w http.ResponseWriter, r *http.Request) {
	s.handoff.mu.Lock()
	defer s.handoff.mu.Unlock()

	switch {
	case s.handoff.token == "": // never enabled: the ordinary case
	case s.handoff.used:
	case time.Now().After(s.handoff.expires):
	case !isLoopback(r.RemoteAddr):
	case subtle.ConstantTimeCompare(
		[]byte(r.URL.Query().Get("t")), []byte(s.handoff.token)) != 1:
	default:
		s.handoff.used = true
		s.setSession(w, s.handoff.tenantID)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.NotFound(w, r)
}

// isLoopback reports whether a request came from this machine.
//
// A request arriving through any proxy or load balancer fails this, which is
// the point: the handoff is for a browser on the same computer that started
// the server, and nothing else.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

	now := time.Now()
	// The same deterministic assembly the brief uses, so the page and the
	// brief can never disagree about what today looks like.
	in, err := brief.Assemble(stores, now)
	if err != nil {
		s.fail(w, "work out your day", err)
		return
	}

	stats, err := stores.Ledger().Stats()
	if err != nil {
		s.fail(w, "read your ledger", err)
		return
	}

	pending, err := stores.Memory().Pending(20)
	if err != nil {
		s.fail(w, "read your memory", err)
		return
	}

	conn, connErr := stores.Ledger().GetConnection(google.Provider)
	if connErr != nil && !errors.Is(connErr, ledger.ErrNoConnection) {
		log.Printf("[web] read calendar connection for %s: %v", t.ID, connErr)
	}

	today, hasBrief, err := stores.Ledger().GetBrief(in.Date)
	if err != nil {
		s.fail(w, "read today's brief", err)
		return
	}
	if hasBrief {
		// Reading it on the page counts as reading it.
		stores.Ledger().MarkBriefOpened(in.Date) //nolint:errcheck
		stores.Metrics().Count(obs.BriefOpened)
	}

	s.render(w, "dashboard.html", map[string]any{
		"Nav":             "today",
		"Tenant":          t,
		"Loc":             in.Loc,
		"Day":             in.Schedule,
		"Concerns":        in.Schedule.Concerns(),
		"Money":           in.Money,
		"Changes":         in.Changes,
		"Commitments":     in.Commitments,
		"Stats":           stats,
		"PendingCount":    len(pending),
		"Today":           in.Date.Format("Monday, 2 January"),
		"Notice":          r.URL.Query().Get("notice"),
		"GoogleConnected": connErr == nil,
		"Connection":      conn,
		"Brief":           today,
		"HasBrief":        hasBrief,
		"BrainReady":      s.brain != nil,
		"PushReady":       s.vapid.Configured(),
	})
}

// handleGenerateBrief writes today's brief on demand. The scheduled 6am run
// uses the same path, so what someone sees here is what they would have been
// sent.
func (s *Server) handleGenerateBrief(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if s.brain == nil {
		s.notice(w, r, "The assistant isn't configured on this server yet.")
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	out, err := WriteBrief(ctx, s.brain, stores, time.Now())
	if err != nil {
		log.Printf("[brief] %s: %v", t.ID, err)
		s.notice(w, r, "Couldn't write your brief just now. Nothing has changed.")
		return
	}
	s.notice(w, r, "Brief ready — "+out.Headline)
}

// handlePushKey hands the browser the public VAPID key it needs to subscribe.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"key": s.vapid.Public}) //nolint:errcheck
}

// handlePushSubscribe records a browser's push endpoint.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request, t tenant.Tenant) {
	if !s.vapid.Configured() {
		http.Error(w, "push is not configured", http.StatusServiceUnavailable)
		return
	}

	var sub push.Subscription
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&sub); err != nil || !sub.Valid() {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}

	stores, err := s.tenants.Stores(t.ID)
	if err != nil {
		s.fail(w, "open your data", err)
		return
	}
	if err := stores.Ledger().SavePushSubscription(ledger.PushSubscription{
		Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth,
	}); err != nil {
		s.fail(w, "save your notification settings", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Deliver sends a brief to every device a tenant has installed, and records
// what actually happened. Push reaches only installed apps, so a silent
// non-delivery has to show up as data rather than as an unexplained absence.
func Deliver(ctx context.Context, sender *push.Sender, stores tenant.TenantStore, b brief.Brief, date time.Time) (int, error) {
	subs, err := stores.Ledger().PushSubscriptions()
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}

	msg := push.Message{
		Title: "Your brief",
		Body:  b.Headline,
		URL:   "/",
		Tag:   "brief-" + date.Format("2006-01-02"),
	}

	var delivered int
	for _, sub := range subs {
		s := push.Subscription{Endpoint: sub.Endpoint}
		s.Keys.P256dh = sub.P256dh
		s.Keys.Auth = sub.Auth

		err := sender.Send(ctx, s, msg)
		switch {
		case errors.Is(err, push.ErrGone):
			// The browser forgot us. Housekeeping, not an incident.
			stores.Ledger().DeletePushSubscription(sub.Endpoint) //nolint:errcheck
		case err != nil:
			log.Printf("[push] %s: %v", stores.Tenant().ID, err)
		default:
			delivered++
			stores.Ledger().MarkPushDelivered(sub.Endpoint) //nolint:errcheck
		}
	}

	if delivered > 0 {
		stores.Ledger().MarkBriefDelivered(date) //nolint:errcheck
		stores.Metrics().Value(obs.BriefDelivered, float64(delivered))
	} else {
		stores.Metrics().Count(obs.BriefFailed)
	}
	return delivered, nil
}

// WriteBrief assembles, renders, stores and meters one brief. Shared by the
// button and the 6am scheduler so they cannot drift apart.
func WriteBrief(ctx context.Context, brain *llm.Client, stores tenant.TenantStore, now time.Time) (brief.Brief, error) {
	in, err := brief.Assemble(stores, now)
	if err != nil {
		return brief.Brief{}, err
	}

	started := time.Now()
	out, err := brief.Render(ctx, brain, in)

	stores.Metrics().RecordAI(obs.AICall{
		Model:      brain.Model(),
		Operation:  "brief",
		Input:      out.Usage.Input,
		Output:     out.Usage.Output,
		CacheRead:  out.Usage.CacheRead,
		CacheWrite: out.Usage.CacheWrite,
		Latency:    time.Since(started),
		Failed:     err != nil,
	})
	if err != nil {
		stores.Metrics().Count(obs.BriefFailed)
		return brief.Brief{}, err
	}

	if err := stores.Ledger().SaveBrief(in.Date, out.Headline, out.Body); err != nil {
		return out, err
	}
	stores.Metrics().Count(obs.BriefGenerated)
	return out, nil
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
	var (
		results  []memory.Recalled
		stops    []ledger.Appointment
		business []ledger.BusinessResult
	)
	if query != "" {
		stores.Metrics().Count(obs.QuestionAsked)

		// An answer draws on both stores: memory knows the rules, the ledger
		// knows what actually happened. Neither alone answers "when did we
		// last do work for Ferguson".
		results, err = stores.Memory().Recall(query, "", 15)
		if err != nil {
			s.fail(w, "search your memory", err)
			return
		}
		business, err = stores.Ledger().SearchBusiness(query)
		if err != nil {
			s.fail(w, "search business records", err)
			return
		}
		stops, err = stores.Ledger().SearchAppointments(query, 5)
		if err != nil {
			s.fail(w, "search your jobs", err)
			return
		}
	}

	s.render(w, "recall_results.html", map[string]any{
		"Query":    query,
		"Results":  results,
		"Business": business,
		"Stops":    stops,
		"Loc":      t.Location(),
	})
}

// --- helpers ---

// humanDuration renders a gap the way someone would say it aloud.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	mins := int(d.Minutes())
	switch {
	case mins < 60:
		return fmt.Sprintf("%d min", mins)
	case mins%60 == 0:
		return fmt.Sprintf("%dh", mins/60)
	default:
		return fmt.Sprintf("%dh %dmin", mins/60, mins%60)
	}
}

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
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	payload := id + "." + issued
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verify(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	id, issuedStr, sig := parts[0], parts[1], parts[2]
	if id == "" {
		return "", false
	}

	want, err := hex.DecodeString(sig)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(id + "." + issuedStr))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return "", false
	}

	issued, err := strconv.ParseInt(issuedStr, 10, 64)
	if err != nil {
		return "", false
	}
	// A session from the future is a clock problem or a forgery attempt;
	// either way it is not something to honour.
	age := time.Since(time.Unix(issued, 0))
	if age < 0 || age > sessionLifetime {
		return "", false
	}
	return id, true
}
