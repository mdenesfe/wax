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
cp .env.example .env                      # then set WAX_MASTER_KEY=$(openssl rand -base64 32)
set -a; source .env; set +a
WAX_API_KEY=$(openssl rand -hex 24) go run ./cmd/wax serve
```

Environment: `WAX_DATABASE_URL`, `WAX_MASTER_KEY`, `WAX_API_KEY` (at least 32
characters), `WAX_LISTEN` (default `127.0.0.1:8080`), `WAX_LOG_LEVEL`
(`debug|info|warn|error`). Logs are JSON on stderr; SIGINT/SIGTERM shut down
gracefully.

Every `/v1` request needs `Authorization: Bearer $WAX_API_KEY`.

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
