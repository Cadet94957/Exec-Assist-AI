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

## Trying it out (no toolchain needed)

Download a build and run one command. Nothing to install, no Go, no compiler.

1. Go to the repo's **Actions** tab → **Build downloads** → **Run workflow**.
2. When it finishes, the binaries are at the bottom of the run page under *Artifacts*.
   Download the one for your computer:

   | You have | Download |
   |---|---|
   | Mac, M1/M2/M3/M4 | `exec-assist-macos-apple-silicon` |
   | Mac, Intel | `exec-assist-macos-intel` |
   | Windows | `exec-assist-windows-amd64` |
   | Linux | `exec-assist-linux-amd64` |

3. Unzip it, then:

```sh
chmod +x ./exec-assist-*                       # macOS and Linux only
./exec-assist-macos-apple-silicon quickstart
```

It prints an address, an account name and a key. Open the address, sign in, and you are
looking at a demo glazier's day: three stops in order, the Harborview gate code attached
to the right one, and the 45-minute downtown parking buffer applied to the Marriott
measure. That last one is the product in miniature — a standing rule, matched to the stop
it affects, without anyone asking.

Run it again later and it picks up the same account rather than starting over. To wipe it,
delete the `data` folder it created next to the binary.

Set your timezone if the guess is wrong — everything about "today" depends on it:

```sh
./exec-assist-... quickstart -tz America/Los_Angeles
```

### The morning brief needs a key

Everything above works with no accounts and no API keys. The brief does not — it is the
one feature that calls Claude, and without a key the button says so plainly:

```sh
ANTHROPIC_API_KEY=sk-ant-... ./exec-assist-... quickstart
```

A brief costs a fraction of a cent. Set a spend limit on the key first anyway — see
[Cap the spend](#cap-the-spend).

### The two warnings you will see

These builds are unsigned. Signing costs money and buys nothing while you are the only
person installing them, so both operating systems will object once, and both are one step.

**macOS** quarantines anything downloaded from the internet, and will refuse to run the
binary — usually with a dialog about an unidentified developer. Clear it:

```sh
xattr -dr com.apple.quarantine ./exec-assist-macos-apple-silicon
```

**Windows** SmartScreen warns about unsigned executables. Running it from PowerShell or
Terminal generally avoids the prompt; if you double-click it and Windows blocks it, choose
*More info* → *Run anyway*.

Neither is a sign anything is wrong. They are what unsigned means. If you ever hand this
to someone you are not on the phone with, that is the point to buy a certificate — see
[Where this is up to](#where-this-is-up-to).

### Phone notifications need a deploy

The 6am brief cannot reach your phone from a laptop, because your phone needs a real HTTPS
address to subscribe to. Local is for looking at the product; [Deploying](#deploying) is
for living with it.

## Running it locally

You need Go 1.24.13 or newer and a C compiler (`gcc`). Both sqlite-vec and go-sqlite3 are
cgo, so `CGO_ENABLED=0` will not build.

```sh
make check                                        # format, vet, tests
make build && ./bin/server quickstart             # one command, as above
```

Or the long way, when you want control over the account:

```sh
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
| `VAPID_PUBLIC_KEY` | unset | From `server vapid-keys`. Unset means briefs are written but not delivered. |
| `VAPID_PRIVATE_KEY` | unset | " |
| `VAPID_SUBJECT` | `mailto:support@example.com` | Contact URL push services use to reach you. |

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
   `.../auth/calendar.events.readonly` and `.../auth/userinfo.email`. Add yourself under
   *Test users*. Note the `.readonly` — see [This product reads](#this-product-reads)
   below.
4. **Create credentials**: APIs & Services → Credentials → Create Credentials → OAuth
   client ID → *Web application*. Under *Authorized redirect URIs* add exactly the value
   you will set as `GOOGLE_REDIRECT_URL` — a trailing-slash mismatch is rejected.
5. Copy the client ID and secret into the environment variables above.

### Two things that will bite you

**While the app is in "Testing", refresh tokens expire after seven days.** The connection
works, then breaks a week later for no visible reason. That is Google's behaviour for
unpublished apps, not a bug here. Publish the app when you start piloting with real
contractors.

**Publishing needs verification because `calendar.events.readonly` is a "sensitive" scope.** That
means a review of your consent screen and a privacy policy — annoying, but weeks not
months. It is *not* the CASA Tier 2 security assessment; that is only for "restricted"
scopes, which is exactly why this product does not touch Gmail. Until you are verified,
an unverified app shows a warning screen and is capped at 100 users, which is plenty for
a pilot.

### This product reads

The Google scope is `calendar.events.readonly`, and that is a deliberate constraint rather
than a phase we have not got to yet.

This assistant exists to tell a contractor about their day. It has no reason to ever
change their calendar, so it does not ask for permission to. That means a stolen token, a
bug, or a mis-issued credential cannot delete a glazier's week — not because the code is
careful, but because there is no code that could. `internal/connect/google` makes exactly
one HTTP request and it is a `GET`, and `TestClientIsReadOnly` fails the build if that
ever stops being true.

It also changes the sentence a contractor reads before trusting you. With this scope,
Google's consent screen says *"See the events on all your calendars."* With the read-write
scope it says *"See, edit, share and permanently delete all the calendars you can
access"* — which is a hard thing to follow with "trust us."

**If you connected a calendar before this change, disconnect and reconnect it.** Existing
tokens keep the old, wider permission until they are re-issued. Settings → Disconnect,
then connect again, and check the consent screen wording while you are there.

The same rule holds going forward: Gmail, when it arrives, is `gmail.readonly`. Any
future need to write is a separate decision with its own scope, its own consent screen and
its own review — never a permission requested early "just in case."

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

## The morning brief

Assembled deterministically, then written by Claude.

`brief.Assemble` reads the ledger and the memory graph and builds a typed day in pure Go —
stops in order, gaps measured, overlaps detected, buffers pulled from standing rules,
outstanding money, and what moved since yesterday. `brief.Render` hands that to Claude for
synthesis and wording only.

The split decides what can go wrong. A model that writes badly produces an awkward brief.
A model asked to work out whether a day is feasible, or which invoice is overdue, produces
a confident wrong answer — and the whole product is a promise to catch exactly that.

Every fact reaches the prompt marked `confirmed`, `observed` or `inferred`, and the prompt
must hedge anything unconfirmed: *"you usually allow about 45 minutes"*, never *"you need
45 minutes"*. A test asserts the marking survives into the request, because this is the
rule that either holds there or is lost silently.

### Measuring the prompt

```sh
make eval      # calls the real API; costs a few cents
```

The brief prompt is the product, and it is what will get tuned once pilot feedback
arrives. Without a measurement that tuning is guesswork that can silently break the two
rules everything rests on: never invent, and never state a guess as fact.

Six fixture days — quiet, ordinary, one carrying an inferred fact, one with money owed,
one with a cancellation, and one with an injection attempt buried in a calendar title —
each checked programmatically:

- **No invented numbers.** Every number in the brief must appear in the data. Times,
  amounts and gate codes are exactly what a contractor acts on.
- **No invented names.** A customer or site the data never mentioned is a fabrication.
- **Hedging.** A brief carrying an inferred fact must hedge it. "You usually allow about
  45 minutes" passes; "you need 45 minutes" fails.
- **Brevity** on a quiet day, and a headline that fits a lock screen.
- **Coverage** — the tight gap, the overdue invoice, the cancellation each have to
  actually make it in.

It prints per-case cost, so the trade-off between prompt quality and spend is visible
rather than guessed at. The checks are programmatic rather than judged by another model:
"did it invent a number" has an exact answer, and an exact answer beats a plausible
opinion for the rule whose violation is the failure the product exists to prevent.

`make check` stays free and offline — the eval is behind a build tag.

### Notifications

```sh
./bin/server vapid-keys      # once; set both as secrets
```

Briefs go out at 6am in each tenant's own timezone. **Never rotate the VAPID keys
casually** — every existing subscription becomes invalid and notifications stop silently.

Push only reaches people who installed the app, and on iOS only when installed to the home
screen. That is why every send is recorded: if delivery numbers come back low during the
pilot, the pilot is measuring installation rather than the brief, and the fix is another
channel — not the conclusion that nobody wanted it. SMS arrives in Phase 2 as a second
`Deliverer`, not a rewrite.

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

Do these in order. Step 1 first because it is the step that has never run here —
this environment has no container daemon, so the image build is the one piece of the
deploy that is genuinely unverified.

```sh
# 1. Prove the image builds. cgo + sqlite-vec is where this breaks if it breaks.
docker build -t exec-assist .

# 2. A volume, before anything writes to it.
fly volumes create exec_assist_data --size 3

# 3. Secrets. Generate the two local ones rather than inventing them.
fly secrets set SESSION_SECRET="$(openssl rand -hex 32)"
fly secrets set TOKEN_KEY="$(openssl rand -hex 32)"

# 4. Push keys. Print them once, set them, and do not regenerate later.
./bin/server vapid-keys        # then: fly secrets set VAPID_PUBLIC_KEY=... VAPID_PRIVATE_KEY=...

# 5. The outside world.
#    Before pasting this key: set a monthly spend limit on it in the Anthropic
#    console. See "Cap the spend" below — it takes two minutes and it is the
#    only thing standing between a retry bug and a surprise bill.
fly secrets set ANTHROPIC_API_KEY=sk-ant-...
fly secrets set VOYAGE_API_KEY=pa-...
fly secrets set GOOGLE_CLIENT_ID=... GOOGLE_CLIENT_SECRET=... \
                GOOGLE_REDIRECT_URL=https://yourdomain/connect/google/callback

# 6. Deploy, then set VAPID_SUBJECT in fly.toml to a real contact address.
make deploy

# 7. Make yourself tenant #1 and check the whole loop works on real data.
fly ssh console -C "/usr/local/bin/server create-tenant -id you -name 'Your Business' -tz America/Los_Angeles"
#    ...then sign in, connect your calendar, and write a brief.

# 8. Prove the backups before you have anyone else's data.
fly ssh console -C "/usr/local/bin/server verify-restore -id you"
```

### Cap the spend

Do this in the Anthropic console before the key ever runs in production: set a **monthly
spend limit** on it, or fund the account with a virtual card holding a small balance.

The app measures what it spends — `ai_usage` records every call in integer micros, and
`/admin` totals it — but measuring is not stopping. A retry loop running unattended
overnight is the classic way a hobby project produces a bill you did not agree to, and no
amount of care in the prompt prevents it. The ceiling has to be something the API enforces,
not something the code intends.

Pick a number you would be annoyed but not hurt by. At one brief plus a handful of
questions per contractor per day, real usage for a five-person pilot is small; the limit is
there for the case where something goes wrong, not for the case where it goes right.

### What to check after the first deploy

The startup log names every disabled feature. Read it once — each line is something
silently not happening:

- `[push] VAPID keys are unset` — briefs are written but never delivered
- `[backup] BACKUP_DIR is unset` — **nothing is being backed up**
- `[google] ... calendar connection is disabled` — no calendar, so no day
- `[extract] ANTHROPIC_API_KEY is unset` — nothing is learned
- `[embed] no VOYAGE_API_KEY set` — recall works but only matches literally

**Never rotate `TOKEN_KEY` casually.** It decrypts stored Google tokens; changing it makes
every existing calendar connection unreadable and every contractor has to reconnect.

One machine, one volume. No database server, no queue, no cache.

**Keep it at one machine.** SQLite is a single file on a single volume; two machines
writing to it will corrupt data. `auto_stop_machines` is off in `fly.toml` for the same
reason — a stopped machine is a missed 6am brief.

## Where this is up to

Phase 0 and Phase 1 of the revised spec are done. What works today: tenants, both stores, the `why_matters`
guard, sign-in, the Today screen showing the day as a plan, Settings with an editable memory,
ask-anything across both stores, Google Calendar with typed-event sync, the extraction
pass with its review queue, the morning brief with web push at 6am local, backups with a
proven restore, observability, and the installable desktop app.

**Next is not a feature.** The spec's Phase 1.5 is a hard gate: recruit three to five
glaziers, charge them, and do not build Phase 2 until at least three keep paying.

Not built yet, and deliberately: SMS (Phase 2), the deterministic conflict engine
(Phase 3), inbound email and the universal webhook (Phase 4), signup and billing
(Phase 5).

### Hardening deferred until there are paying customers

A review against the failure modes people report from running agents in production turned
up four more guards. Three are deferred on purpose — they defend against problems this
product does not have at five customers you can phone, and the MVP is better without them:

- **A replayable trace of model calls.** `ai_usage` records what each call cost but not
  what it said, so a wrong brief cannot be reconstructed after the fact. At pilot scale you
  can just ask the contractor what it told them. Worth building before you have customers
  you cannot phone.
- **An in-code daily cost breaker.** The spend limit on the API key covers the case that
  actually hurts. A per-tenant breaker matters once one tenant's runaway usage could
  degrade another's service.
- **Rate limiting.** There is none. Behind key-based sign-in with a handful of known users
  this is close to theoretical; it stops being theoretical at self-serve signup (Phase 5),
  which is also when `POST /login` becomes worth brute-forcing.

The fourth is a real gap with a specific catch, recorded so it is not rediscovered the hard
way: **the container runs as root.** Adding a `USER` directive alone does not work, because
Fly creates a root-owned filesystem when it mounts the volume at `/data` and that mount
covers whatever the image did — the server then starts and cannot open its database, which
looks exactly like a SQLite bug. The fix is an entrypoint that chowns the mounted volume as
root and then drops privileges with `gosu`. Small, but it needs a real container daemon to
verify, and an unverified change here breaks the deploy rather than degrading it.

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
