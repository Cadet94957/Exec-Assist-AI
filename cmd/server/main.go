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

	"github.com/Cadet94957/Exec-Assist-AI/internal/embed"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
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

	site, err := web.New(tenants, secret, os.Getenv("INSECURE_COOKIES") != "1")
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
