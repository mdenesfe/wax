# wax

A WhatsApp Web multi-device client written from scratch in Go. It links to a
phone as a "linked device" via QR code and is driven through a REST API with
signed webhooks and a rate-limited, durable send queue. One process serves
many instances (one linked device each).

No WhatsApp-specific libraries are used. Cryptography comes from the Go
standard library only (`crypto/ecdh`, `crypto/ed25519`, `crypto/aes`,
`crypto/cipher`, `crypto/hkdf`, `crypto/sha256`).

## Status

| Phase | Scope | State |
|-------|-------|-------|
| 1 | Transport, Noise handshake, binary nodes | ✅ done |
| 2 | QR pairing, persistent identity (PostgreSQL) | ✅ done |
| 3 | Signal protocol, 1:1 text messages | ✅ done |
| 4 | Groups, media, receipts, presence, calls | ✅ done |
| 5 | History sync, app state sync | ✅ done |
| 6 | Multi-instance, REST API, webhooks, queue | ✅ done |

## Layout

```
api                 REST API (Go 1.22 ServeMux, bearer auth)
cmd/wax             `wax serve` (API server, default) and `wax run` (one instance, console)
cmd/wax-probe       Phase 1 tool: raw handshake + node logging
internal/manager     instances, outbox (token bucket), webhook queue, panic isolation
internal/backoff     shared exponential backoff with jitter
internal/cryptoutil  shared HKDF / HMAC / AES-CBC / AES-GCM helpers
internal/keys        X25519 key pairs, libsignal-compatible Curve25519 signatures
internal/signal      Signal protocol v3: X3DH, Double Ratchet, Sender Keys, wire formats (pure, no I/O)
internal/media       media encryption (HKDF keys, AES-CBC + HMAC), upload/download
internal/appstate    app state sync: LTHash, mutation keys and MACs, patch/snapshot/recovery decoding
internal/transport   framing, WebSocket, Noise XX handshake, cert chain, encrypted conn
internal/wabin       binary XML nodes: tokens, JIDs, encoder/decoder, masked formatting
internal/proto       protobuf schemas + generated code (see internal/proto/README.md)
internal/waversion   client version, live version detection
internal/client      session lifecycle: connect loop, backoff, keepalive, pairing, prekeys
internal/pairing     QR content/rendering, pair-success verification
internal/store       PostgreSQL persistence, secrets sealed with WAX_MASTER_KEY
internal/testutil    fake WhatsApp server for end-to-end tests
```

## Requirements

- Go 1.25+ (pgx v5.11 requires it; `crypto/hkdf` needs 1.24)
- Docker (for the development PostgreSQL)
- protoc + protoc-gen-go, only when changing `.proto` files

## Running the API server

```bash
docker compose up -d                      # PostgreSQL on 127.0.0.1:5433
cp .env.example .env                      # fill in the secrets (see below)
set -a; source .env; set +a
go run ./cmd/wax serve -listen 127.0.0.1:8080 -log-level info
```

The environment carries secrets only, and `.env` holds nothing else:

| Variable | Secret |
|---|---|
| `WAX_MASTER_KEY` | encrypts all keys and sessions at rest; `openssl rand -base64 32`. Losing it means re-pairing every instance |
| `WAX_API_KEY` | bearer token for the API, at least 32 characters; `openssl rand -hex 32` |
| `WAX_DATABASE_URL` | PostgreSQL DSN (contains the password) |
| `WAX_TEST_DATABASE_URL` | DSN of the separate test database (tests only) |

Everything else is a flag of `wax serve`: `-listen` (default
`127.0.0.1:8080`), `-log-level` (`debug|info|warn|error`), `-version`,
`-no-version-check`, and `-allow-private-webhooks` (webhook receivers on this
host or a private network; refused by default to prevent SSRF). Logs are
JSON on stderr; SIGINT/SIGTERM shut down gracefully.

### API contract

`GET /openapi.json` serves the OpenAPI 3.1 description of every endpoint,
schema and webhook event (no key needed). A test compares it with the code
(routes, key types, JSON field names, webhook events), so it cannot drift.

### Metrics

`wax serve -metrics-listen 127.0.0.1:9090` serves Prometheus metrics at
`/metrics` on a separate address without authentication: keep it internal.
They are written by the node itself (no client library):

| Metric | Labels |
|---|---|
| `wax_instance_status` (1 per current status), `wax_instances_running` | tenant, instance, status |
| `wax_messages_received_total`, `wax_messages_sent_total` | tenant, instance (, result) |
| `wax_decrypt_failures_total` (recoverable / error), `wax_disconnects_total` | tenant, instance |
| `wax_outbox_pending`, `wax_webhooks_pending` | tenant, instance |
| `wax_webhook_deliveries_total` (ok / retry / dead), `wax_webhook_delivery_seconds` | tenant (, instance) |
| `wax_api_requests_total`, `wax_api_request_seconds` | route pattern, code |
| `wax_api_rate_limited_total` | tenant |
| `wax_lease_events_total` (acquired / released / lost / fenced) | event |
| `wax_db_connections`, `wax_goroutines`, `wax_heap_bytes` | state |

Each node reports the instances it runs. Per-instance series grow with the
number of accounts (a few series each).

### Running several nodes

Any number of `wax serve` processes can share one database; each instance
runs on exactly one of them:

```bash
wax serve -listen 10.0.0.1:8080 -node-id n1    # -advertise defaults to http://<listen>
wax serve -listen 10.0.0.2:8080 -node-id n2
```

- **Leases.** A node owns an instance through a lease in the database and
  renews it every 10 s (leases last 30 s; all times are database time, so
  node clocks do not matter). Free leases are claimed every 2 s.
- **Failover.** A node that shuts down releases its leases; another node
  takes the instances over in about 2 s and reconnects without a QR code.
  After a crash the takeover waits until the lease expires (at most 30 s),
  never sooner, so an account never runs on two nodes. A node that cannot
  reach the database stops its instances one round before its leases could
  expire.
- **Balancing.** Each node runs a fair share (instances ÷ live nodes,
  rounded up, at most `-max-instances`); a joining node takes over the
  surplus of the others.
- **Routing.** Requests may reach any node. A request for an instance that
  runs elsewhere is forwarded once to its owner (`-advertise` must be
  reachable by the other nodes; keep that network private or put TLS in
  front). Listings are answered from the database by any node.
- **Limit.** A node that freezes for longer than a lease (for example a
  suspended VM) and then resumes may briefly overlap with the new owner;
  WhatsApp then disconnects one of the two ("replaced"), and that instance
  waits for `POST /v1/instances/{id}/connect`.

### Tenants and keys

The API is multi-tenant. Every request carries `Authorization: Bearer <key>`:

- **Admin key** (`WAX_API_KEY`): manages tenants and their keys only. It
  cannot read or control any instance.
- **Tenant key**: manages that tenant's instances only. Another tenant's
  instance answers `404`, so even its existence is hidden. Instance names
  are unique per tenant (two tenants can both have `main`).

Tenant keys look like `wax_<id>_<secret>`. They are shown once, when
issued; the database keeps only an HMAC of the secret (keyed from
`WAX_MASTER_KEY`), and every request is checked against it, so a revoked key
stops working immediately.

| Method & path (admin key) | Purpose |
|---|---|
| `POST /v1/tenants` | create `{id, name, max_instances, requests_per_second}` |
| `GET /v1/tenants`, `GET /v1/tenants/{tenant}` | list tenants, show one |
| `PATCH /v1/tenants/{tenant}` | change name or limits (absent fields keep their value) |
| `DELETE /v1/tenants/{tenant}` | delete (only once it has no instances) |
| `POST /v1/tenants/{tenant}/keys` | issue a key; the response is the only place it appears |
| `GET /v1/tenants/{tenant}/keys` | list key ids |
| `DELETE /v1/tenants/{tenant}/keys/{key}` | revoke a key |

Instances created before tenants existed belong to the tenant `default`.

### Resource isolation

- **API rate per tenant**: `requests_per_second` (burst: two seconds'
  worth; 0 = the node default set with `-tenant-rps`, 20). Above it the API
  answers `429` with `Retry-After`; other tenants are unaffected. Limits
  apply per node.
- **Instance quota per tenant**: `max_instances` (0 = no quota), enforced
  atomically, also for concurrent requests (`403` when reached).
- **Large uploads**: at most 4 large request bodies per node at once, and
  at most half of them for one tenant.
- **Heavy background work**: history sync chunks of all accounts on a node
  are processed at most 2 at a time.
- **Sends**: each instance's outbox is paced by its `rate_per_minute`.
- **Database connections**: set `pool_max_conns` in `WAX_DATABASE_URL`
  (for example `...?sslmode=disable&pool_max_conns=20`); the node logs the
  pool size at start. pgx defaults to max(4, CPUs).
- Requests forwarded between nodes carry a signature (keyed from
  `WAX_MASTER_KEY`), so a client cannot forge one to skip the rate limit.

All routes below use a tenant key.

| Method & path | Purpose |
|---|---|
| `POST /v1/instances` | create `{id, webhook_url, webhook_secret, rate_per_minute}` |
| `GET /v1/instances`, `GET/PATCH/DELETE /v1/instances/{id}` | list, status, update, logout + delete |
| `POST /v1/instances/{id}/connect` | start a new pairing after logout |
| `GET /v1/instances/{id}/qr` | current QR (`?format=png` for an image) |
| `POST /v1/instances/{id}/messages` | queue `{to, text}` or `{to, media:{kind, mimetype, caption, file_name, data_base64}}`; `client_id` makes it idempotent |
| `GET /v1/instances/{id}/outbox/{oid}` | send status |
| `GET /v1/instances/{id}/chats` | chats (`limit`, `offset`) |
| `GET /v1/instances/{id}/chats/{chat}/messages` | messages (`limit`, `before` RFC 3339) |
| `GET …/messages/{mid}/media` | download and decrypt a media message |
| `POST /v1/instances/{id}/read`, `/presence` | read receipts, availability |
| `GET /v1/instances/{id}/groups/{group}` | group metadata |
| `POST /v1/instances/{id}/calls/{call}/reject` | reject an incoming call |

`to` is a phone number (digits, optional `+`) or a JID. Webhooks are POSTed
in order per instance as `{event, instance, timestamp, data}` with
`X-Wax-Signature: sha256=<hex HMAC-SHA256(webhook_secret, body)>`; failed
deliveries are retried with backoff for about a day. Events include
`message`, `receipt`, `send_status`, `qr`, `connected`, `disconnected`,
`logged_out`, `call`, `group_update`, `presence`, `history_sync`,
`app_state_unverified`.

### App state integrity

Every app state record is authenticated individually (content and index
MACs) and every patch by its patch MAC. The collection-wide LTHash is checked
too, but for some collections of real accounts it covers records that are
never delivered to linked devices (even the hash the phone sends in a
recovery differs from the sum of its own records). Such a collection is
accepted as *unverified*: `app_state_unverified` is sent once, the phone is
asked for its authoritative copy (at most hourly), and a recovery restores
full verification. A verified collection whose LTHash stops matching is
still treated as an integrity failure (resync, then recovery).

## Running one instance on the console

```bash
go run ./cmd/wax run -instance test       # shows a QR code on first start
```

After pairing, restarting the same command logs in without a QR code.
Useful flags: `-debug -dump-nodes` (masked node log), `-qr-png qr.png`,
`-version 2.3000.X`, `-send-to <number|jid> -text <text>` (send once after
connecting; add `-send-file <path>` to send a file), `-media-dir <dir>` (save
received media), `-reject-calls`, `-push-name <name>`. While running, stdin
accepts `/send <number|jid> <text>`, `/media <number|jid> <file> [caption]`
and `/group <jid>`.

`WAX_MASTER_KEY` encrypts all private keys at rest. Keep it outside the
database backups; losing it means re-pairing every instance.

## Tests

```bash
go test -race ./...                       # unit tests; DB tests are skipped
WAX_TEST_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax_test?sslmode=disable go test -race ./...
```

The second form also runs the store tests and the end-to-end client tests
against an in-process fake WhatsApp server (pairing, 515 restart, login,
prekey upload, logout, keepalive timeout) and the API end-to-end test (pairing,
webhooks with signatures and retries, outbox, idempotency, delete). The
whole suite runs in about two seconds of test time.
