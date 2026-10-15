# wax

A WhatsApp Web multi-device client written from scratch in Go. It links to a
phone as a "linked device" via QR code and will expose a REST API.

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
| 5 | History sync, app state sync | — |
| 6 | Multi-instance, REST API, webhooks, queue | — |

## Layout

```
cmd/wax             runs one linked-device instance (QR pairing, reconnects)
cmd/wax-probe       Phase 1 tool: raw handshake + node logging
internal/keys        X25519 key pairs, libsignal-compatible Curve25519 signatures
internal/signal      Signal protocol v3: X3DH, Double Ratchet, Sender Keys, wire formats (pure, no I/O)
internal/media       media encryption (HKDF keys, AES-CBC + HMAC), upload/download
internal/transport   framing, WebSocket, Noise XX handshake, cert chain, encrypted conn
internal/binary      binary XML nodes: tokens, JIDs, encoder/decoder, masked formatting
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

## Running

```bash
docker compose up -d                      # PostgreSQL on 127.0.0.1:5433
cp .env.example .env                      # then set WAX_MASTER_KEY=$(openssl rand -base64 32)
set -a; source .env; set +a
go run ./cmd/wax -instance test           # shows a QR code on first start
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
prekey upload, logout, keepalive timeout).
