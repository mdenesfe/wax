# wax

An open implementation of the WhatsApp Web multi-device protocol, written
from scratch in Go, with documentation of how the protocol works.

wax links to a phone as a companion ("linked") device, the way WhatsApp Web
does, and speaks the protocol end to end: the Noise handshake, binary XML
stanzas, QR pairing, Signal encryption for 1:1 and group messages, app state
and history sync, media encryption, users, LIDs, groups and communities. On
top of it, a server runs many accounts behind a REST API with signed
webhooks, and an admin interface makes it easy to use.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/chat-dark.png">
  <img alt="wax admin: an account opened as a WhatsApp Desktop-like window" src="docs/images/chat-light.png">
</picture>

<sub>Screenshots use made-up data from <code>cmd/wax-demo</code>.</sub>

## Why

The protocol behind WhatsApp Web is not published. wax aims to be a clear,
readable implementation of it and a written explanation of every layer, so
that others can learn from it and build on it:

- **[The protocol, documented](docs/protocol/README.md):** ten chapters,
  from the transport to media, each linked to the code that implements it.
- **No WhatsApp libraries.** Everything is written here. Cryptography comes
  from the Go standard library only (`crypto/ecdh`, `crypto/ed25519`,
  `crypto/aes`, `crypto/cipher`, `crypto/hkdf`, `crypto/sha256`).
- **Tested against a fake server.** The client runs end to end against an
  in-process fake WhatsApp server: pairing, login, messages, groups, media,
  app state and history.

## What is implemented

| Layer | Scope |
|---|---|
| Transport | WebSocket framing, Noise XX handshake, server certificate check, keepalive, reconnect with backoff |
| Stanzas | Binary XML encoding (tokens, JIDs, packed strings), IQs, acks, receipts, notifications |
| Pairing | QR pairing, ADV signature verification, prekeys, login, logout |
| Encryption | Signal X3DH, Double Ratchet and sender keys, retry receipts |
| Messages | Text, replies, mentions, forwards and media in both directions; reactions, view once and system messages received; read receipts, presence, call rejection |
| People | Phone number and LID addressing, device lists, profiles, pictures, privacy tokens |
| Groups | Metadata, members, sending, communities |
| Sync | App state (LTHash verified) and history sync |
| Media | Encryption, upload and download; WebM to Ogg Opus rewrap for voice notes |
| Server | Many accounts per process, tenants and API keys, several nodes with leases, durable send queue, signed webhooks, metrics, OpenAPI |
| Admin interface | A WhatsApp Desktop-like web interface over the API |

What is not implemented yet is listed at the end of each protocol chapter.

## Quick start

Try the admin interface with made-up data, no WhatsApp account needed:

```bash
go run github.com/mdenesfe/wax/cmd/wax-demo@latest   # http://127.0.0.1:8090, any key signs in
```

Run wax with Docker (the server and its PostgreSQL):

```bash
git clone https://github.com/mdenesfe/wax && cd wax
cp .env.example .env     # set WAX_MASTER_KEY (openssl rand -base64 32) and WAX_API_KEY (openssl rand -hex 32)
docker compose up -d     # http://127.0.0.1:8080
```

Open the admin interface, sign in with `WAX_API_KEY`, create a tenant and
a key, sign in with that key, add a WhatsApp account and scan its QR code
from the phone (Settings, Linked devices). Then use the REST API from any
language. The [getting started guide](docs/usage/getting-started.md) walks
through it, including the API and webhooks.

## Ways to use wax

| | |
|---|---|
| **Server with REST API and webhooks** | `docker compose up -d`, the image `ghcr.io/mdenesfe/wax`, or the programs on the [releases page](https://github.com/mdenesfe/wax/releases) (`wax serve`); or `go install github.com/mdenesfe/wax/cmd/wax@latest`. Needs PostgreSQL. |
| **Go library** | `go get github.com/mdenesfe/wax`: link an account and handle its messages in your own Go program. See [using wax as a library](docs/usage/library.md) and [examples/echo](examples/echo/main.go). |
| **Learning the protocol** | Read [docs/protocol](docs/protocol/README.md) next to the code. |

## Documentation

- **Protocol:** [overview](docs/protocol/README.md) and chapters
- **Using wax:** [getting started](docs/usage/getting-started.md),
  [REST API](docs/usage/api.md), [webhooks](docs/usage/webhooks.md),
  [operations](docs/usage/operations.md),
  [admin interface](docs/usage/admin-ui.md),
  [Go library](docs/usage/library.md)
- **Inside wax:** [architecture](docs/architecture.md)

The full API contract is served at `GET /openapi.json`.

## Layout

```
wax.go               the Go library: github.com/mdenesfe/wax
proto                WhatsApp protobuf schemas and generated code (public)
examples/echo        a minimal program using the library
cmd/wax              `wax serve` (API server), `wax run` (one account on the console), `wax version`
cmd/wax-demo         the admin interface with made-up data
cmd/wax-probe        raw handshake and node logging
api                  REST API, tenants, node forwarding, OpenAPI
internal/transport   framing, WebSocket, Noise XX handshake, certificate chain
internal/wabin       binary XML nodes: tokens, JIDs, encoder and decoder
internal/pairing     QR content and rendering, pair-success verification
internal/keys        X25519 key pairs and Curve25519 signatures
internal/signal      Signal protocol: X3DH, Double Ratchet, sender keys (no I/O)
internal/appstate    app state: LTHash, mutation keys and MACs, patches and snapshots
internal/media       media encryption, upload and download, WebM to Ogg Opus
internal/client      the session: connect loop, handlers, sending, sync, groups, profiles
internal/store       PostgreSQL persistence; secrets sealed with WAX_MASTER_KEY
internal/manager     accounts, leases, send queue, webhooks, metrics
internal/webui       the embedded admin interface
internal/testutil    the fake WhatsApp server used by the tests
scripts              documentation screenshots
```

## Building and testing

Needs Go 1.25 or newer and Docker (for PostgreSQL); `protoc` and
`protoc-gen-go` only when changing `.proto` files.

```bash
docker compose up -d postgres        # the database only, on 127.0.0.1:5433
go build ./cmd/wax
go test -race ./...                  # database tests are skipped
WAX_TEST_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax_test?sslmode=disable go test -race ./...
```

The second test run also covers the store, the end-to-end client tests
against the fake WhatsApp server and the API. See
[CONTRIBUTING.md](CONTRIBUTING.md) to contribute and
[SECURITY.md](SECURITY.md) to report a vulnerability.

## Disclaimer

wax is an independent project. It is not affiliated with, endorsed by or
connected to WhatsApp or Meta. "WhatsApp" is a trademark of its owner and
is used here only to describe what the software interoperates with.

Using an unofficial client may break WhatsApp's terms of service, and
WhatsApp may restrict or ban accounts that use one. Use wax responsibly:
only with accounts you own, with the consent of the people you message, and
never for spam or bulk unsolicited messages. The protocol can change at any
time, and wax may stop working until it is updated.

## License

[Apache License 2.0](LICENSE)
