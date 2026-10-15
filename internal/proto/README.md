# internal/proto

WhatsApp protobuf schemas and their generated Go code.

## Layout

One directory per WhatsApp Web protobuf module:

| Directory        | WhatsApp Web module   | Contents                                         |
|------------------|-----------------------|--------------------------------------------------|
| `wa6`            | `WAWebProtobufsWa6`   | Noise handshake messages, `ClientPayload`        |
| `wacert`         | `WACert`              | Noise server certificate chain                   |
| `wacompanionreg` | `WACompanionReg`      | `DeviceProps`, `ClientPairingProps`               |
| `waadv`          | `WAAdv`               | Pairing identity attestation (ADV)               |
| `wae2e`          | `WAWebProtobufsE2E`   | Message content, protocol messages               |
| `waserversync`   | `WAWebProtobufsServerSync` | App state patches/snapshots                  |
| `wasyncaction`   | `WAWebProtobufSyncAction` (+ `SyncdSnapshotRecovery`) | App state values   |
| `wahistorysync`  | `WAWebProtobufsHistorySync` (+ `WebMessageInfo`) | History sync payload    |
| `signalwire`     | libsignal `WhisperTextProtocol` | Signal wire messages                   |
| `signalstore`    | (ours)                | Signal session storage format                    |

These are **subsets** of the official schemas. Only fields this project reads
or writes are declared. Undeclared fields received from the server are kept as
unknown fields by protobuf-go, so they never fail decoding and are re-emitted
unchanged if the message is re-serialized.

## Regenerating Go code

```bash
brew install protobuf        # protoc
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
./internal/proto/generate.sh
```

The generated `*.pb.go` files are committed, so building the project does not
need protoc.

## Updating schemas when WhatsApp changes

WhatsApp Web ships its protobuf definitions inside its JavaScript bundles. The
authoritative source is always the current WhatsApp Web client. To add or
verify a field:

1. Find the message in the current WhatsApp Web bundle. Open
   `https://web.whatsapp.com` in a browser, search the loaded JS sources for
   the module name (e.g. `WAWebProtobufsWa6.pb`), and read the field
   `{name: [number, flags, type]}` specs.
2. Cross-check against a maintained open-source extraction. whatsmeow's
   `proto/` directory is regenerated from WhatsApp Web regularly. Read it as a
   reference only.
3. Add the field to the `.proto` file here with **exactly** the same field
   number and wire type. Never renumber or reuse a number.
4. Run `./internal/proto/generate.sh`, then `go build ./... && go test ./...`.
5. In the commit message, note the WhatsApp Web version the change was
   verified against.

Field *names* in this directory are ours. Only numbers and wire types have to
match WhatsApp.
