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
| 2 | QR pairing, persistent identity (PostgreSQL) | — |
| 3 | Signal protocol, 1:1 text messages | — |
| 4 | Groups, media, receipts, presence, calls | — |
| 5 | History sync, app state sync | — |
| 6 | Multi-instance, REST API, webhooks, queue | — |

## Layout

```
cmd/wax-probe        Phase 1 tool: handshake + node logging
internal/keys        X25519 key pairs, libsignal-compatible Curve25519 signatures
internal/transport   framing, WebSocket, Noise XX handshake, cert chain, encrypted conn
internal/binary      binary XML nodes: tokens, JIDs, encoder/decoder, masked formatting
internal/proto       protobuf schemas + generated code (see internal/proto/README.md)
internal/waversion   client version, live version detection
internal/client      client payloads, disconnect classification, node connection
```

## Requirements

- Go 1.24+ (for `crypto/hkdf`)
- protoc + protoc-gen-go, only when changing `.proto` files

## Try it

```bash
go test -race ./...
go run ./cmd/wax-probe -debug -duration 60s
```

The probe connects as a new, unpaired device with throwaway keys, completes
the handshake and logs every node. Binary payloads and JIDs are masked
unless you pass `-unmask`.
