# Pilot workflow: receptionist → shared job → estimate follow-up

This release adds a signed webhook adapter alongside the existing read-only
Google Calendar integration. It does not claim a native connector for a named
receptionist vendor. An administrator must map their tool's events into this
contract, using that tool's supported API/webhook integration.

## Connect and test

1. Configure `TOKEN_KEY`, sign in, and open **Follow-ups, decisions and connected
   tools** from Today (or `/hub`).
2. Create a connection for each source account. Copy the one-time signing key
   and hook path into the adapter; use the deployed site's HTTPS origin.
3. Deliver a `booking.created` event. The hub creates a customer, property and
   job, with source-scoped external IDs. It does not duplicate the appointment
   the receptionist already wrote into Google Calendar.
4. Select the corresponding job and synced calendar stop on `/hub`, choose
   **Review link**, then **Approve link**. The exact local association is logged
   and executed once. The Google calendar itself is not changed.
5. Deliver `measurement.completed` for that same external job. A persistent
   **Send estimate** commitment appears on Today, on the linked stop, in the
   brief, and in business search. Set its owner and who it is waiting on.
6. Deliver `estimate.sent` with a source receipt/reference in `evidence`. The
   commitment closes only if this receipt belongs to the same canonical job
   and happened at or after its originating measurement. Delivery order does
   not matter. Replaying an identical event does nothing.

An event receipt confirms what the connected system reports. It does not prove
that the customer read the estimate. The source adapter must emit `estimate.sent`
only after its source system confirms sending, never when a draft is created.

## HTTP contract

`POST /hooks/{tenant_id}/{connection_id}` with one JSON object, maximum 64 KiB.
Unknown fields are rejected. Payload text is data, never instructions or approval.

```json
{
  "id": "source-event-123",
  "type": "measurement.completed",
  "occurred_at": "2026-09-25T16:00:00Z",
  "job_id": "source-job-456",
  "title": "Ferguson window measure",
  "customer_id": "source-customer-789",
  "customer_name": "Ferguson",
  "property_id": "source-property-321",
  "address": "41 Cedar Lane",
  "due_at": "2026-09-28T17:00:00Z",
  "evidence": "Technician completed measurement; source record measure-123."
}
```

Supported types: `booking.created`, `measurement.completed`, `estimate.sent`.
All fields above except `due_at` are required. Timestamps use RFC3339. `due_at`
defaults to 48 elapsed hours after measurement (not business days); supply it
explicitly when the contractor has a different service target. A job represents
one estimate lifecycle; a new estimate cycle needs a new external job ID.

Send these headers:

- `Content-Type: application/json`
- `X-ExecAssist-Timestamp`: current Unix timestamp in seconds.
- `X-ExecAssist-Signature`: lowercase hex HMAC-SHA256 of
  `timestamp + "." + exact_request_body`, using the displayed signing key's
  UTF-8 bytes. Do not hex-decode the key.

Example signer (adapter-side Python, no third-party dependencies):

```python
import hashlib, hmac, json, time, urllib.request
body = json.dumps(event, separators=(",", ":")).encode()
timestamp = str(int(time.time()))
signature = hmac.new(signing_key.encode(), timestamp.encode() + b"." + body,
                     hashlib.sha256).hexdigest()
request = urllib.request.Request(hook_url, data=body, headers={
    "Content-Type": "application/json",
    "X-ExecAssist-Timestamp": timestamp,
    "X-ExecAssist-Signature": signature,
}, method="POST")
with urllib.request.urlopen(request, timeout=15) as response:
    print(response.read().decode())
```

Signatures expire after five minutes. Retry using the same event ID and content
with a fresh timestamp/signature. IDs must identify immutable source events,
not just jobs. Responses:

- `200`: accepted; JSON contains canonical `job_id` and `duplicate`.
- `400`: invalid payload or unsupported type. Fix the adapter; don't retry unchanged.
- `401`: invalid signature, expired timestamp, or disabled/unknown connection.
- `409`: event ID reused with different content, or job identity changed. Review
  the source IDs; don't silently overwrite an existing customer/property link.
- `413`: payload over the limit.
- `5xx`: transient failure; retry with bounded exponential backoff.

## Identity and authority

External customer/property/job IDs are scoped to a connection account. Similar
names never automatically merge people, properties, or jobs. Source-created
job/customer/property snapshots use their original values; later events do not
overwrite them. Google owns appointment timing/title/location. A contractor's
explicitly approved local link owns the association to a job and survives sync.

The first action capability is `link_appointment`, a local database operation
with a proposal, explicit CSRF-protected decision, current-state check and audit
record in one transaction. There is no generic model-to-tool execution path.
Webhook credentials cannot approve actions. The product's authority stages are
observe → propose → approve → execute → verify; Google remains read-only until a
specific write capability earns its own consent and implementation.

Multiple webhook source accounts are supported. The Google connector still reads
one primary calendar per tenant. Cross-vendor merging and multiple Google accounts
are separate migrations; never imply the current pilot already supports them.

## Persistence and operations

`ledger.db` is authoritative for receipts, external identity bindings,
commitments, action approvals/audit, credentials, and memory confirmation and
provenance. Only source mirror rows are rebuildable. Never delete or recreate the
whole ledger to re-sync a tool. Back up and restore **both** databases together
using the existing backup commands, and preserve the registry and encryption key
through the deployment's recovery procedure.

Disconnecting a webhook disables ingestion but preserves the audit trail and
unfinished work. Reconnect with a new key/connection only after considering ID
mapping: a new connection is a separate source identity, not a transparent key
rotation. Credential rotation in-place is not included in this pilot.

## Pilot acceptance and next decisions

Run the six-step workflow above with actual source events and a calendar for
three to five contractors. Measure handoffs caught, follow-ups completed, wrong
links/corrections, days waiting, and time saved before expanding the connector set.
The existing paid-retention gate remains. Test the hub's workflow before making
retention judgments about a calendar-only brief.

Search is deterministic retrieval across memories, appointments, jobs, contacts,
invoices and follow-ups. It does not execute requests, interpret arbitrary date
constraints, or promise full conversational reasoning. Named-vendor adapters,
email/SMS sending, autonomous external writes and natural-language planning remain
future capabilities, requiring their own live credentials and verification.
