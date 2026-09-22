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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	googleconnect "github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/embed"
	"github.com/Cadet94957/Exec-Assist-AI/internal/extract"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/obs"
	"github.com/Cadet94957/Exec-Assist-AI/internal/push"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
	"github.com/Cadet94957/Exec-Assist-AI/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// No arguments means someone double-clicked the binary, which is the most
	// likely way this is ever launched by the person it is built for. That has
	// to do the friendly thing, not start a bare server they cannot sign in to.
	//
	// Production says "serve" explicitly — see the Dockerfile ENTRYPOINT —
	// so nothing real depends on the fall-through this replaces.
	cmd, args := "quickstart", []string{}
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}

	var err error
	switch cmd {
	case "quickstart":
		err = quickstart(args)
	case "serve":
		err = serve(serveOptions{})
	case "create-tenant":
		err = createTenant(args)
	case "seed-demo":
		err = seedDemo(args)
	case "backfill":
		err = backfill(args)
	case "vapid-keys":
		err = vapidKeys()
	case "backup":
		err = backup(args)
	case "restore":
		err = restore(args)
	case "verify-restore":
		err = verifyRestore(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
	if err != nil {
		fatal(fmt.Errorf("%s: %w", cmd, err))
	}
}

func usage() {
	fmt.Println("Exec Assist — an operating assistant for trade contractors.")
	fmt.Println()
	fmt.Println("  (no command)    set up and run it locally, then open your browser")
	fmt.Println("  serve           run the server only, for a real deployment")
	fmt.Println("  create-tenant   add an account")
	fmt.Println("  seed-demo       fill an account with a demo day")
	fmt.Println("  vapid-keys      generate the keys push notifications need")
	fmt.Println("  backup          snapshot every account")
	fmt.Println("  restore         rebuild an account from a snapshot")
	fmt.Println("  verify-restore  prove a backup can actually be restored")
	fmt.Println()
	fmt.Println("Add -h to any command for its options.")
}

func vapidKeys() error {
	keys, err := push.GenerateKeys(envOr("VAPID_SUBJECT", "mailto:support@example.com"))
	if err != nil {
		return err
	}
	fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n\n", keys.Public, keys.Private)
	fmt.Println("Set both as secrets. Never rotate them casually: every existing")
	fmt.Println("subscription becomes invalid and notifications stop silently.")
	return nil
}

// fatal reports an error and exits, pausing first on Windows.
//
// A double-clicked console program on Windows closes its window the instant it
// exits, so without this the whole failure — the reason, the fix, everything —
// flashes past and the app simply appears to do nothing. The terminal check
// matters as much as the pause: piped or redirected, there is nobody to press
// a key, and blocking would hang a CI job instead of failing it.
func fatal(err error) {
	log.Printf("%v", err)
	if runtime.GOOS == "windows" && isTerminal(os.Stdin) {
		fmt.Println()
		fmt.Println("Press Enter to close this window.")
		fmt.Fscanln(os.Stdin)
	}
	os.Exit(1)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// serveOptions describes how serve runs. The zero value is a production
// server; only quickstart sets anything.
//
// These are arguments rather than environment variables on purpose. The
// handoff below is an authentication bypass, and a bypass a stray env var can
// switch on in production is a different and much worse thing than one that
// requires a caller to ask for it.
type serveOptions struct {
	// local means one person running this on their own computer. It listens
	// on loopback only — no firewall prompt on Windows or macOS, and nothing
	// reachable from the café wifi — and moves to a free port if the usual
	// one is taken rather than failing.
	local bool
	// handoffToken and handoffTenant enable the single-use sign-in link, so
	// nobody has to copy a 48-character key out of a terminal.
	handoffToken, handoffTenant string
	// ready is called with the address to open once the port is bound, so a
	// browser is never sent to a server that is not listening yet.
	ready func(baseURL string)
}

// serve runs the web server.
func serve(opts serveOptions) error {
	port := envOr("PORT", "8080")
	addr := ":" + port
	if opts.local {
		addr = "127.0.0.1:" + port
	}
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

	vapid := push.Keys{
		Public:  os.Getenv("VAPID_PUBLIC_KEY"),
		Private: os.Getenv("VAPID_PRIVATE_KEY"),
		Subject: envOr("VAPID_SUBJECT", "mailto:support@example.com"),
	}
	if !vapid.Configured() {
		log.Print("[push] VAPID keys are unset — briefs will be written but not delivered (run: server vapid-keys)")
	}

	site, err := web.New(tenants, secret, os.Getenv("INSECURE_COOKIES") != "1", gcfg, box, brain, vapid)
	if err != nil {
		return err
	}
	if opts.handoffToken != "" {
		site.EnableLocalHandoff(opts.handoffToken, opts.handoffTenant)
	}

	srv := &http.Server{
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

	// A backup nobody schedules is a backup nobody has.
	if dir := os.Getenv("BACKUP_DIR"); dir != "" {
		go backupLoop(ctx, tenants, dir)
	} else {
		log.Print("[backup] BACKUP_DIR is unset — no scheduled backups are running")
	}

	// The morning brief. Runs on a short tick and fires per tenant when their
	// own local clock reaches the hour — a Los Angeles glazier and a New York
	// one both get 6am, not one of them getting 3am.
	if brain != nil {
		go briefLoop(ctx, brain, tenants, push.NewSender(vapid))
	}

	// Learning runs far less often than syncing: patterns emerge over weeks,
	// and every pass costs money.
	if brain != nil {
		go extractLoop(ctx, brain, tenants)
	}

	ln, err := listen(addr, opts.local)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Printf("[web] listening on %s, data in %s", ln.Addr(), dataDir)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if opts.ready != nil {
		// 127.0.0.1 rather than "localhost": the listener is IPv4 loopback,
		// and a browser that resolves localhost to ::1 first would find
		// nothing there.
		_, bound, _ := net.SplitHostPort(ln.Addr().String())
		opts.ready("http://127.0.0.1:" + bound)
	}

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

// listen binds addr. For a local run, a port already in use — 8080 is a
// popular one — moves to any free port instead of failing, since the address
// is opened for the person rather than typed by them. A deployment keeps the
// port it was given, because a proxy is pointed at it.
func listen(addr string, local bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil || !local {
		return ln, err
	}
	fallback, ferr := net.Listen("tcp", "127.0.0.1:0")
	if ferr != nil {
		return nil, err // the original error explains more
	}
	log.Printf("[web] %s is in use, so using %s instead", addr, fallback.Addr())
	return fallback, nil
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
		res, err := googleconnect.Sync(callCtx, cfg, stores.Ledger(), box, time.Now())
		cancel()

		switch {
		case errors.Is(err, ledger.ErrNoConnection):
			// Most tenants simply have not connected a calendar.
		case err != nil:
			stores.Metrics().Count(obs.SyncFailed)
			log.Printf("[google] sync %s: %v", t.ID, err)
		default:
			stores.Metrics().Count(obs.SyncSucceeded)
			if n := res.Created + res.Changed + res.Removed; n > 0 {
				stores.Metrics().Value(obs.EventsIngested, float64(n))
				log.Printf("[google] sync %s: %d new, %d changed, %d removed",
					t.ID, res.Created, res.Changed, res.Removed)
			}
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

// backup snapshots every tenant, or one named tenant.
func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	id := fs.String("id", "", "tenant to back up; omit for all")
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "data directory")
	to := fs.String("to", envOr("BACKUP_DIR", "./backups"), "where to write snapshots")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tenants, err := tenant.NewManager(*dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	if *id != "" {
		snap, err := tenants.Backup(*id, *to)
		if err != nil {
			return err
		}
		fmt.Printf("%s -> %s (%d bytes)\n", snap.TenantID, snap.Dir, snap.Bytes)
		return nil
	}

	snaps, err := tenants.BackupAll(*to)
	for _, s := range snaps {
		fmt.Printf("%s -> %s (%d bytes)\n", s.TenantID, s.Dir, s.Bytes)
	}
	return err
}

// restore rebuilds a tenant from a snapshot into a clean data directory.
func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	id := fs.String("id", "", "tenant id (required)")
	from := fs.String("from", "", "snapshot directory (required)")
	into := fs.String("into", "", "clean data directory to restore into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *from == "" || *into == "" {
		fs.Usage()
		return errors.New("-id, -from and -into are all required")
	}

	if err := tenant.Restore(*from, *into, *id); err != nil {
		return err
	}
	fmt.Printf("Restored %s into %s\n", *id, filepath.Join(*into, "tenants", *id))
	fmt.Println("Run verify-restore against it before trusting it.")
	return nil
}

// verifyRestore is the operational test §24 requires: back up, restore into a
// clean directory, open both stores, and read from each. A backup that has
// never been restored is a file with a reassuring name.
func verifyRestore(args []string) error {
	fs := flag.NewFlagSet("verify-restore", flag.ExitOnError)
	id := fs.String("id", "", "tenant to verify (required)")
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		fs.Usage()
		return errors.New("-id is required")
	}

	work, err := os.MkdirTemp("", "restore-check-*")
	if err != nil {
		return fmt.Errorf("create scratch directory: %w", err)
	}
	defer os.RemoveAll(work)

	tenants, err := tenant.NewManager(*dataDir)
	if err != nil {
		return err
	}
	defer tenants.Close()

	fmt.Printf("Backing up %s...\n", *id)
	snap, err := tenants.Backup(*id, filepath.Join(work, "backups"))
	if err != nil {
		return err
	}

	clean := filepath.Join(work, "clean")
	fmt.Println("Restoring into a clean directory...")
	if err := tenant.Restore(snap.Dir, clean, *id); err != nil {
		return err
	}

	dir := filepath.Join(clean, "tenants", *id)
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		return fmt.Errorf("open restored ledger: %w", err)
	}
	defer led.Close()
	mem, err := memory.Open(filepath.Join(dir, "memory.db"), led.DB())
	if err != nil {
		return fmt.Errorf("open restored memory: %w", err)
	}
	defer mem.Close()

	stats, err := led.Stats()
	if err != nil {
		return fmt.Errorf("read restored ledger: %w", err)
	}
	live, archived, err := mem.Counts()
	if err != nil {
		return fmt.Errorf("query restored memory: %w", err)
	}

	fmt.Printf("\nRestored tenant checks out:\n")
	fmt.Printf("  ledger: %d contacts, %d jobs, %d appointments\n",
		stats.Contacts, stats.Jobs, stats.Appointments)
	fmt.Printf("  memory: %d live facts, %d archived\n", live, archived)
	fmt.Println("\nBackup and restore verified.")
	return nil
}

// extractInterval is how often the assistant re-reads a tenant's recent
// history looking for rules. Daily is plenty — a weekday pattern does not
// appear between lunch and dinner — and it keeps per-tenant spend predictable.
const extractInterval = 24 * time.Hour

// briefHour is when the brief goes out, in each tenant's own timezone.
const briefHour = 6

// briefLoop writes and delivers each tenant's brief at 6am local.
//
// It ticks every fifteen minutes and asks whether a tenant is due, rather than
// scheduling a timer per tenant: timers drift across DST and are awkward to
// reason about after a restart, and a brief is not worth that complexity.
func briefLoop(ctx context.Context, brain *llm.Client, tenants *tenant.Manager, sender *push.Sender) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			briefRound(ctx, brain, tenants, sender)
		}
	}
}

func briefRound(ctx context.Context, brain *llm.Client, tenants *tenant.Manager, sender *push.Sender) {
	all, err := tenants.List()
	if err != nil {
		log.Printf("[brief] list tenants: %v", err)
		return
	}

	now := time.Now()
	for _, t := range all {
		if ctx.Err() != nil {
			return
		}
		local := now.In(t.Location())
		if local.Hour() != briefHour {
			continue
		}

		stores, err := tenants.Stores(t.ID)
		if err != nil {
			log.Printf("[brief] open %s: %v", t.ID, err)
			continue
		}

		// One brief per day. The tick is finer than the hour, so without this
		// a tenant would get four notifications between 6:00 and 6:59.
		day := t.StartOfDay(now)
		if _, exists, err := stores.Ledger().GetBrief(day); err == nil && exists {
			continue
		}

		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		out, err := web.WriteBrief(callCtx, brain, stores, now)
		if err != nil {
			cancel()
			log.Printf("[brief] write %s: %v", t.ID, err)
			continue
		}

		delivered, err := web.Deliver(callCtx, sender, stores, out, day)
		cancel()
		if err != nil {
			log.Printf("[brief] deliver %s: %v", t.ID, err)
			continue
		}
		log.Printf("[brief] %s: written, delivered to %d device(s)", t.ID, delivered)
	}
}

// backupLoop snapshots every tenant nightly.
func backupLoop(ctx context.Context, tenants *tenant.Manager, dest string) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snaps, err := tenants.BackupAll(dest)
			if err != nil {
				log.Printf("[backup] %v", err)
			}
			log.Printf("[backup] snapshotted %d tenants to %s", len(snaps), dest)
		}
	}
}

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

	started := time.Now()
	res, err := extract.Run(callCtx, brain,
		extract.Stores{Ledger: stores.Ledger(), Memory: stores.Memory()},
		t.Location(), time.Now())

	// Every model call is recorded, successful or not — an extraction that
	// keeps failing still costs money and still needs to be visible.
	if res.Usage.Input > 0 || res.Usage.CacheRead > 0 || err != nil {
		stores.Metrics().RecordAI(obs.AICall{
			Model:      brain.Model(),
			Operation:  "extract",
			Input:      res.Usage.Input,
			Output:     res.Usage.Output,
			CacheRead:  res.Usage.CacheRead,
			CacheWrite: res.Usage.CacheWrite,
			Latency:    time.Since(started),
			Failed:     err != nil,
		})
	}
	if res.Recorded > 0 {
		stores.Metrics().Value(obs.MemoryProposed, float64(res.Recorded))
	}
	return res, err
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

// quickstart is the one command a person who downloaded a binary should need.
//
// The normal path — create-tenant, then seed-demo, then run, each with flags —
// is three commands and a key you have to copy out of the first one's output
// before it scrolls away. That is fine for an engineer and a wall for everyone
// else. This does the same work, in order, and prints the URL and the key
// together at the end where they are actually useful.
//
// It is deliberately safe to run twice: an existing account is reused and not
// re-seeded, so nobody ends up with six copies of the demo day.
func quickstart(args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ExitOnError)
	// Three characters is the floor the tenant id validator enforces, so a
	// friendlier-looking "me" fails before anything else happens.
	id := fs.String("id", "my-business", "account id")
	name := fs.String("name", "My Business", "business name")
	tz := fs.String("tz", "", "IANA timezone, e.g. America/Los_Angeles (default: this computer's)")
	dataDir := fs.String("data", envOr("DATA_DIR", localDataDir()), "where to keep your data")
	if err := fs.Parse(args); err != nil {
		return err
	}

	zone, source := localZone(*tz)

	// serve() reads DATA_DIR from the environment, so -data has to reach it
	// that way or the account gets created in one directory and served from
	// another — which presents as a valid key being rejected at sign-in.
	os.Setenv("DATA_DIR", *dataDir)

	acct, key, created, err := ensureTenant(*dataDir, *id, *name, zone)
	if err != nil {
		return err
	}

	// Sign-in sets a Secure cookie by default, which a browser silently
	// discards over plain HTTP — the login form just redisplays itself with no
	// error. quickstart is local HTTP by definition, so opt out here rather
	// than making someone diagnose that.
	if os.Getenv("INSECURE_COOKIES") == "" {
		os.Setenv("INSECURE_COOKIES", "1")
	}
	// Without this, sessions are invalidated on every restart.
	if os.Getenv("SESSION_SECRET") == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return fmt.Errorf("generate session secret: %w", err)
		}
		os.Setenv("SESSION_SECRET", hex.EncodeToString(secret))
	}

	// A single-use ticket that signs this browser in, so nobody has to
	// retype a 48-character key. See web.Server.EnableLocalHandoff.
	ticket := make([]byte, 32)
	if _, err := rand.Read(ticket); err != nil {
		return fmt.Errorf("generate sign-in link: %w", err)
	}
	token := hex.EncodeToString(ticket)

	// Everything below waits for the port to be bound, so the link printed
	// and opened carries the port actually in use — which is not 8080 if
	// something else already had it — and the browser never races a server
	// that is not listening yet.
	ready := func(base string) {
		openAt := base + "/start?t=" + token

		fmt.Println()
		fmt.Println("  Your assistant is ready.")
		fmt.Println()
		fmt.Println("  Opening your browser. If it does not appear, go to:")
		fmt.Printf("    %s\n", openAt)
		fmt.Println()
		if created {
			// Printed to be kept, not to be typed — the link above signs
			// this browser in. The key is needed on another device, or after
			// the link expires, and it cannot be shown again.
			fmt.Println("  Keep these somewhere safe. They are how you sign in from")
			fmt.Println("  anywhere else, and the key is not recoverable:")
			fmt.Println()
			fmt.Printf("    Account:  %s\n", *id)
			fmt.Printf("    Key:      %s\n", key)
		} else {
			fmt.Printf("  Account: %s — with the key you saved the first time.\n", *id)
		}
		fmt.Println()
		fmt.Printf("  Your data:  %s\n", *dataDir)
		fmt.Println("              (to start over, delete that folder and run this again)")
		printZone(acct.Timezone, zone, source, created, *dataDir)
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			fmt.Println("  Brief:      off. Set ANTHROPIC_API_KEY to turn on the morning brief.")
		}
		fmt.Println()
		fmt.Println("  Leave this window open while you use it. Ctrl+C stops it.")
		fmt.Println()

		openBrowser(openAt)
	}

	return serve(serveOptions{
		local:         true,
		handoffToken:  token,
		handoffTenant: *id,
		ready:         ready,
	})
}

// localDataDir is where a local run keeps its data unless told otherwise:
// ~/Library/Application Support/ExecAssist on a Mac, %AppData%\ExecAssist on
// Windows.
//
// Not the current directory. Where that is depends on how the app was
// launched: the Mac instructions run it from the home folder, and Windows runs
// an exe opened from inside a zip in a temporary folder that is emptied
// afterwards — so every launch would find no account, create a new one, and
// hand out a new key. A fixed per-user folder is the same wherever the app is
// started from, and it survives someone tidying their Downloads.
func localDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "ExecAssist")
	}
	return "./data"
}

// openBrowser asks the operating system to open a URL, and gives up quietly.
//
// Every failure here is survivable, because the URL is printed above either
// way. A headless machine, a locked-down desktop, an unusual Linux with no
// xdg-open — none of those are reasons to fail to start a server.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// Via rundll32 rather than `cmd /c start`, which treats the first
		// quoted argument as a window title and mangles URLs containing &.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// printZone tells the person which timezone their account uses and how much
// to trust it. Everything about "today" and the time of the morning brief
// depends on it, and a wrong zone produces no error — only a day that is
// quietly hours out.
func printZone(stored, wanted string, source zoneSource, created bool, dataDir string) {
	switch {
	case !created && stored != wanted && source != zoneFallback:
		// An account made earlier keeps the zone it was made with. Say so
		// rather than print a zone it is not using.
		fmt.Printf("  Timezone:   %s — this account was set up with it, but this computer\n", stored)
		fmt.Printf("              says %s. To switch, delete the %q folder and run this again.\n", wanted, dataDir)
	case source == zoneFallback:
		fmt.Printf("  Timezone:   %s — COULD NOT TELL this computer's timezone.\n", stored)
		fmt.Println("              \"Today\" and the morning brief will be hours out. Run this")
		fmt.Println("              again with your zone, for example:  -tz America/Chicago")
	case source == zoneDetected:
		fmt.Printf("  Timezone:   %s — from this computer; set it with -tz if wrong\n", stored)
	default:
		fmt.Printf("  Timezone:   %s\n", stored)
	}
}

// zoneSource records where a timezone came from, so the person running this
// can be told how far to trust it.
type zoneSource int

const (
	zoneExplicit zoneSource = iota // they passed -tz
	zoneDetected                   // read from this computer
	zoneFallback                   // could not tell; UTC
)

// localZone reports the timezone to use and where it came from.
//
// Every "what does today look like" calculation keys off this, so a wrong zone
// does not fail loudly — it quietly shows the wrong day and sends the brief at
// the wrong hour. Hence the caller says out loud which one it picked, and says
// it loudest when it could not tell.
func localZone(explicit string) (string, zoneSource) {
	if explicit != "" {
		return explicit, zoneExplicit
	}
	if name, ok := detectZone(); ok {
		return name, zoneDetected
	}
	return "UTC", zoneFallback
}

// ensureTenant creates and seeds the account on first run, and does nothing on
// every run after that. It returns the account as stored, and the access key
// only when it created one.
//
// Returning the stored account matters on later runs: its timezone was fixed
// when it was created, and printing a freshly detected zone instead would
// describe a setting the account is not actually using.
func ensureTenant(dataDir, id, name, zone string) (t tenant.Tenant, key string, created bool, err error) {
	tenants, err := tenant.NewManager(dataDir)
	if err != nil {
		return t, "", false, err
	}
	defer tenants.Close()

	if existing, err := tenants.Get(id); err == nil {
		return existing, "", false, nil
	}

	t, key, err = tenants.Create(id, name, "glazier", zone, "")
	if err != nil {
		return t, "", false, fmt.Errorf("create your account: %w", err)
	}
	stores, err := tenants.Stores(t.ID)
	if err != nil {
		return t, "", false, err
	}
	if err := seedTenant(t, stores); err != nil {
		return t, "", false, fmt.Errorf("seed the demo day: %w", err)
	}
	return t, key, true, nil
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
	return seedTenant(t, stores)
}

// seedTenant writes the demo day and the learned facts behind it.
//
// Split out of seedDemo so quickstart can seed without going through flag
// parsing. One copy of the demo data, not two — the moment there are two, they
// drift, and the screenshots stop matching what a new person actually sees.
func seedTenant(t tenant.Tenant, stores *tenant.Stores) error {
	// A day's stops, in deliberately scrambled order — the ledger sorts them.
	day := t.StartOfDay(time.Now())
	for _, a := range []struct {
		id, title, where string
		hour             int
	}{
		// "Downtown" is load-bearing, not scenery: the standing rule below is
		// matched to a stop by the words in it, so without this the 45-minute
		// parking buffer never attaches and the demo quietly fails to show the
		// one thing this product is for.
		{"demo-2", "Marriott 4th St — lobby panel measure", "1200 4th St, Downtown", 11},
		{"demo-1", "Harborview — storefront reglaze", "88 Harbor Way", 8},
		{"demo-3", "Ferguson residence — shower door fit", "41 Cedar Ln", 14},
	} {
		if err := stores.Ledger().UpsertAppointment(ledger.Appointment{
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
	for i, f := range facts {
		// Demo data still has to satisfy the real guard — provenance is not
		// optional just because a fact was seeded.
		f.Status = memory.Observed
		f.Provenance = memory.Provenance{
			SourceType: "contractor",
			ObservedAt: time.Now().UTC(),
			Note:       "Seeded demo data.",
			Sources:    []memory.SourceRef{{Kind: "appointment", RefID: fmt.Sprintf("demo-%d", i+1)}},
		}
		n, err := stores.Memory().Record(f)
		if err != nil {
			return err
		}
		recorded[f.Label] = n.ID
	}

	// The edge is the point: it is what lets the assistant explain a rule
	// rather than just assert it.
	if _, err := stores.Memory().Link(
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
