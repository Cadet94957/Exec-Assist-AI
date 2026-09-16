# Google Calendar: launch checklist

Google Calendar is a product connection, not a customer setup task.

## Customer experience

The contractor should only have to:

1. Open Settings.
2. Click **Connect Google Calendar**.
3. Review Google's consent screen.
4. Approve access.
5. Return to Exec Assist and see **Calendar connected and up to date**.

They should never be asked to create a Google Cloud project, enable an API, create OAuth credentials, or copy environment variables. Those are operator/deployment tasks.

The connection reads the contractor's **primary Google Calendar**. It uses the read-only Calendar scope and does not create, edit, or delete calendar events.

## Operator setup

The deployment owner creates and verifies one Google OAuth application for Exec Assist. Set:

- `GOOGLE_CLIENT_ID`
- `GOOGLE_CLIENT_SECRET`
- `GOOGLE_REDIRECT_URL`
- `TOKEN_KEY`

`TOKEN_KEY` must be present before calendar connections are enabled. OAuth tokens are encrypted before entering the tenant ledger.

The Google OAuth application should be published/verified before paid pilot users are onboarded. Do not make the seven-day refresh-token behavior of Google's unverified Testing mode part of the customer experience.

## Connection lifecycle

```text
Settings
  -> Connect Google Calendar
  -> signed, expiring OAuth state
  -> Google consent
  -> callback
  -> validate state + authenticated tenant
  -> exchange authorization code
  -> securely store refresh token
  -> immediate first sync
  -> Connected
```

A failed first sync must not be presented as a clean connection. The account is stored so the background sync can retry, and Settings tells the contractor that the connection needs attention.

## Ongoing health

Connected calendars are refreshed every 10 minutes. The ledger records the last successful sync and the last error. A stale or failing calendar must be visible to the contractor rather than silently feeding stale data into the morning brief.

The Settings page exposes:

- last successful sync
- a manual **Sync now** action
- **Try sync again** when the last sync failed
- **Reconnect** when the contractor needs to re-authorize
- **Disconnect**

The product should prefer an honest stale-data warning over a confident brief built from an unhealthy connection.

## Disconnect semantics

Disconnect removes the local Google credential and the Google-sourced appointments from Exec Assist. It also attempts to revoke the Google refresh token. If Google's revoke endpoint is unavailable, the local credential is still deleted and the UI tells the contractor that remote revocation could not be confirmed.

## Permissions

Keep Google Calendar access read-only. If a future feature needs write access, treat that as a separate product decision with a separate scope, consent experience, and verification review. Never request write permission early "just in case."

## MVP scope

The MVP intentionally uses the primary calendar only. Calendar selection, additional Google products, and write actions can be added later without changing the customer mental model above.
