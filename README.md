# Exec Assist

**An AI executive assistant for independent trade contractors, starting with glaziers.**

Exec Assist isn't another AI receptionist; contractors already have one of those.
It sits *above* the tools they already pay for: the receptionist that books the
job, the calendar, the invoicing app, and the inbox. It's the one place that sees
the whole picture and remembers *why* things are done the way they are.

> Nothing else in a contractor's stack knows that the Marriott on 4th bills the
> general contractor rather than the property manager, always wants tempered
> glass, and locks the loading dock at 3pm. Exec Assist does, and it gets better
> the longer it works for you.

---

## A day with Exec Assist

**6:00am: the morning brief arrives on your phone.** It lists today's stops in
order, the tight gap between the second and third, an invoice that's gone
overdue, and a cancellation from overnight. It's written in plain language and
short enough to read at a red light.

**On the Today screen**, each stop has what you need attached to it: the gate
code for the Harborview job, and a 45-minute parking buffer added to the
downtown measure because you always need one there. Nobody had to ask for that.

**After a measurement**, an *estimate to send* follow-up appears automatically.
It closes itself when your connected tool confirms the estimate went out.

**Over time, it learns.** Exec Assist notices patterns in your work and
suggests them as rules ("Marriott jobs bill the GC"). You confirm or dismiss
each one with a tap, and nothing is treated as fact until you say so.

## What it does

| | |
|---|---|
| 📅 **Plans your day** | Pulls in Google Calendar or iCloud Calendar and lays out the day as an ordered plan, with gaps measured and conflicts flagged. |
| ☀️ **Writes a morning brief** | Delivered at 6am in your time zone: stops, money owed, what changed since yesterday. |
| 🧠 **Remembers your business** | Keeps a memory of customer quirks, site rules, and preferences. Every entry records *why* it matters, and you can see and edit all of it. |
| 🔁 **Tracks follow-ups** | Connects to your receptionist or booking tool, creates job records, and keeps estimates and other loose ends from slipping. |
| 🔎 **Searches everything** | One search box covers memory, appointments, jobs, follow-ups, contacts, invoices, and email. |
| 📥 **Reads your inbox** | Connects to iCloud Mail and Gmail, read-only, so email shows up in search and the brief. |

## Built so a contractor can trust it

- **Read-only by design.** Exec Assist can *see* your calendar and email, but it
  has no ability to change or delete them. A bug or a stolen password can't wipe
  your week, because the code to do so doesn't exist, and an automated test fails
  the build if that ever changes.
- **It asks before acting.** Actions go through a clear cycle: Exec Assist
  notices something, proposes an action, waits for your approval, carries it
  out, and confirms the result.
- **It never states a guess as fact.** Learned rules stay marked *tentative*
  until you confirm them, and the brief hedges anything unconfirmed. ("You
  usually allow about 45 minutes," never "you need 45 minutes.")
- **It's measured against making things up.** Every version of the brief is
  automatically checked to make sure it never invents a number, a name, or a
  time that isn't in your data.
- **Your data is yours.** Each business gets its own separate database files.
  "Export everything you know about my business" is literally a file copy, and
  backups are restore-tested, not just written.
- **Accessible.** Every page passes automated accessibility checks in both light
  and dark mode, and the app is fully usable with a keyboard.

## Try the demo

No installation and no accounts are needed. Download the build for your
computer from the repository's **Actions → Build downloads** page, unzip it,
and run it. Your browser opens to a sample glazier's day, already signed in.

Step-by-step instructions for Mac and Windows, including the one-time security
prompt, are in [the developer guide](docs/development.md#trying-it-out).

Exec Assist works as a website and as an installable desktop or phone app from
the same build.

## Where it stands

The core product works end to end:

- the Today plan
- business memory with review
- calendar and email sync
- the follow-up hub
- the morning brief with phone notifications
- backups with tested restore
- the installable app

**Next:** a paid pilot with three to five glaziers, using real calendars and
booking tools. It will measure missed handoffs caught, follow-ups completed,
and time saved.

## Under the hood

A single Go program with one SQLite database per business, so there's no
database server or other infrastructure to run. The web interface is
server-rendered with HTMX and installable as an app.
Anthropic's Claude writes the brief and powers the learning; Voyage embeddings
can optionally power meaning-based search. It deploys to Fly.io.

## Documentation

| Guide | What's in it |
|---|---|
| [Developer & operator guide](docs/development.md) | Architecture, running locally, configuration, Google Calendar setup, how learning and the brief work, deploying, backups |
| [Apple and Gmail setup](docs/apple-and-gmail.md) | Connecting iCloud Calendar, iCloud Mail, and Gmail |
| [Hub workflow](docs/hub-workflow.md) | Connecting a receptionist or booking tool, and the follow-up workflow |
| [Google Calendar launch](docs/google-calendar-launch.md) | Launch checklist for offering Google Calendar as a built-in connection |
