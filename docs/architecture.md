# Architecture

This document describes how wax is built: which package does what, how a
WhatsApp session runs inside the process, how state is stored, how several
instances, tenants and nodes share one database, and what the security model
is. Protocol details (Noise, binary nodes, Signal, app state) are covered in
[docs/protocol/](protocol/README.md); the REST API and admin interface are documented
for users in [docs/usage/](usage/getting-started.md).

Everything here describes the code as it is. Where something is not
implemented, this document says so.

## Package map

The module is `github.com/mdenesfe/wax`. Dependencies point downwards only:
the protocol packages know nothing about the client, the client knows nothing
about the manager, and the manager knows nothing about HTTP.

```
 +--------------------------------------------------------------------------+
 |  cmd/wax (serve, run)    cmd/wax-probe    cmd/wax-demo    examples/*     |
 +--------------------------------------------------------------------------+
 |  api  (REST, auth, forwarding, limits, metrics, OpenAPI)  --> webui      |
 |  wax  (the public Go library, a facade over internal/client)             |
 +--------------------------------------------------------------------------+
 |  internal/manager  (instances, leases, outbox, webhooks, metrics)        |
 +--------------------------------------------------------------------------+
 |  internal/client   (session lifecycle, handlers, send, sync, caches)     |
 +--------------------------------------------------------------------------+
 |  internal/store    (PostgreSQL, sealing, migrations)                     |
 +--------------------------------------------------------------------------+
 |  transport   wabin   signal   keys   media   appstate   pairing          |
 |  proto/*     cryptoutil   backoff   waversion   ratelimit   metrics      |
 +--------------------------------------------------------------------------+
```

| Package | Role |
|---|---|
| [internal/wabin](../internal/wabin) | Binary XML nodes: token tables, JIDs, encoder and decoder, masked formatting for logs |
| [internal/transport](../internal/transport) | Frame layer, WebSocket, Noise XX handshake, certificate chain check, encrypted connection |
| [internal/keys](../internal/keys) | X25519 key pairs and libsignal-compatible Curve25519 signatures |
| [internal/signal](../internal/signal) | Signal protocol v3 (X3DH, Double Ratchet, Sender Keys, wire formats). Pure, no I/O |
| [internal/media](../internal/media) | Media encryption (HKDF keys, AES-CBC plus HMAC), upload and download, WebM to Ogg conversion |
| [internal/appstate](../internal/appstate) | App state sync: LTHash, mutation keys and MACs, patch and snapshot decoding |
| [internal/pairing](../internal/pairing) | QR content and rendering, pair-success verification |
| [proto](../proto) | Protobuf schemas and generated code; public, since message contents are these types |
| [internal/cryptoutil](../internal/cryptoutil), [internal/backoff](../internal/backoff), [internal/waversion](../internal/waversion), [internal/ratelimit](../internal/ratelimit), [internal/metrics](../internal/metrics) | Shared helpers: HKDF/HMAC/AES, backoff with jitter, client version detection, token bucket, Prometheus text output |
| [internal/store](../internal/store) | PostgreSQL persistence; imports `keys`, `signal`, `appstate` and `wabin` for its row types |
| [internal/client](../internal/client) | One linked-device session: connect loop, pairing, login, messages, groups, media, sync |
| [internal/manager](../internal/manager) | Many instances on one node, leases across nodes, outbox, webhook queue |
| [api](../api) | REST API on Go 1.22 `ServeMux` |
| [internal/webui](../internal/webui) | Embedded admin interface (static files) |
| [cmd/wax](../cmd/wax) | `wax serve` (API server, default) and `wax run` (one instance on the console) |
| [internal/testutil/fakewa](../internal/testutil/fakewa) | In-process fake WhatsApp server for tests |
| [wax](../wax.go) (module root) | The public Go library: `Open`, `Run`, events, sending, groups, profiles; type aliases over `internal/client`, which stays free to change ([usage/library.md](usage/library.md)) |
| [cmd/wax-demo](../cmd/wax-demo), [examples/echo](../examples/echo) | The admin interface with made-up data; a minimal library program |

`wax run` ([cmd/wax/run.go](../cmd/wax/run.go)) uses `internal/client`
directly, without the manager or the API, and so does the public library
`wax`: neither takes leases, so an account must run in one process at a
time there. `cmd/wax-probe` is a phase 1 tool
that performs the raw handshake and logs nodes.

## The client

[internal/client](../internal/client) runs one WhatsApp linked device. A
`Client` is created with `client.New(Config)` and driven by `Run(ctx)`, which
blocks until the context ends or a terminal condition occurs.

### Session lifecycle and connect loop

`Run` ([client.go](../internal/client/client.go)) loops:

1. Load the device from the store, or create and store a new unpaired one
   (fresh noise key, identity key, signed prekey, ADV secret). A database
   error is retried with backoff, so a database restart does not stop an
   instance.
2. Build the client payload: a login payload for a paired device, a
   registration payload otherwise. The WhatsApp Web version is read on every
   attempt (`Config.Version`), so a newer version applies on the next
   reconnect.
3. `runSession` ([session.go](../internal/client/session.go)) dials (30 s
   timeout), performs the Noise handshake and serves the connection until it
   ends, returning a `sessionResult` with a `DisconnectAction`.
4. The action decides what happens next:

| Action | Behaviour |
|---|---|
| `ActionReconnectNow` | Reconnect at once, backoff reset (for example the 515 restart after pairing) |
| `ActionReconnect` | Reconnect after `Backoff.Delay(attempt)`; the counter resets after a session that stayed logged in for 30 s |
| `ActionLoggedOut` | Delete the stored device, emit `LoggedOut`, return `ErrLoggedOut` |
| `ActionReplaced` | Emit `StreamReplaced`, return `ErrReplaced` |
| `ActionUpgradeRequired` | Emit `ClientOutdated`, return `ErrClientOutdated` |
| `ActionTempBanned` | Emit `TemporaryBan`, return `ErrTempBanned` |
| other (fatal, QR timeout, pairing failure) | Return the error |

Every session end emits `Disconnected` first.

### Goroutines of a session

Each session has three long-lived goroutines plus tracked helpers:

```
            WebSocket
               |
          +----v-----+   iq result/error, ack   +-------------------+
          |  reader  |------------------------->| waiters (by id)   |
          +----+-----+                          +-------------------+
               | everything else                         ^
               v                                         | sendIQ waits
          +----------+   unbounded, never blocks        |
          | nodeQueue|                                   |
          +----+-----+                                   |
               v                                         |
          +----------+  handleNode (in order)   +--------+----------+
          | handler  |------------------------->| goSafe helpers    |
          +----------+                          | (post-login, QR,  |
                                                |  app state, ...)  |
          +----------+                          +-------------------+
          | keepalive|  ping every 20-30 s, 10 s timeout,
          +----------+  reconnect after 90 s without a pong
```

- **Reader** (`readLoop`): reads and decodes nodes. Responses (`iq` of type
  `result`/`error`, and `ack`) are handed to the waiting `sendIQ` caller by
  id. All other nodes go into the `nodeQueue`
  ([nodequeue.go](../internal/client/nodequeue.go)). The queue never blocks
  the reader: if it did, a handler waiting for an IQ response would never
  see it, because only the reader delivers responses. Undecodable frames are
  logged and skipped.
- **Handler** (`handleLoop`): pops nodes in arrival order and dispatches them
  by tag (`handleNode` in [handlers.go](../internal/client/handlers.go)):
  `success`, `iq`, `notification`, `ib`, `message`, `receipt`, `call`,
  `presence`, `chatstate`; stream errors and failures are classified into a
  disconnect action. A panic in one handler is logged and the next node is
  processed.
- **Keepalive** (`keepAliveLoop`): pings at a random interval between 20 and
  30 s and ends the session after 90 s without a successful ping, since a
  half-open TCP connection can otherwise look alive for a long time.
- **Helpers** started with `goSafe`: work that needs IQ round trips must not
  run on the handler goroutine. Examples: post-login setup (purge old
  buffers, prekey maintenance, `passive`/`active` IQ, then `Connected` and app
  state sync), the history sync worker, the QR rotator, prekey refill,
  app state sync after `server_sync`, retry receipts, recovery responses.
  `goSafe` tracks the goroutine in the session's wait group and turns a panic
  into a session end with `ActionReconnect` instead of a process crash.

`runSession` waits for all tracked goroutines before returning, then fails
any remaining IQ waiters with `ErrNotConnected`.

### Concurrency and locks

`Client.mu` guards the pointer to the current session, `sendMu` serializes
outgoing messages (ordering and device fan-out), `preKeyMu` serializes prekey
uploads, and `addrLocks` holds one lock per Signal address while a session
record is read, advanced and written. The IQ waiter map and every cache have
their own mutex.

`Config.HeavySlots` is a semaphore shared by all clients of a process: a
history sync chunk (download, inflate, thousands of rows) holds a slot while
it is processed. The manager sets it to 2 slots per node.

### Events

`Config.OnEvent` receives events synchronously on the handler goroutine
([events.go](../internal/client/events.go)). The callback must be fast and
must not call back into the client synchronously; `emit` recovers panics in
it. The event types are:

| Group | Events |
|---|---|
| Pairing | `QRCode`, `QRTimeout`, `PairSuccess`, `PairError` |
| Connection | `Connected`, `Disconnected`, `LoggedOut`, `StreamReplaced`, `ClientOutdated`, `TemporaryBan`, `KeepAliveTimeout` |
| Messages | `Message`, `UndecryptableMessage`, `Receipt`, `IdentityChanged` |
| Groups, presence, calls | `GroupUpdate`, `Presence`, `ChatPresence`, `CallEvent` |
| Sync | `AppStateChange`, `AppStateSyncComplete`, `AppStateUnverified`, `AppStateSyncError`, `HistorySync`, `AccountSync` |

### Caches

`ttlCache` ([ttlcache.go](../internal/client/ttlcache.go)) is a mutex-guarded
map with a per-entry TTL. It is bounded at 10,000 entries: when full, expired
entries are swept, and if all are still fresh the cache is cleared (which
only costs refetches).

| Cache | Key | TTL | Invalidated by |
|---|---|---|---|
| `devCache` | user | 10 min | `devices` notification, `account_sync` with `devices`, identity change |
| `groups` | group JID | 1 h | group notifications |
| `pictures` | user or group | 6 h (also caches "no picture") | `picture` notification |
| `profiles` | user | 10 min | `status` notification |
| `joined` | (single entry) | 2 min | |

`writtenCache` ([written.go](../internal/client/written.go)) remembers LID
mappings and push names already written, so the hot message path skips
upserts that would change nothing. Picture lookups are limited to 3 at once
and profile lookups to one at a time, with a hold-off after the server rate
limits them.

### Crash safety

The rule is: a stanza is acknowledged only after its effects are durable.
If the process dies before the ack, the server redelivers the stanza.

- **Event buffer.** Decrypting a 1:1 message reads the Signal session and
  the buffer row in one round trip, then writes the advanced session, the
  identity and the decrypted plaintext (`wax_event_buffer`) in one batch in
  the same transaction. A redelivered ciphertext finds its plaintext there
  instead of failing to decrypt against an already advanced ratchet. After
  the message is handled the plaintext is cleared; the row then only drops
  duplicates and is purged after 30 days. Group messages (`skmsg`) use the
  same buffer.
- **Dispatch before ack.** `dispatchMessage` stores app state key shares and
  history sync notifications from our own phone, stores the message, emits
  the event (the manager writes the webhook row in its callback), and only
  then is the delivery receipt sent, which doubles as the ack. If a key share
  or history notification cannot be stored, the stanza is left
  unacknowledged for redelivery. A failed write of the message row itself is
  logged and does not block the ack.
- **Deferred stanzas.** Stanzas wax cannot process yet (newsletters, bots,
  unknown notification types, unsupported `enc` types) are stored verbatim in
  `wax_deferred_stanzas` and acknowledged after the write, because the server
  delivers its offline queue in windows and stalls on unacknowledged stanzas.
  They are replayed in id order right after login, before new stanzas.
  Processing of newsletters and bots is not implemented.
- **Pairing.** The pairing result is stored before the confirmation is sent
  to the server.
- **Sent messages.** Outgoing plaintexts are kept in `wax_sent_messages` for
  7 days to answer retry receipts.

## Persistence

[internal/store](../internal/store) uses PostgreSQL through pgx v5
(`pgxpool`). `store.Open` parses nothing from the environment itself; it
receives the DSN and master key, connects, pings and applies migrations.

### Store API

`Store` wraps a pool or a transaction behind one `dbtx` interface, so every
method works both inside and outside a transaction:

- `InTx(ctx, fn)` runs `fn` in a transaction (nested calls reuse it; panic
  or error rolls back).
- `Batch(ctx, fn)` queues the writes made by `fn` into one `pgx.Batch` and
  sends them in one round trip; PostgreSQL runs a batch atomically. Reads
  inside a batch fail with `ErrBatchRead`. Batches are used for decryption
  state, prekey generation and lease state publishing.

### Migrations

Migrations are embedded SQL files in
[internal/store/migrations](../internal/store/migrations), applied in order
at startup. Each runs in its own transaction under a transaction-scoped
advisory lock, and applied versions are recorded in `wax_migrations`, so
several nodes can start against the same database at once. There are no
down migrations.

### Tables

| Table | Migration | Purpose | Sealed columns |
|---|---|---|---|
| `wax_devices` | 001, 005 | Identity of each linked device: registration id, keys, JID/LID after pairing, push name, NCT salt | `noise_key`, `identity_seed`, `signed_prekey`, `adv_secret`, `nct_salt` |
| `wax_prekeys` | 001 | One-time prekeys, with an `uploaded` flag | `private_key` |
| `wax_identities` | 001 | Identity keys of remote devices (trust on first use) | |
| `wax_sessions` | 002 | Signal session records per remote device address | `record` |
| `wax_lid_map` | 002 | Phone number user to LID user mapping | |
| `wax_event_buffer` | 002 | Decrypted payloads by ciphertext hash, for crash safety and duplicate detection | `plaintext` |
| `wax_sent_messages` | 002 | Recently sent messages, to answer retry receipts | `plaintext` |
| `wax_deferred_stanzas` | 003 | Stanzas stored before ack for later replay | `raw` |
| `wax_sender_keys` | 004 | Sender key chains per group and sender | `record` |
| `wax_sender_key_sent` | 004 | Devices that already have our sender key for a group | |
| `wax_appstate_keys` | 005 | App state sync keys shared by the phone | `key_data` |
| `wax_appstate_versions` | 005, 007 | Version, LTHash and `unverified` flag per collection | |
| `wax_appstate_macs` | 005 | Index MAC to value MAC of live records (to update the LTHash) | |
| `wax_contacts` | 005, 011 | Contact names, push names and usernames | |
| `wax_chats` | 005, 013 | Chats with settings (archived, pinned, muted; `settings_at` when app state last set them) and history metadata | |
| `wax_messages` | 005, 013 | Messages from live traffic and history sync; revokes and edits are applied (`edited_at`) | `content` |
| `wax_history_queue` | 005 | History sync notifications waiting to be downloaded | `notification` |
| `wax_instances` | 006, 008 | Instance configuration: tenant, name, webhook, send rate | `webhook_secret` |
| `wax_outbox` | 006 | Durable send queue | `payload` |
| `wax_webhook_queue` | 006 | Durable webhook queue | `payload` |
| `wax_tenants` | 008, 010 | Tenants with `max_instances` and `requests_per_second` | |
| `wax_api_keys` | 008 | Tenant API keys (HMAC of the secret only) | |
| `wax_nodes` | 009 | Nodes of the cluster with address and heartbeat | |
| `wax_instance_leases` | 009 | Which node runs an instance until when, plus published status | |
| `wax_privacy_tokens` | 012 | Privacy ("trusted contact") tokens received and issued | `token` |

Device-scoped tables reference `wax_devices` with `ON DELETE CASCADE`, so a
logout (which deletes the device row) removes all Signal and sync state.
Queue and lease tables reference `wax_instances`. Contact names, chat
metadata and message metadata (sender, time, type) are stored in plaintext;
only message content is sealed.

### Sealing

[internal/store/sealer.go](../internal/store/sealer.go) encrypts secrets with
AES-256-GCM before they are written:

- The master key (`WAX_MASTER_KEY`, 32 bytes, base64) is never used directly.
  Three independent subkeys are derived with HKDF-SHA256 under separate
  labels: the sealing key (`wax/store/secrets/v1`), the API key HMAC key
  (`wax/store/api-keys/v1`) and the node forwarding key
  (`wax/node-forward/v1`).
- A sealed value is `version (1 byte) || nonce (12 random bytes) ||
  ciphertext`. The version byte is reserved for a future key rotation;
  rotation itself is not implemented.
- The GCM associated data binds each value to its location:
  `table|column|instance|rowkey...` (for example a session record is bound to
  the instance and the Signal address). A ciphertext copied into another row,
  column or instance fails to open with `ErrSealed`. This is also why
  instance ids can never be renamed.
- Where the row key is a generated id (outbox, webhook queue, history queue),
  the row is inserted first and the sealed payload is written in the same
  transaction once the id is known.

Losing the master key means every instance must be paired again.

## Multi-instance and multi-tenant

### Manager and instances

[internal/manager](../internal/manager) owns the instances a node runs. An
`Instance` ([instance.go](../internal/manager/instance.go)) has three
goroutines, each with panic recovery so a failure stays inside one instance:

- `runLoop` creates a client and calls `Run`. When it returns, the error is
  mapped to a status and a wait: logged out (new pairing after 1 s), QR
  timeout and replaced (wait for `POST /connect`), outdated (10 min), banned
  (1 h), anything else (30 s).
- `outboxLoop` sends queued messages.
- `deliveryLoop` delivers webhooks.

The internal instance id is `tenant:name`; tenants only ever see the name.
`Manager.Resolve` looks an instance up by tenant and name, so an instance of
another tenant is simply not found. `Manager.Views` builds listings from the
database (configuration plus the status published by the owning node), and
overrides status for instances running locally. `onEvent` updates the
status, kicks the outbox on `Connected` and converts events to webhooks.

### Tenants and API keys

Tenants ([internal/store/tenants.go](../internal/store/tenants.go)) carry two
limits: `max_instances` (0 means no quota) and `requests_per_second` (0 means
the node default). The quota is enforced in `CreateInstance` by locking the
tenant row (`FOR UPDATE`) and counting, so concurrent creates cannot exceed
it. A tenant can only be deleted once it has no instances.

Tenant keys have the form `wax_<id>_<secret>` (8-byte id in hex, 32-byte
secret in base64url). Only `HMAC(keyMAC, id || secret)` is stored; the
plaintext key is returned once on creation. Every request looks the key up by
id and compares the MAC in constant time, so revocation is immediate.

### Leases across nodes

Several `wax serve` processes can share one database. Each instance runs on
exactly one node, decided by a lease ([lease.go](../internal/manager/lease.go),
[internal/store/leases.go](../internal/store/leases.go)). All expiry checks
use the database clock (`now()`), so node clocks do not matter.

Leases last 30 s (`Timing.LeaseTTL`). A round runs every TTL / 3 (10 s),
and between rounds a node below its share polls for free leases every
min(2 s, round / 3).

One lease goroutine per node runs each round:

1. **Heartbeat**: upsert the node in `wax_nodes` with its advertised address.
2. **Renew**: extend every unexpired lease of this node. An expired lease is
   never renewed. Instances whose lease was not renewed are stopped at once
   ("lost").
3. **Fence**: if the database cannot be reached and the last successful
   renewal is older than TTL minus one round, all local instances are
   stopped before their leases can expire and another node takes over.
4. **Balance**: the fair share is `ceil(instances / live nodes)`, capped by
   `-max-instances`. A node above its share releases the surplus (highest
   ids first); a node below it acquires free leases.
5. **Publish**: changed statuses and QR flags are written to the lease rows
   in one batch, so any node can answer listings.

`AcquireLeases` selects instances without a live lease using
`FOR UPDATE ... SKIP LOCKED` and only overwrites a lease whose `expires_at`
has passed. A node that shuts down releases its leases and deletes its node
row, so another node picks up the instances within the claim interval. After
a crash the takeover waits for expiry. A node that freezes longer than a
lease (for example a suspended VM) and then resumes may briefly overlap with
the new owner; there is no fencing token on the WhatsApp side, and WhatsApp
then ends one connection as "replaced".

### Request forwarding

Any node can receive any request. The `scoped` wrapper in
[api/api.go](../api/api.go) resolves the instance; if it does not run on this
node and the request was not already forwarded, it looks up the lease owner
and proxies the request there ([api/forward.go](../api/forward.go)) with
`httputil.ReverseProxy`.

The proxy sets `X-Wax-Forwarded: <node>;<unix time>;<hex mac>`, where the MAC
is HMAC-SHA256 with the node forwarding subkey over node id, timestamp,
method and request URI. The receiving node accepts it only within 30 s of
its own clock. A valid signature means the request is not forwarded again
and not counted against the tenant rate limit a second time. The
`Authorization` header is forwarded and checked again on the owner. Traffic
between nodes is plain HTTP to the `-advertise` address unless TLS is put in
front; keep that network private.

### Outbox

`wax_outbox` ([outbox.go](../internal/manager/outbox.go)) is the durable send
queue. The API inserts the row before anything is sent, with the WhatsApp
message id fixed at enqueue time, so a retry cannot create a duplicate
message. An optional `client_id` makes the enqueue idempotent (unique index
per instance).

The outbox loop of an instance runs only while the client is logged in. It
takes the oldest due item that heads its chat's queue (`NextOutbox`) and
waits for a token bucket
that paces sends at the instance's `rate_per_minute` (default 20, burst 3).
Success marks the row `sent`; permanent errors (invalid item or JID,
unsupported chat, server rejection, recipient not on WhatsApp) or the 10th
attempt mark it `failed`; both emit a `send_status` webhook. Other errors
schedule a retry with backoff (5 s up to 30 min), and a disconnect during a
send does not count as an attempt. Messages to one chat go out in order: an
item waiting for its retry holds back later items to the same chat, but not
to other chats (migration 013 indexes pending items per chat).

### Webhooks

Webhook events are written to `wax_webhook_queue` from the client's handler
goroutine (bounded to 5 s), before the stanza that caused them is
acknowledged ([webhook.go](../internal/manager/webhook.go)).

- **Ordering**: deliveries per instance are strictly ordered. A failing head
  blocks later events until it is delivered or given up.
- **Signing**: the body is a JSON envelope `{event, instance, timestamp,
  data}`. With a secret, `X-Wax-Signature: sha256=<hex HMAC-SHA256(secret,
  body)>` is added, along with `X-Wax-Event`, `X-Wax-Delivery` and
  `X-Wax-Instance`.
- **Retries**: any non-2xx answer or network error is retried with backoff
  from 5 s to 4 h for 24 hours after the event was queued (at most 50
  attempts), then marked `dead`.
  Redirects are not followed. Delivered and dead rows are purged after 7
  days.
- **Retargeting**: when the webhook URL changes, pending events become due at
  once; when it is removed, they are marked dead.

### Resource isolation

| Resource | Limit | Where |
|---|---|---|
| API requests | Token bucket per tenant, rate `requests_per_second` (default 20 via `-tenant-rps`), burst two seconds' worth; `429` with `Retry-After`. Per node | [api/limits.go](../api/limits.go) |
| Instances | `max_instances` per tenant, `403` when reached | `store.CreateInstance` |
| Large request bodies (1 MiB or more, or unknown length) | 4 at once per node, at most half per tenant | `largeSlots` in [api/limits.go](../api/limits.go) |
| Request body size | 90 MiB | [api/api.go](../api/api.go) |
| History sync processing | 2 chunks at once per node, across all instances | `HeavySlots` |
| Sends | `rate_per_minute` per instance | outbox token bucket |
| Database connections | `pool_max_conns` in the DSN (pgx default max(4, CPUs)) | pgxpool |

### Metrics

[internal/metrics](../internal/metrics) writes the Prometheus text format
with the standard library only (a nil registry is a no-op). With
`-metrics-listen`, a separate unauthenticated server exposes `/metrics`:
instance status, messages, decrypt failures, disconnects, queue depths,
webhook deliveries, lease events, pool and runtime gauges, and API requests
by route pattern (never by id). The list is in the
[README](../README.md#metrics).

## API and admin interface

The API ([api](../api)) is a Go 1.22 `ServeMux`. `routeTable` in
[api/api.go](../api/api.go) lists every authenticated route with its role:

| Routes | Role |
|---|---|
| `GET /healthz`, `GET /openapi.json`, `GET /ui/...`, `GET /` (redirect) | none |
| `/v1/tenants...` (create, list, get, update, delete, keys) | admin key (`WAX_API_KEY`) |
| `/v1/instances...` (instances, QR, messages, outbox, chats, media, pictures, profiles, groups, read, presence, calls) | tenant key |

The admin key is compared in constant time and only works on admin routes;
a tenant key only works on tenant routes. Every handler that acts on one
instance receives it from `scoped`, which resolves it within the caller's
tenant. Each request runs with panic recovery. Errors from WhatsApp or the
media servers are logged and returned as a generic `502`, because they can
contain protocol details and signed CDN paths.

The OpenAPI 3.1 document ([api/openapi.json](../api/openapi.json)) is
embedded and served. `TestOpenAPIContract` in
[api/openapi_test.go](../api/openapi_test.go) fails when it and the code
disagree on routes, key types, JSON field names or webhook events.

The admin interface ([internal/webui](../internal/webui)) is plain HTML, CSS
and JavaScript embedded in the binary, with no build step. It calls the API
of the same origin with the key the user signs in with, kept in
`sessionStorage`. Every response carries a strict Content-Security-Policy
(`default-src 'none'`, scripts, styles and connections only from `'self'`,
images and media also from `blob:`, no framing, no forms), plus `nosniff`,
`no-referrer` and `X-Frame-Options: DENY`. `TestNoUnsafeDOM` checks that the
scripts do not insert server data as HTML.

Endpoint details and examples are in [docs/usage/api.md](usage/api.md).

## Security model

### Secrets and where they live

The environment holds secrets only: `WAX_MASTER_KEY` (root of all sealing and
MAC keys), `WAX_API_KEY` (admin bearer token, at least 32 characters, serve
mode only) and `WAX_DATABASE_URL` (DSN; defaults to the docker compose
database when empty). Everything else is a command line flag.

In the database, private keys, Signal and sender key records, app state keys,
message contents, queued payloads, webhook secrets, deferred stanzas and
privacy tokens are sealed, so a dump without the master key does not reveal
them. API keys are stored as HMACs only. Contact names, chat lists, message
metadata and JIDs are not encrypted.

### Network protections

- **Webhook SSRF.** The webhook HTTP client checks the resolved IP at connect
  time (so DNS rebinding cannot bypass it) and refuses loopback, private,
  link-local (including cloud metadata), multicast, unspecified and CGNAT
  (100.64.0.0/10) addresses. Proxies are disabled because they would bypass
  the check, and redirects are not followed. `-allow-private-webhooks` turns
  the address check off. Webhook URLs must be absolute http(s) URLs without
  credentials.
- **Media downloads.** Download hosts come from the server's `media_conn`
  answer, never from the message; the message only supplies a direct path,
  which must start with `/`. Profile pictures are fetched only from HTTPS
  URLs on `whatsapp.net` or its subdomains, are size-limited and must sniff
  as an image.
- **Inter-node requests** are signed (see Request forwarding) but not
  encrypted.
- **Metrics** have no authentication and must stay on an internal address.

### Logging hygiene

Logs are JSON on stderr. The client logs JIDs through `wabin.MaskJID`, which
keeps only the last four characters of the user part; node dumps
(`-dump-nodes` in `wax run`) are masked unless explicitly disabled. Signed
media and picture URLs are stripped from errors before they are logged.
The code does not log keys, webhook secrets or message contents.

The API keeps an access log (`logRequest` in [api/api.go](../api/api.go)):
every request with its route pattern, status, duration, client address and
caller (`admin` or the tenant key's public id), at `info` for changes and
`debug` for reads; paths are left out because they may contain chat
addresses. Deleting an instance also logs `instance deleted`.

## Testing

```bash
go test -race ./...                                       # no database: DB tests skip
WAX_TEST_DATABASE_URL=postgres://... go test -race ./...  # separate test database
```

- **Without a database**: wire formats, crypto, Signal, app state, pairing,
  rate limiting, lease arithmetic (`TestFairShare`, `TestFenceIfStale`),
  webhook signing, the web UI CSP and DOM checks, and the OpenAPI contract
  test.
- **With `WAX_TEST_DATABASE_URL`**: the store tests
  ([internal/store](../internal/store)), the client end-to-end tests built on
  the harness in [internal/client/session_test.go](../internal/client/session_test.go),
  and the API tests built on the setup in [api/api_test.go](../api/api_test.go)
  (end-to-end, tenant isolation, limits, cluster, compose, OpenAPI served).
- **fakewa** ([internal/testutil/fakewa](../internal/testutil/fakewa)) is an
  in-process WebSocket server that performs the server side of the Noise
  handshake with a test certificate chain, exposes the decrypted node stream
  to the test, and plays the phone during QR pairing. Client tests use it for
  pairing, the 515 restart, login, prekey upload, logout and keepalive
  timeouts; API tests for full flows with webhooks, retries, outbox and
  idempotency; cluster tests run several managers on one database to check
  forwarding, failover and balancing.
- **Contract test**: `TestOpenAPIContract` keeps
  [api/openapi.json](../api/openapi.json) in sync with the code.

No test talks to the real WhatsApp servers.
