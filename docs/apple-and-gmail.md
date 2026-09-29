# Try Exec Assist with your Apple Calendar, Apple Mail, and Gmail

These connections read iCloud Calendar through CalDAV and iCloud Mail/Gmail
through IMAP over TLS. They work together with the existing Google Calendar
connector. No Google Cloud project is needed for the new Gmail inbox connection.

Apple Mail and Apple Calendar are apps that can display accounts from several
providers. Use the iCloud connections only for data stored in iCloud. A Gmail
account viewed in Apple Mail still uses the Gmail connection. Calendars stored
only “On My Mac” are not available to this connector.

## 1. Download the new build

In GitHub, open **Actions → Build downloads → Run workflow** on the branch
containing this change. Download the artifact for your computer after the build
succeeds, unzip it, and follow `READ ME FIRST.txt`.

Start with the **personal** option to create a separate account without sample
appointments or sample memories. For an Apple silicon Mac, in Terminal:

```sh
cd ~/Downloads/ExecAssist-Mac-AppleSilicon
xattr -c ExecAssist-Mac-AppleSilicon
chmod +x ExecAssist-Mac-AppleSilicon
./ExecAssist-Mac-AppleSilicon quickstart -personal -tz America/Los_Angeles
```

For an Intel Mac, substitute `ExecAssist-Mac-Intel` in those commands.
For Windows, open PowerShell in the unzipped folder and run:

```powershell
.\ExecAssist-Windows.exe quickstart -personal -tz America/Los_Angeles
```

For a source checkout with Go and the C/SQLite build prerequisites:

```sh
make build
./bin/server quickstart -personal -tz America/Los_Angeles
```

Your browser opens already signed in. Keep the terminal open. Subsequent starts
with `-personal` reuse that account. Your original demo account stays separate.
Quickstart creates and reuses a local encryption key automatically; no credential
environment variables are needed for these three connections.

## 2. Connect Apple Calendar

1. At [account.apple.com](https://account.apple.com), sign in to your Apple Account.
2. Open **Sign-In and Security → App-Specific Passwords** and generate a password
   named “Exec Assist Calendar”. Your Apple Account needs two-factor authentication.
3. In Exec Assist, open **Settings → Apple and Gmail → Apple Calendar (iCloud)**.
4. Enter your **Apple Account sign-in email** and the generated password.
5. Choose **Connect and sync**. Check Today against your actual calendar.

The connector discovers your iCloud calendar collections. It reads a window
from 30 days ago through 60 days ahead, including server-expanded recurrence
instances and moved exceptions. All-day/floating dates use the account timezone.
If the server cannot supply a complete supported response, sync reports an error
and preserves the previous copy; it does not replace it with a partial schedule.

## 3. Connect Apple Mail

1. Generate another Apple app-specific password named “Exec Assist Mail”.
2. In **Settings → Apple Mail (iCloud)**, enter your **full iCloud mailbox address**
   (`@icloud.com`, `@me.com`, `@mac.com`, or an iCloud-hosted custom domain address)
   and that password. This may differ from your Apple Account sign-in email.
3. Choose **Connect and sync**, then open **Inbox**.

The fixed server is `imap.mail.me.com:993`. Local folders in the Apple Mail app
and other providers shown in that app are not read.

## 4. Connect Gmail

1. Enable 2-Step Verification in your Google Account if it is not already on.
2. Open [Google App passwords](https://myaccount.google.com/apppasswords) and create
   a password named “Exec Assist”.
3. In **Settings → Gmail**, enter your full Gmail/Google Workspace email and the
   generated 16-character password. Spaces in the displayed password are removed.
4. Choose **Connect and sync**, then open **Inbox**.

The fixed server is `imap.gmail.com:993`. If App passwords is unavailable, this
connection method cannot be used with that account. Google may block it for work
or school accounts, Advanced Protection, or some security-key-only configurations.
A Workspace administrator may also disable IMAP. This release does **not** add
Gmail OAuth or ask users to weaken those policies.

## 5. Try the useful loop

- Compare **Today** with your iCloud events.
- Open **Inbox** and check that recent mail from both providers appears.
- Use **Search your business** on Today to find a sender or subject.
- If you have configured `ANTHROPIC_API_KEY`, choose **Write today's brief**.
  The brief includes up to 15 recent inbox previews from the last 24 hours.
  Without that key, calendar, inbox, and search still work.
- Change a test event in Apple Calendar, then choose **Sync now** in Settings.
- Read a message here and confirm it stays unread in your normal mail app.

Mail previews may be sent to the configured AI provider when producing a brief.
Email is treated as untrusted sender claims, not as instructions or permission to
act. Mail does not automatically create or complete business follow-ups and is
not currently used by the long-term memory extraction pass.

## Current limits and behavior

- One account per new connector; Apple Mail and Gmail can both be connected.
- Automatic sync every ten minutes while the application is running; manual sync
  and initial connection fetch immediately. A local laptop must stay awake.
- Inbox only, latest 200 messages per provider from the past 30 days. Not Sent,
  Archive, custom folders, attachments, or a full historical mailbox search.
- Each email fetch is capped at the first 64 KiB; stored text is capped at 8,000
  characters. Some MIME messages have no usable preview within that limit.
- HTML is converted to text; scripts, external images, and attachments are not
  rendered. Reading uses IMAP `EXAMINE` and `BODY.PEEK`, preserving unread flags.
- Inbox sync replaces the bounded local snapshot atomically. Archived/deleted
  mail leaves that snapshot on successful sync. A failed sync preserves it.
- App passwords grant broader permissions than the app uses. The app issues no
  mail sends, mail updates, calendar writes, or calendar deletions.
- Disconnect removes the stored credential and that connector's local inbox or
  calendar mirror. Previously generated briefs, learned memories, and backups
  remain. Revoke the app password at Apple/Google to invalidate it remotely.
- Local quickstart stores its AES-GCM key in `token.key` under its data directory,
  created with owner-only permissions on Unix. Preserve it with your data. This
  protects credentials in database-only copies; it is not protection against
  someone who can read both the database and the key on your computer.
- Hosted `serve` mode still requires a stable `TOKEN_KEY` secret and HTTPS. It
  never auto-generates a disk key. Back up the registry and both tenant databases,
  and retain the encryption key separately in the deployment secret manager.

## Verification

Automated tests cover CalDAV discovery, expanded recurring instances, all-day
conversion, rejected credential destinations, IMAP read-only wire commands, MIME
previews, encrypted credentials, failed-sync preservation, atomic replacement,
source and tenant isolation, form CSRF protection, and mail in the brief.
Real Apple/Google authentication requires your account credentials and must be
verified by connecting in Settings. Do not paste app passwords into chat or GitHub.

Provider setup references:
- [Apple app-specific passwords](https://support.apple.com/102654)
- [iCloud Mail server settings](https://support.apple.com/102525)
- [Google app passwords and availability](https://support.google.com/accounts/answer/185833)
