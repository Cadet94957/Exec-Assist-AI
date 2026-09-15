# Exec Assist

An AI executive assistant for individual trade contractors — glaziers first.

It is **not** an AI receptionist. Contractors already have one of those. This sits
*above* the apps they already pay for — the receptionist that books the job, the
calendar, the invoicing tool, the email — and is the only thing that holds the whole
picture, and remembers *why*.

Nothing else in a contractor's stack knows that the Marriott on 4th bills the GC rather
than the property manager, always wants tempered, and locks the dock at 3pm. This does,
and it gets better the longer it works for you.

## How it is built

Two stores per contractor, and keeping them separate is the whole design:

| | What goes in it | Why |
|---|---|---|
| **Ledger** (`ledger.db`) | Jobs, appointments, contacts, invoices — a mirror of their other apps | Disposable. Always re-syncable. |
| **Memory** (`memory.db`) | Rules, lessons, preferences — each with a reason it matters | This is the product. |

The memory graph is [corbym/memoryweb](https://github.com/corbym/memoryweb) (MIT), linked
as a Go library rather than run as a separate MCP server.

memoryweb is a **decision log, not an event log** — `why_matters` is mandatory by design.
Dumping appointments into it as nodes would poison it: embedding noise, meaningless
recall, a graph that slowly becomes useless. So `memory.Record` refuses any write without
a reason it should change future behaviour, and that rule lives in exactly one place
(`internal/memory/memory.go`). If you only protect one invariant in this codebase, protect
that one.

Each contractor gets their own pair of SQLite files. Isolation is structural, not a
`WHERE tenant_id = ?` that someone can forget — and "export everything you know about my
business" is a file copy.

## Running it locally

You need Go 1.24.13 or newer and a C compiler (`gcc`). Both sqlite-vec and go-sqlite3 are
cgo, so `CGO_ENABLED=0` will not build.

```sh
make check                                        # format, vet, tests
make tenant ID=dave-glass NAME="Dave's Glass" TZ=America/Los_Angeles
./bin/server seed-demo -id dave-glass             # a plausible day, for looking at
make run                                          # http://localhost:8080
```

`create-tenant` prints an access key once and stores only its hash. Save it.

`seed-demo` is a demo aid, not part of the product — point it at a throwaway tenant.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | |
| `DATA_DIR` | `./data` | Per-tenant SQLite lives here. **Mount a volume at this path in production or every deploy destroys all customer data.** |
| `SESSION_SECRET` | random | Must be 32+ chars. Unset means everyone is signed out on every restart. |
| `VOYAGE_API_KEY` | unset | Turns on semantic recall. Unset is fine — memoryweb falls back to literal matching. |
| `EMBED_MODEL` | `voyage-3.5-lite` | Must return 1024 dimensions; memoryweb's schema stores nothing else. |
| `INSECURE_COOKIES` | unset | Set to `1` for local HTTP only. Never in production. |
| `TOKEN_KEY` | unset | 64 hex chars (`openssl rand -hex 32`). Encrypts stored Google tokens. Without it calendar connection is disabled rather than storing credentials in the clear. |
| `GOOGLE_CLIENT_ID` | unset | From Google Cloud Console — see below. |
| `GOOGLE_CLIENT_SECRET` | unset | " |
| `GOOGLE_REDIRECT_URL` | unset | `https://yourdomain/connect/google/callback`, matching the console exactly. |
| `ANTHROPIC_API_KEY` | unset | Turns on learning. Without it, synced data is mirrored but nothing is learned from it. |
| `ANTHROPIC_MODEL` | `claude-opus-5` | |
| `ANTHROPIC_BASE_URL` | unset | Point at a gateway, proxy, or stub. Leave unset for Anthropic. |
| `BACKUP_DIR` | unset | Where nightly tenant snapshots are written. Unset means no scheduled backups. |

### About embeddings

memoryweb expects an Ollama-shaped `/api/embed`. Rather than run Ollama, the server hosts
that shape itself on a loopback listener and translates to a hosted provider
(`internal/embed`). Nothing to install, nothing to supervise.

Semantic search is an upgrade, not a launch requirement. Running with no key is a
legitimate way to operate until there are enough customers to justify the spend.

## Connecting Google Calendar

This is the one step nobody can do for you — it needs your Google account.

1. **Create a project** at [console.cloud.google.com](https://console.cloud.google.com).
2. **Enable the Google Calendar API** (APIs & Services → Library → search "Google
   Calendar API" → Enable).
3. **Configure the OAuth consent screen.** Choose *External*. Add the scopes
   `.../auth/calendar.events` and `.../auth/userinfo.email`. Add yourself under *Test
   users*.
4. **Create credentials**: APIs & Services → Credentials → Create Credentials → OAuth
   client ID → *Web application*. Under *Authorized redirect URIs* add exactly the value
   you will set as `GOOGLE_REDIRECT_URL` — a trailing-slash mismatch is rejected.
5. Copy the client ID and secret into the environment variables above.

### Two things that will bite you

**While the app is in "Testing", refresh tokens expire after seven days.** The connection
works, then breaks a week later for no visible reason. That is Google's behaviour for
unpublished apps, not a bug here. Publish the app when you start piloting with real
contractors.

**Publishing needs verification because `calendar.events` is a "sensitive" scope.** That
means a review of your consent screen and a privacy policy — annoying, but weeks not
months. It is *not* the CASA Tier 2 security assessment; that is only for "restricted"
scopes, which is exactly why this product does not touch Gmail. Until you are verified,
an unverified app shows a warning screen and is capped at 100 users, which is plenty for
a pilot.

### How the sync behaves

- A window of 30 days back and 60 days forward, re-fetched every 10 minutes.
- Recurring events are expanded into individual instances (`singleEvents=true`), so a
  weekly job appears on each of its dates.
- The Google event id is the appointment's external id, so re-syncing updates in place.
- **Deletions are handled by reconciling**, not by tombstones: anything still in the
  window that Google no longer returns gets removed. Google only reports cancellations on
  incremental syncs, so this is the reliable approach.
- Manually added appointments are never touched by the sync.
- Failures are recorded on the connection and shown on the dashboard. A calendar that
  silently stopped updating is worse than one that says it is stuck.

## Where things live

Two places, deliberately.

**Today** (`/`) — the only screen anyone opens daily. Stops in order, counters, and the
ask-anything box. Nothing else.

**Settings** (`/settings`) — calendar connection, **Memory**, and account.

The memory sits in Settings rather than the main navigation, the way Claude's own memory
does. A contractor does not want to manage a knowledge graph; they want their day handled.
It stays visible and editable because that is how a wrong inference gets corrected and how
you answer "what does this thing actually know about me" — but it is not a destination.
When something is waiting to be reviewed, Today shows one quiet line pointing at it.

`/learned`, the old URL, redirects to `/settings#memory`.

## It is a desktop app and a website

The same build is both. Visit the URL like any site, or install it — Chrome and Edge offer
an Install button, and it then gets its own window, its own icon in the dock or Start
menu, and launches with no address bar.

- **Zero external resources.** No CDN, no analytics, no fonts fetched from anywhere. The
  whole app is served by your one binary, which is verified in a browser test.
- **Accessible**, and checked rather than assumed: an automated axe-core pass over every
  page in both light and dark reports zero violations at any severity, plus a keyboard
  walkthrough covering the skip link, tab order, and the live region that announces search
  results. Two real failures came out of that pass and were fixed — unlabelled login
  fields, and two colour pairs measured at 4.44:1 and 2.65:1 against a 4.5 minimum.

If you later want a signed `.dmg`/`.exe` in the Applications folder, that is a Tauri
wrapper around exactly this — about a day of work, but it brings an Apple Developer
account, notarization, a Windows signing certificate, and an auto-update channel you would
own. Worth doing when a customer asks; not before.

## How learning works

This is the step that makes it more than a calendar mirror.

Once a day (or on demand via `backfill`), the assistant re-reads the last 90 days of the
ledger and asks Claude what *general rules* the data supports — not what happened, but
what should change a decision next time. Anything it finds is filed in the memory graph
**tagged unconfirmed**, and shown under Settings → Memory for a yes/no.

```sh
./bin/server backfill -id dave-glass     # one pass now, for one tenant
./bin/server backfill                    # every tenant
```

### Why the prompt is mostly about what *not* to record

A model asked to "find insights" will always find some. That is the failure mode that
turns this from useful into noise, and it is not something the `why_matters` guard can
catch — the guard rejects an *empty* reason, not a worthless one.

So `internal/extract/prompt.go` spends most of its length on exclusions, and explicitly
blesses the empty answer: *"An empty list is the correct answer far more often than not,
and is never a failure."* It also requires an **evidence** field naming what in the data
supports each claim; anything without one is dropped before it reaches the graph. That is
the check that keeps out plausible inventions.

Three more things hold the line:

- Nothing is relied on until a person confirms it. An inference is not knowledge.
- Domains are a fixed set of six. memoryweb degrades when domains multiply, because recall
  gets diluted across near-identical buckets.
- A run proposing more than ten facts is treated as a malfunction and truncated.

### A known limitation

Duplicate detection matches on the *normalised label*, so "Marriott bills the GC" and
"Marriott 4th St bills the general contractor" are not recognised as the same rule. The
main defence is that every run is given the facts already known and told not to restate
them — but a determined rephrasing can still slip through. Review is where you catch it,
and discarding is one click and reversible.

## Backups

A successful backup is not evidence the system is safe, so the restore is tested rather
than assumed.

```sh
./bin/server backup                       # every tenant
./bin/server verify-restore -id dave-glass  # back up, restore to a clean dir, read both stores
```

Set `BACKUP_DIR` and the server snapshots every tenant nightly. Leave it unset and it says
so at startup rather than pretending.

`verify-restore` is the operational test the spec requires: a backup nobody has ever
restored is a file with a reassuring name. Run it before you have customers, and on a
schedule once you do.

## What it costs to run

Every model call is recorded per tenant with its tokens, latency, outcome and an estimated
cost. Cached reads are priced separately from fresh input — counting them together would
overstate the bill by roughly what prompt caching saves, hiding the saving the margin
depends on. Watch the cache hit rate: if it collapses, something is silently invalidating
the prefix, and that shows up as a bill before it shows up as a bug.

## Deploying

```sh
fly volumes create exec_assist_data --size 3
fly secrets set SESSION_SECRET="$(openssl rand -hex 32)"
fly secrets set TOKEN_KEY="$(openssl rand -hex 32)"
fly secrets set GOOGLE_CLIENT_ID=... GOOGLE_CLIENT_SECRET=... \
                GOOGLE_REDIRECT_URL=https://yourdomain/connect/google/callback
fly secrets set ANTHROPIC_API_KEY=sk-ant-...
make deploy
```

**Never rotate `TOKEN_KEY` casually.** It decrypts stored Google tokens; changing it makes
every existing calendar connection unreadable and every contractor has to reconnect.

One machine, one volume. No database server, no queue, no cache.

**Keep it at one machine.** SQLite is a single file on a single volume; two machines
writing to it will corrupt data. `auto_stop_machines` is off in `fly.toml` for the same
reason — a stopped machine is a missed 6am brief.

## Where this is up to

Phases 0, 1 and 2 are done. What works today: tenants, both stores, the `why_matters`
guard, sign-in, the Today screen, Settings with an editable memory, recall by text, Google
Calendar connection with background sync, the extraction pass with its review queue, and
the installable desktop app.

Not built yet: magic-link sign-in, the inbound email address and universal webhook, the
morning and evening briefs over SMS, and billing.

Two things are unverified here because they need live credentials: the Google OAuth token
exchange, and a real Claude call. Everything either side of them is covered by tests that
run offline — the extraction suite drives the real SDK encoding against a stub API, so
what is untested is the network hop, not the logic.

Two things that are cheap now and expensive later, so do them early:

- **Start Twilio A2P 10DLC registration before you need it.** US SMS requires it and it
  takes 1–3 weeks.
- **Do not add Gmail OAuth.** Gmail read scopes are Google *restricted* scopes and require
  a CASA Tier 2 security assessment — thousands of dollars and months. A per-tenant
  forwarding address gets ~90% of the value with none of that. Calendar scopes are only
  *sensitive*, which is a far lighter process.

## Layout

```
cmd/server/          the single binary — serve, create-tenant, seed-demo
internal/memory/     the ONLY way into the graph; Record() enforces why_matters
internal/ledger/     operational records; own schema, no judgement
internal/tenant/     registry, per-tenant store handles, local-day maths
internal/embed/      Ollama-shaped endpoint backed by a hosted provider
internal/web/        HTMX handlers and templates; no build step
```
