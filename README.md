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

### About embeddings

memoryweb expects an Ollama-shaped `/api/embed`. Rather than run Ollama, the server hosts
that shape itself on a loopback listener and translates to a hosted provider
(`internal/embed`). Nothing to install, nothing to supervise.

Semantic search is an upgrade, not a launch requirement. Running with no key is a
legitimate way to operate until there are enough customers to justify the spend.

## Deploying

```sh
fly volumes create exec_assist_data --size 3
fly secrets set SESSION_SECRET="$(openssl rand -hex 32)"
make deploy
```

One machine, one volume. No database server, no queue, no cache.

**Keep it at one machine.** SQLite is a single file on a single volume; two machines
writing to it will corrupt data. `auto_stop_machines` is off in `fly.toml` for the same
reason — a stopped machine is a missed 6am brief.

## Where this is up to

Phase 0 of six. What works today: tenants, both stores, the guard, sign-in, the dashboard,
the "What I've learned" page, and recall by text.

Not built yet: Google Calendar sync, the inbound email address and universal webhook, the
morning and evening briefs over SMS, and billing.

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
