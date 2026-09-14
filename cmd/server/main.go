// Command server runs the whole product: one binary, one process.
//
// It serves the operator dashboard on the public port and, on a loopback-only
// listener, an Ollama-shaped embeddings endpoint that the memory graph calls
// back into. Keeping both in one process means there is no sidecar to deploy,
// supervise, or forget to restart.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	googleconnect "github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/embed"
	"github.com/Cadet94957/Exec-Assist-AI/internal/extract"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
	"github.com/Cadet94957/Exec-Assist-AI/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create-tenant":
			if err := createTenant(os.Args[2:]); err != nil {
				log.Fatalf("create-tenant: %v", err)
			}
			return
		case "seed-demo":
			if err := seedDemo(os.Args[2:]); err != nil {
				log.Fatalf("seed-demo: %v", err)
			}
			return
		case "backfill":
			if err := backfill(os.Args[2:]); err != nil {
				log.Fatalf("backfill: %v", err)
			}
			return
		}
	}

	if err := serve(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func serve() error {
	addr := ":" + envOr("PORT", "8080")
	dataDir := envOr("DATA_DIR", "./data")

	secret, err := sessionSecret()
	if err != nil {
		return err
	}

	// Bring up the embeddings shim first and point memoryweb at it, before any
	// graph is opened. Loopback only — exposing it would let anyone spend the
	// embedding budget.
	embedAddr, stopEmbed, err := startEmbedShim()
	if err != nil {
		return err
	}
	defer stopEmbed()

	provider := embed.FromEnv(os.Getenv("VOYAGE_API_KEY"), os.Getenv("EMBED_MODEL"))
	if _, disabled := provider.(embed.Disabled); disabled {
		log.Printf("[embed] no VOYAGE_API_KEY set — memory recall will use literal matching")
	} else {
		log.Printf("[embed] provider %s serving on %s", provider.Name(), embedAddr)
	}

	tenants, err := tenant.NewManager(dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	gcfg := googleconnect.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
	}

	// A missing TOKEN_KEY is not fatal — the rest of the product works — but
	// it does mean no third-party credential can be stored, so say so once
	// rather than failing later at the moment someone tries to connect.
	var box *secrets.Box
	if key := os.Getenv("TOKEN_KEY"); key != "" {
		box, err = secrets.NewBox(key)
		if err != nil {
			return err
		}
	} else {
		log.Print("[connect] TOKEN_KEY is unset — calendar connection is disabled (generate one with: openssl rand -hex 32)")
	}

	switch {
	case !gcfg.Configured():
		log.Print("[google] GOOGLE_CLIENT_ID/SECRET/REDIRECT_URL not all set — calendar connection is disabled")
	case box == nil:
		log.Print("[google] credentials present but TOKEN_KEY is not — calendar connection is disabled")
	default:
		log.Print("[google] calendar connection enabled")
	}

	llmCfg := llmConfigFromEnv()
	var brain *llm.Client
	if llmCfg.Configured() {
		brain, err = llm.New(llmCfg)
		if err != nil {
			return err
		}
		log.Printf("[extract] enabled, model %s", brain.Model())
	} else {
		log.Print("[extract] ANTHROPIC_API_KEY is unset — nothing will be learned from synced data")
	}

	site, err := web.New(tenants, secret, os.Getenv("INSECURE_COOKIES") != "1", gcfg, box)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           site.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shut down on SIGTERM so Fly's deploys don't kill a request mid-flight,
	// and so every tenant's WAL gets checkpointed back into its .db file.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Keep every connected calendar fresh in the background. Without this a
	// contractor only sees changes when they happen to open the dashboard,
	// and the 6am brief would be built from whatever was last fetched.
	if gcfg.Configured() && box != nil {
		go syncLoop(ctx, gcfg, tenants, box)
	}

	// Learning runs far less often than syncing: patterns emerge over weeks,
	// and every pass costs money.
	if brain != nil {
		go extractLoop(ctx, brain, tenants)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[web] listening on %s, data in %s", addr, dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		log.Print("[web] shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// syncInterval is how often connected calendars are re-fetched. Frequent
// enough that a booking made this morning is in this afternoon's plan, and far
// under Google's per-user quota.
const syncInterval = 10 * time.Minute

// syncLoop refreshes every connected tenant's calendar until the server stops.
func syncLoop(ctx context.Context, cfg googleconnect.Config, tenants *tenant.Manager, box *secrets.Box) {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAll(ctx, cfg, tenants, box)
		}
	}
}

func syncAll(ctx context.Context, cfg googleconnect.Config, tenants *tenant.Manager, box *secrets.Box) {
	all, err := tenants.List()
	if err != nil {
		log.Printf("[google] list tenants: %v", err)
		return
	}

	for _, t := range all {
		if ctx.Err() != nil {
			return
		}
		stores, err := tenants.Stores(t.ID)
		if err != nil {
			log.Printf("[google] open %s: %v", t.ID, err)
			continue
		}

		// One slow tenant must not stall every other tenant's sync.
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := googleconnect.Sync(callCtx, cfg, stores.Ledger, box, time.Now())
		cancel()

		switch {
		case errors.Is(err, ledger.ErrNoConnection):
			// Most tenants simply have not connected a calendar.
		case err != nil:
			log.Printf("[google] sync %s: %v", t.ID, err)
		case res.Fetched > 0 || res.Removed > 0:
			log.Printf("[google] sync %s: %d events, %d removed", t.ID, res.Fetched, res.Removed)
		}
	}
}

// llmConfigFromEnv reads the model settings. ANTHROPIC_BASE_URL exists so the
// whole path can be pointed at a gateway, a proxy, or a stub during testing.
func llmConfigFromEnv() llm.Config {
	return llm.Config{
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		Model:   os.Getenv("ANTHROPIC_MODEL"),
		BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
	}
}

// extractInterval is how often the assistant re-reads a tenant's recent
// history looking for rules. Daily is plenty — a weekday pattern does not
// appear between lunch and dinner — and it keeps per-tenant spend predictable.
const extractInterval = 24 * time.Hour

func extractLoop(ctx context.Context, brain *llm.Client, tenants *tenant.Manager) {
	ticker := time.NewTicker(extractInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			extractAll(ctx, brain, tenants)
		}
	}
}

func extractAll(ctx context.Context, brain *llm.Client, tenants *tenant.Manager) {
	all, err := tenants.List()
	if err != nil {
		log.Printf("[extract] list tenants: %v", err)
		return
	}
	for _, t := range all {
		if ctx.Err() != nil {
			return
		}
		res, err := runExtract(ctx, brain, tenants, t)
		if err != nil {
			log.Printf("[extract] %s: %v", t.ID, err)
			continue
		}
		if res.Recorded > 0 || res.Rejected > 0 {
			log.Printf("[extract] %s: proposed %d, recorded %d, duplicates %d, rejected %d",
				t.ID, res.Proposed, res.Recorded, res.Duplicates, res.Rejected)
		}
	}
}

func runExtract(ctx context.Context, brain *llm.Client, tenants *tenant.Manager, t tenant.Tenant) (extract.Result, error) {
	stores, err := tenants.Stores(t.ID)
	if err != nil {
		return extract.Result{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	return extract.Run(callCtx, brain,
		extract.Stores{Ledger: stores.Ledger, Memory: stores.Memory},
		t.Location(), time.Now())
}

// backfill runs one extraction pass now, rather than waiting for the daily
// tick. This is what turns a freshly connected calendar into a populated graph
// while someone is still watching.
func backfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	id := fs.String("id", "", "tenant to learn from; omit for all")
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	brain, err := llm.New(llmConfigFromEnv())
	if err != nil {
		return err
	}

	tenants, err := tenant.NewManager(*dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	list, err := tenants.List()
	if err != nil {
		return err
	}

	ctx := context.Background()
	ran := 0
	for _, t := range list {
		if *id != "" && t.ID != *id {
			continue
		}
		ran++
		res, err := runExtract(ctx, brain, tenants, t)
		if err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
		fmt.Printf("%s: read %d appointments, proposed %d, recorded %d (%d duplicates, %d rejected)\n",
			t.Name, res.Appointments, res.Proposed, res.Recorded, res.Duplicates, res.Rejected)
		fmt.Printf("  tokens: %d in (%d from cache), %d out\n",
			res.Usage.Input, res.Usage.CacheRead, res.Usage.Output)
	}
	if ran == 0 {
		return errors.New("no matching tenant")
	}
	fmt.Println("\nAnything new is waiting for review under Settings.")
	return nil
}

// startEmbedShim binds a loopback listener, serves the Ollama-shaped endpoint
// on it, and sets MEMORYWEB_OLLAMA_ENDPOINT so the graph finds it.
func startEmbedShim() (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("embed listener: %w", err)
	}

	provider := embed.FromEnv(os.Getenv("VOYAGE_API_KEY"), os.Getenv("EMBED_MODEL"))
	srv := &http.Server{
		Handler:           embed.Handler(provider),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[embed] serve: %v", err)
		}
	}()

	endpoint := "http://" + ln.Addr().String() + "/api/embed"
	if err := os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", endpoint); err != nil {
		return "", nil, fmt.Errorf("set embed endpoint: %w", err)
	}

	return endpoint, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}, nil
}

func createTenant(args []string) error {
	fs := flag.NewFlagSet("create-tenant", flag.ExitOnError)
	id := fs.String("id", "", "short account id, e.g. dave-glass (required)")
	name := fs.String("name", "", "business name (required)")
	trade := fs.String("trade", "glazier", "trade")
	tz := fs.String("tz", "UTC", "IANA timezone, e.g. America/Los_Angeles")
	phone := fs.String("phone", "", "mobile number for the daily brief")
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		fs.Usage()
		return errors.New("both -id and -name are required")
	}

	tenants, err := tenant.NewManager(*dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	t, key, err := tenants.Create(*id, *name, *trade, *tz, *phone)
	if err != nil {
		return err
	}

	fmt.Printf("Created %s (%s)\n", t.Name, t.ID)
	fmt.Printf("Data:   %s\n", tenants.Dir(t.ID))
	fmt.Printf("Key:    %s\n\n", key)
	fmt.Println("Save that key now — it is not stored and cannot be shown again.")
	return nil
}

// seedDemo fills a tenant with a plausible day and a handful of learned facts,
// so the dashboard and the "What I've Learned" page can be judged before any
// real integration exists. It is a demo aid, not part of the product: point it
// at a throwaway tenant, never at a paying one.
func seedDemo(args []string) error {
	fs := flag.NewFlagSet("seed-demo", flag.ExitOnError)
	id := fs.String("id", "", "tenant to seed (required)")
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		fs.Usage()
		return errors.New("-id is required")
	}

	tenants, err := tenant.NewManager(*dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	t, err := tenants.Get(*id)
	if err != nil {
		return err
	}
	stores, err := tenants.Stores(*id)
	if err != nil {
		return err
	}

	// A day's stops, in deliberately scrambled order — the ledger sorts them.
	day := t.StartOfDay(time.Now())
	for _, a := range []struct {
		id, title, where string
		hour             int
	}{
		{"demo-2", "Marriott 4th St — lobby panel measure", "1200 4th St", 11},
		{"demo-1", "Harborview — storefront reglaze", "88 Harbor Way", 8},
		{"demo-3", "Ferguson residence — shower door fit", "41 Cedar Ln", 14},
	} {
		if err := stores.Ledger.UpsertAppointment(ledger.Appointment{
			ID:       a.id,
			Title:    a.title,
			StartsAt: day.Add(time.Duration(a.hour) * time.Hour).UTC(),
			Location: sql.NullString{String: a.where, Valid: true},
			Source:   "manual",
		}); err != nil {
			return err
		}
	}

	// The learned layer: rules and lessons, never the jobs themselves.
	facts := []memory.Fact{
		{Label: "Harborview gate code is 4417", Domain: "properties", Kind: memory.Reference,
			Detail:     "North entrance keypad. The loading dock is locked after 3pm.",
			WhyMatters: "Without it the crew waits at the gate and the stop runs 30 minutes late."},
		{Label: "Marriott 4th St bills the GC, not the property manager", Domain: "customers", Kind: memory.Standing,
			WhyMatters: "Invoicing the property manager gets it rejected and delays payment by a month."},
		{Label: "Marriott 4th St always specifies tempered", Domain: "customers", Kind: memory.Standing,
			WhyMatters: "Quoting annealed there means requoting and losing a day."},
		{Label: "Downtown stops need an extra 45 minutes on weekdays", Domain: "scheduling", Kind: memory.Finding,
			WhyMatters: "Parking, not driving, is what makes back-to-back downtown jobs run late."},
		{Label: "No bookings after 3pm on Tuesdays", Domain: "scheduling", Kind: memory.Standing,
			WhyMatters: "Kid pickup — booking over it means cancelling on a customer."},
		{Label: "Moved insulated units to Northwest Glass", Domain: "suppliers", Kind: memory.Decision,
			WhyMatters: "They are who to call for IG lead times now; Pacific is quotes only."},
		{Label: "Two late units from Pacific cost us the Marriott job", Domain: "suppliers", Kind: memory.Issue,
			WhyMatters: "Explains why nothing time-critical goes to Pacific any more."},
	}

	recorded := make(map[string]string, len(facts))
	for _, f := range facts {
		n, err := stores.Memory.Record(f)
		if err != nil {
			return err
		}
		recorded[f.Label] = n.ID
	}

	// The edge is the point: it is what lets the assistant explain a rule
	// rather than just assert it.
	if _, err := stores.Memory.Link(
		recorded["Moved insulated units to Northwest Glass"],
		recorded["Two late units from Pacific cost us the Marriott job"],
		memory.CausedBy,
		"Pacific missed two IG deliveries in six weeks and we lost the job over it.",
	); err != nil {
		return err
	}

	fmt.Printf("Seeded %s with 3 stops today and %d learned facts.\n", t.Name, len(facts))
	return nil
}

// sessionSecret reads SESSION_SECRET, or generates an ephemeral one for local
// development. An ephemeral secret signs out every user on restart, which is
// fine locally and not acceptable in production — so say so loudly.
func sessionSecret() ([]byte, error) {
	if s := os.Getenv("SESSION_SECRET"); s != "" {
		if len(s) < 32 {
			return nil, errors.New("SESSION_SECRET must be at least 32 characters")
		}
		return []byte(s), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	log.Print("[web] WARNING: SESSION_SECRET is unset — using a random one. Everyone is signed out on every restart.")
	return []byte(hex.EncodeToString(buf)), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
