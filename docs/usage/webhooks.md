# Webhooks

wax reports what happens on an account (incoming messages, receipts, send
results, pairing and connection changes, calls, and more) by POSTing JSON
to the instance's `webhook_url`. Events are stored in a durable queue in
the database first, so they survive restarts and node failover.

## Configuring

Set the URL and a secret when creating the instance or later:

```bash
curl -X PATCH http://localhost:8080/v1/instances/main \
  -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"webhook_url": "https://hooks.example.com/wax", "webhook_secret": "<a long random string>"}'
```

- `webhook_url` must be an absolute `http` or `https` URL without
  credentials. Use `https` in production.
- `webhook_secret` is optional but strongly recommended: without it,
  deliveries carry no signature. Generate it like any other secret, for
  example `openssl rand -hex 32`. It is never returned by the API.

Events are queued only while a webhook URL is set. Events that happen while
an instance has no URL are not stored for later.

## Delivery

Each event is one HTTP request:

```
POST /wax HTTP/1.1
Host: hooks.example.com
Content-Type: application/json
User-Agent: wax-webhook/1
X-Wax-Event: message
X-Wax-Delivery: 1234
X-Wax-Instance: main
X-Wax-Signature: sha256=5f2b...e9

{"event":"message","instance":"main","timestamp":"2026-01-01T12:00:00Z","data":{...}}
```

The body is always an envelope:

| Field | Meaning |
|---|---|
| `event` | Event name (see the list below). Also in the `X-Wax-Event` header. |
| `instance` | Instance name (as the tenant sees it). Also in `X-Wax-Instance`. |
| `timestamp` | When wax produced the event (UTC, RFC 3339). |
| `data` | Event-specific object. |

`X-Wax-Delivery` is the delivery id. It stays the same across retries of
the same event, so you can use it to drop duplicates.

**Success** is any `2xx` response within 15 seconds. Everything else
(timeouts, connection errors, `3xx`, `4xx`, `5xx`) is a failure and the
event is retried. Redirects are not followed: the receiver must answer at
the configured URL. Proxy environment variables are ignored for webhook
requests.

**Ordering.** Deliveries for one instance are strictly ordered: the next
event is sent only after the current one was delivered or given up. A
receiver that keeps failing therefore holds back that instance's later
events. Answer quickly (queue the work on your side and return `2xx`)
rather than processing inside the request. Different instances are
delivered independently.

## Verifying the signature

When the instance has a `webhook_secret`, every delivery carries:

```
X-Wax-Signature: sha256=<hex of HMAC-SHA256(webhook_secret, raw request body)>
```

To verify it, compute the HMAC over the **raw body bytes exactly as
received** (before any JSON parsing or re-encoding), and compare it with the
header in constant time. Reject the request if they differ.

The signature is computed with the secret configured at the time of each
delivery attempt. If you rotate the secret, events still in the queue are
signed with the new one; accept both secrets on your side for a short time
while you switch.

### Go

```go
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// validSignature reports whether header is the wax signature of body.
func validSignature(secret, body []byte, header string) bool {
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

type envelope struct {
	Event     string          `json:"event"`
	Instance  string          `json:"instance"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

func main() {
	secret := []byte(os.Getenv("WEBHOOK_SECRET"))
	http.HandleFunc("POST /wax", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if !validSignature(secret, body, r.Header.Get("X-Wax-Signature")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var e envelope
		if err := json.Unmarshal(body, &e); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		log.Printf("delivery %s: %s from %s", r.Header.Get("X-Wax-Delivery"), e.Event, e.Instance)
		w.WriteHeader(http.StatusNoContent) // hand the work to a queue; answer fast
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:9000", nil))
}
```

### Node.js

Uses only built-in modules (Node 18 or newer).

```js
const http = require("node:http");
const crypto = require("node:crypto");

const secret = process.env.WEBHOOK_SECRET;

// validSignature reports whether header is the wax signature of body (a Buffer).
function validSignature(body, header) {
  if (typeof header !== "string" || !header.startsWith("sha256=")) return false;
  const got = Buffer.from(header.slice(7), "hex");
  const want = crypto.createHmac("sha256", secret).update(body).digest();
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}

http.createServer((req, res) => {
  if (req.method !== "POST" || req.url !== "/wax") {
    res.writeHead(404).end();
    return;
  }
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks); // the raw bytes, not re-serialized JSON
    if (!validSignature(body, req.headers["x-wax-signature"])) {
      res.writeHead(401).end("bad signature");
      return;
    }
    const { event, instance, data } = JSON.parse(body);
    console.log(`delivery ${req.headers["x-wax-delivery"]}: ${event} from ${instance}`, data);
    res.writeHead(204).end(); // hand the work to a queue; answer fast
  });
}).listen(9000, "127.0.0.1");
```

With Express, read the body with `express.raw({ type: "application/json" })`
for this route; `express.json()` would hand you parsed JSON and the HMAC of
re-serialized JSON will not match.

## Retries and dead events

A failed delivery is retried with exponential backoff and jitter:

- An event is retried for **24 hours** after it was queued (with at most
  50 attempts as a safety net).
- The delay after attempt *n* is random between 2.5 seconds and a ceiling
  of 5 s x 2^(n-1) (5 s, 10 s, 20 s, ... capped at 4 hours), so a receiver
  that is down for hours is retried every few hours once the ceiling is
  reached.
- When a failed attempt is past the 24 hours, the event is marked **dead**: it is not
  delivered again, the node logs `webhook delivery given up` with the
  delivery id, event and error, and the metric
  `wax_webhook_deliveries_total{result="dead"}` increases. The next event
  in the queue is then delivered.

There is no API to replay dead events. If your receiver may be down for
longer, plan to resynchronize from the API (for example list recent
messages with `GET /v1/instances/{id}/chats/{chat}/messages`, and check
send results with `GET /v1/instances/{id}/outbox/{oid}`).

Delivered and dead events are purged from the database after 7 days.
`wax_webhooks_pending` shows the backlog per instance.

## Changing the URL

A URL change takes effect for the queue immediately:

- **New URL:** pending events (including ones waiting for a retry) are
  retried at once, against the new URL. You do not have to wait for the old
  backoff to expire.
- **URL removed** (`"webhook_url": ""`): pending events are dropped (marked
  dead with `webhook url removed`), and no new events are queued.

A `PATCH` that leaves out `webhook_secret` keeps the current secret, so
changing only the URL never turns signing off by accident.

## Private address protection

By default wax refuses to deliver webhooks to addresses that are not
public: loopback, private networks (RFC 1918 and IPv6 ULA), link-local
(including cloud metadata services such as `169.254.169.254`), the shared
address space `100.64.0.0/10`, multicast and unspecified addresses. The
check runs on the resolved IP at connect time, so DNS names that resolve
(or later rebind) to internal addresses are refused too. This prevents a
tenant from using webhooks to reach services inside your network.

Such deliveries fail with `webhook address is not public` and are retried
like any other failure until they are dead. The URL itself is accepted when
you set it; the check happens on delivery.

If your receiver really runs on the same host or a private network, start
the server with:

```bash
wax serve -allow-private-webhooks
```

This disables the check for all tenants on that node, so use it only when
you trust every tenant (or for local development).

## Events

The authoritative list, with the schema of each event, is the `webhooks`
section of `GET /openapi.json`.

### Messages

| Event | Description | Main `data` fields |
|---|---|---|
| `message` | A message was received, or sent from another device of the same account (`from_me: true`). Includes reactions and protocol messages. | `id`, `chat`, `sender`, `sender_alt`, `from_me`, `is_group`, `push_name`, `timestamp`, `type`, `text`, `media` (with a `download` API path), `reaction`, `quoted_id`, `quoted`, `mentions`, `forwarded`, `forwarding_score`, `view_once`, `raw` (the decrypted protobuf as protojson), `source` (`live`) |
| `undecryptable_message` | A message could not be decrypted; wax has asked the sender to retry, so the message often follows as a `message` event. | `id`, `chat`, `sender`, `error` |
| `receipt` | Delivery or read receipts for messages. | `ids`, `chat`, `sender`, `type` (`delivered` when empty on the wire, otherwise WhatsApp's receipt type such as `read`), `timestamp` |
| `send_status` | An outbox item was sent or finally failed. | `outbox_id`, `client_id`, `message_id`, `chat`, `status` (`sent` or `failed`), `timestamp` (sent) or `code` and `error` (failed) |

In a `message` event, `type` is one of `text`, `reaction`, `protocol`,
`image`, `video`, `audio`, `voice`, `document` or `sticker`.

A failed `send_status` carries a stable `code` for programs, and `error`
with the details for people:

| `code` | Meaning |
|---|---|
| `not_on_whatsapp` | The number has no WhatsApp account. |
| `invalid_recipient` | Not a user or group JID, or a chat that cannot be sent to (broadcast lists). |
| `server_rejected` | WhatsApp refused the message. |
| `invalid_message` | The message cannot be built, for example a sticker that is not WebP. |
| `retries_exhausted` | Temporary errors (network, timeouts) until the outbox gave up. |

### Pairing and connection

| Event | Description | Main `data` fields |
|---|---|---|
| `qr` | A QR code to scan (codes rotate while pairing). | `code`, `png_base64`, `timeout_seconds`, `index`, `total` |
| `qr_timeout` | No QR code was scanned in time; call `POST /v1/instances/{id}/connect` to start again. | (empty) |
| `paired` | The phone linked this device. | `jid`, `lid`, `platform`, `business_name` |
| `pair_error` | Pairing failed. | `error` |
| `connected` | Logged in and online. | `jid` |
| `disconnected` | The connection ended. `action` says what happens next: `reconnect` (with backoff), `reconnect-now`, `logged-out`, `replaced`, `upgrade-required`, `temp-banned` or `fatal`. | `action`, `retry_in_seconds`, `error` |
| `logged_out` | The device was unlinked; a new QR pairing is needed. | `code`, `detail` |
| `replaced` | Another connection took over this session; call `connect` to resume. | (empty) |
| `client_outdated` | WhatsApp rejected the client version. | `version` |

### Contacts, groups and calls

| Event | Description | Main `data` fields |
|---|---|---|
| `call` | Call signalling: `type` is for example `offer` (an incoming call), `accept`, `reject` or `terminate`. Use `call_id` to reject an offer. | `type`, `from`, `call_id`, `creator`, `group`, `video`, `reason`, `timestamp` |
| `presence` | A contact's availability. | `from`, `available`, `last_seen` |
| `chat_presence` | Typing or recording state in a chat. | `chat`, `sender`, `state`, `media` |
| `group_update` | Group membership or metadata changed. | `group`, `changes` |
| `identity_changed` | A contact's identity key changed (for example a new phone). | `jid` |

### Sync

| Event | Description | Main `data` fields |
|---|---|---|
| `history_sync` | A chunk of chat history from the phone was stored (it is then available through the chats and messages endpoints). | `type`, `chunk`, `progress`, `conversations`, `messages`, `push_names` |
| `app_state_error` | An app state collection (contacts, chat settings and so on) failed to sync. | `collection`, `error` |
| `app_state_unverified` | An app state collection's overall hash cannot be verified, although each record is authenticated. The phone is asked for its authoritative copy (at most hourly); a successful recovery restores full verification. Sent once per collection. | `collection`, `version` |
| `chat_update` | A chat was archived, pinned or marked read on the phone (or the reverse). Only the settings the change carries are present. | `chat`, `chat_alt`, `archived`, `pinned`, `read`, `timestamp`, `source`, `full_sync` |

`chat_update` comes from two sources (`source`):

- `app_state`: a change made on the phone, or, right after linking, the
  full sync of the phone's settings (`full_sync: true`, one event per chat
  setting). `read: false` means the chat was marked unread. `timestamp` is
  when the change was made on the phone.
- `history`: the state of the chats in history sync, sent for archived or
  pinned chats only (others are neither). The value is the one stored after
  the chunk, so a newer change from app state wins.

`chat` is the chat as WhatsApp names it, which may be a LID
(`...@lid`); `chat_alt` is its other address (the phone number JID for a
LID and the reverse) when wax knows it.
