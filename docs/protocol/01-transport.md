# 01. Transport: WebSocket, Noise and the connection lifecycle

wax talks to WhatsApp over a single WebSocket. Every message on that socket is a length-prefixed frame. The first three frames carry a Noise `XX` handshake (Curve25519, AES-256-GCM, SHA-256) in which the server proves its identity with a certificate chain and the client sends its `ClientPayload` (a login for a paired device, or a registration for a new one). After the handshake every frame is AES-GCM encrypted with a per-direction key and an implicit counter nonce. On top of that stream wax exchanges binary XML nodes (see [02-binary-xml.md](02-binary-xml.md)), keeps the connection alive with pings, classifies server-initiated disconnects and reconnects with exponential backoff. This document describes exactly what the code in `internal/transport` and `internal/client` does.

## Layering

```
+-------------------------------------------+
| stanzas (iq, message, receipt, ...)       |  internal/client
+-------------------------------------------+
| binary XML node + flags byte              |  internal/wabin
+-------------------------------------------+
| AES-256-GCM, counter nonce (NoiseConn)    |  internal/transport/conn.go
+-------------------------------------------+
| 3-byte length-prefixed frames (+header)   |  internal/transport/frame.go
+-------------------------------------------+
| binary WebSocket messages                 |  internal/transport/websocket.go
+-------------------------------------------+
```

`client.Connect()` in `internal/client/conn.go` wires these together: `transport.Dial()`, then `transport.ClientHandshake()`, returning a `*client.Conn` with `SendNode()` and `ReadNode()`.

## WebSocket endpoint

| Setting | Value | Where |
|---|---|---|
| URL | `wss://web.whatsapp.com/ws/chat` | `transport.DefaultURL` |
| `Origin` header | `https://web.whatsapp.com` | `transport.DefaultOrigin` |
| Message type | binary only; text messages are ignored | `WSConn.ReadFrame()` |
| Read limit | `MaxFrameSize + 64` bytes per WebSocket message | `transport.Dial()` |

The URL can be overridden (`Config.URL`, `ConnectOptions.URL`) and an `*http.Client` can be supplied, for example to use a proxy. No other HTTP headers are set. The WebSocket library is `github.com/coder/websocket`.

## Frames

Every unit sent or received is a frame: a 3-byte big-endian length followed by that many bytes of payload (`appendFrame()` in `internal/transport/frame.go`). The largest payload is therefore `2^24 - 1` bytes (`MaxFrameSize`); larger payloads fail with `ErrFrameTooLarge`.

```
+--------+--------+--------+----------------------+
| len>>16| len>>8 |  len   | payload (len bytes)  |
+--------+--------+--------+----------------------+
```

Outgoing: each frame is written as exactly one binary WebSocket message (`WSConn.SendFrame()`).

Incoming: wax does not rely on WebSocket message boundaries. `frameSplitter.push()` buffers received bytes and emits every complete frame, so one message may carry several frames and one frame may span several messages. The test `TestFrameSplitter` feeds the same byte stream in chunks of 1, 2, 3, 5 and 1000 bytes and expects identical frames.

### Connection header (prologue)

The very first WebSocket message is prefixed with a 4-byte connection header, sent once and never again (`WSConn.prologue` is cleared after the first successful write):

```
57 41 06 03
'W''A' |  |
       |  +-- token dictionary version (wabin.DictVersion = 3)
       +----- protocol magic 6
```

It is defined as `client.ConnHeader` in `internal/client/conn.go`. The same bytes are also the Noise prologue (see below), so if the server received a different header, decryption of `ServerHello` would fail.

The comment on `DialOptions.Prologue` mentions an optional edge-routing header that may precede the connection header. wax never sends one; it always uses the plain 4-byte header.

So the first message on the wire looks like:

```
57 41 06 03 | LL LL LL | <HandshakeMessage{clientHello}> protobuf
```

## Noise handshake

`transport.ClientHandshake()` in `internal/transport/handshake.go` runs the initiator side of the `XX` pattern:

```
-> e
<- e, ee, s, es        (+ encrypted certificate chain)
-> s, se               (+ encrypted ClientPayload)
```

Handshake messages are protobuf `HandshakeMessage` values (`proto/wa6/wa6.proto`), each sent as one plain (unencrypted) frame:

| Step | Field | Contents |
|---|---|---|
| 1, client | `clientHello.ephemeral` | client ephemeral public key (32 bytes) |
| 2, server | `serverHello.ephemeral` | server ephemeral public key (32 bytes, cleartext) |
| | `serverHello.static` | server static key, encrypted |
| | `serverHello.payload` | serialized `CertChain`, encrypted |
| 3, client | `clientFinish.static` | client static Noise key, encrypted |
| | `clientFinish.payload` | serialized `ClientPayload`, encrypted |

wax rejects a `ServerHello` whose ephemeral is not 32 bytes or whose static or payload field is empty.

### Symmetric state

`internal/transport/noise.go` implements the Noise SymmetricState:

- **Protocol name**: `Noise_XX_25519_AESGCM_SHA256` padded with four NUL bytes to exactly 32 bytes (`NoiseProtocolName`). Because it is 32 bytes it is used directly as the initial hash `h`, not hashed. The chaining key `ck` starts equal to `h`.
- **Prologue**: `mixHash(prologue)` immediately after initialization, with the connection header `57 41 06 03`.
- **MixHash**: `h = SHA-256(h || data)`.
- **MixKey**: `HKDF-SHA256(salt = ck, ikm = dh_output, info = "")`, 64 bytes. The first 32 become the new `ck`, the last 32 key a fresh AES-256-GCM cipher, and the nonce counter resets to 0.
- **DH**: X25519 (`keys.DH()`).
- **EncryptAndHash / DecryptAndHash**: AES-GCM with associated data `h`; the ciphertext is then mixed into `h`.
- **Split**: `HKDF-SHA256(salt = ck, ikm = empty, info = "")`, 64 bytes. The first 32 bytes key client-to-server traffic, the last 32 server-to-client.

The nonce is 12 bytes: eight zero bytes followed by a 32-bit big-endian counter (`nonceFor()`). This differs from textbook Noise (4 zero bytes and a 64-bit counter). `TestNonceLayout` pins it:

```
nonceFor(0x01020304) = 00 00 00 00 00 00 00 00 01 02 03 04
```

No cipher exists before the first `mixKey`; in `XX` the first encryption always follows a DH, so the unused "key from h" step is skipped.

### Order of operations (client)

1. Generate an ephemeral key pair, `mixHash(e.pub)`, send `clientHello`.
2. Read `serverHello`. `mixHash(server_e)`, `mixDH(e.priv, server_e)` (ee).
3. `decryptAndHash(static)` gives the server static key (must be 32 bytes). `mixDH(e.priv, server_s)` (es).
4. `decryptAndHash(payload)` gives the certificate chain; verify it (next section).
5. `encryptAndHash(noise_key.pub)`, then `mixDH(noise_key.priv, server_e)` (se).
6. `encryptAndHash(client_payload)`, send `clientFinish`.
7. `split()` and wrap the frame connection in a `NoiseConn`.

The client's static key is the per-device **Noise key**, generated once and persisted with the device (`store.Device.NoiseKey`). It is the same key that is shown in the pairing QR code.

The whole exchange is bounded by `DefaultHandshakeTimeout` (20 s). The timeout closes the connection instead of cancelling a read context. On top of that, `runSession()` gives dial plus handshake a combined `connectTimeout` of 30 s. Failures are returned as `*HandshakeError{Step, Err}`, which matches both `ErrHandshake` and the cause with `errors.Is`.

A responder implementation, `transport.ServerHandshake()` in `internal/transport/server.go`, exists only for tests and local simulation.

## Server certificate verification

`CertVerifier.Verify()` in `internal/transport/cert.go` checks the decrypted `serverHello.payload`, a protobuf `CertChain` (`proto/wacert/wacert.proto`):

```
CertChain
  leaf          NoiseCertificate { details: bytes, signature: bytes }
  intermediate  NoiseCertificate { details: bytes, signature: bytes }

Details { serial, issuerSerial, key (32 bytes), notBefore, notAfter }
```

Checks, in order:

1. The intermediate's signature over its raw `details` bytes verifies under the pinned root key (`WhatsAppRootKey`). Signatures are 64-byte XEdDSA-style Curve25519 signatures (`keys.VerifySlice()`). The details are decoded only after the signature passes.
2. The intermediate's `issuerSerial` equals the root serial (`WhatsAppRootSerial` = 0).
3. The intermediate is within `notBefore`/`notAfter` (Unix seconds). A zero or absent bound is treated as unbounded.
4. The leaf's signature verifies under the intermediate's `key`.
5. The leaf's `issuerSerial` equals the intermediate's `serial`.
6. The leaf is within its validity window.
7. The leaf's `key` equals the server static key from step 3 of the handshake (constant-time compare). Without this a man in the middle holding any valid chain could use its own static key.

Every failure wraps `ErrCertificate` and aborts the handshake at step `"verify certificate"`. `Config.Verifier` can replace the verifier (used by tests with a fake PKI).

## ClientPayload

`internal/client/payload.go` builds the protobuf `wa6.ClientPayload`. `Client.payloadFor()` picks the variant: login if the stored device is paired, registration otherwise.

Fields common to both (`basePayload()`):

| Field | Value |
|---|---|
| `userAgent.platform` | `WEB` |
| `userAgent.releaseChannel` | `RELEASE` |
| `userAgent.appVersion` | primary/secondary/tertiary of the WhatsApp Web version (see "Version detection") |
| `userAgent.mcc`, `mnc` | `"000"`, `"000"` |
| `userAgent.osVersion`, `osBuildNumber` | `"0.1"` |
| `userAgent.manufacturer` | `""` |
| `userAgent.device` | `"Desktop"` |
| `userAgent.localeLanguageIso6391`, `localeCountryIso31661Alpha2` | from `DeviceConfig` (defaults `en`, `US`) |
| `webInfo.webSubPlatform` | `WEB_BROWSER` |
| `connectType` | `WIFI_UNKNOWN` |
| `connectReason` | `USER_ACTIVATED` |

**Login** (`LoginPayload()`): `username` is the numeric phone-number user of the device JID (parsed as `uint64`), `device` is its device index, `passive = true`, `pull = true`.

**Registration** (`RegistrationPayload()`): `passive = false`, `pull = false`, and `devicePairingData`:

| Field | Encoding |
|---|---|
| `eRegid` | Signal registration id, 4 bytes big-endian |
| `eKeytype` | one byte `0x05` (`keys.DJBType`) |
| `eIdent` | identity public key, 32 bytes |
| `eSkeyId` | signed prekey id, 3 bytes big-endian |
| `eSkeyVal` | signed prekey public key, 32 bytes |
| `eSkeySig` | signature over `0x05 || signed_prekey`, 64 bytes |
| `buildHash` | `MD5("a.b.c")` of the version string (`Version.BuildHash()`) |
| `deviceProps` | serialized `DeviceProps{os, version 0.1.0, platformType, requireFullSync=false}` |

`os` defaults to `"wax"` and `platformType` to `DESKTOP`; these control the name and icon in the phone's linked devices list.

What happens next is outside the transport: for a registration the server sends an `<iq type="set">` with `<pair-device>` (QR refs); for a login it sends `<success>` or `<failure>`.

## Encrypted transport

`NoiseConn` (`internal/transport/conn.go`) carries all traffic after the handshake:

- Each direction has its own AES-256-GCM key from `split()` and its own 32-bit counter starting at 0.
- Frame `n` in a direction is encrypted with nonce `nonceFor(n)` and **no associated data**.
- The ciphertext (plaintext plus 16-byte GCM tag) is sent as one length-prefixed frame.
- The write counter advances even if the write fails, so a nonce is never reused. At `2^32 - 1` the connection is closed rather than wrap.
- Because counters are implicit, any lost, duplicated or corrupted frame desynchronizes the stream. A decryption failure is fatal: `Receive()` closes the connection and returns `ErrDecrypt` (`TestTamperedFrameIsFatal`).

The decrypted plaintext is a binary XML payload starting with a flags byte; see [02-binary-xml.md](02-binary-xml.md). A frame that decrypts but cannot be decoded is **not** fatal: `Conn.ReadNode()` returns `*DecodeError` and the read loop logs and skips it.

## Session structure

`Client.runSession()` (`internal/client/session.go`) runs one connection with three goroutines:

- `readLoop()`: reads nodes, delivers IQ results/errors and acks to waiting senders, pushes everything else to a non-blocking queue (`nodeQueue`). A transport read error ends the session with `ActionReconnect`.
- `handleLoop()`: processes queued nodes in arrival order (`handleNode()`); a panic in a handler is logged and the next node is processed.
- `keepAliveLoop()`: see below.

Stanza ids for outgoing requests come from `Conn.NextID()`: a random 2-byte prefix rendered as 4 hex characters, a hyphen and a per-connection counter (for example `3fa1-17`).

## Keepalive

There are two kinds of ping.

**Client pings** (`keepAliveLoop()` in `internal/client/handlers.go`): after a random interval in `[KeepAliveMin, KeepAliveMax)` (default 20 to 30 s) wax sends

```
<iq id="3fa1-17" to="s.whatsapp.net" type="get" xmlns="w:p">
  <ping/>
</iq>
```

and waits up to `KeepAliveTimeout` (10 s) for the matching `iq` result. On failure it emits a `KeepAliveTimeout` event and keeps trying. If no ping has succeeded for longer than `KeepAliveMaxFailure` (90 s), the session ends with `ActionReconnect`, since a half-open TCP connection may otherwise look alive indefinitely.

**Server pings**: an incoming `<iq type="get" xmlns="urn:xmpp:ping">` is answered with `<iq type="result" id="..." to="...">`, addressed to the request's `from` or to `s.whatsapp.net` if absent (`handleIQ()`, `iqResult()`).

`cmd/wax-probe` has its own minimal loop with the same 20 to 30 s interval but does not wait for responses.

## Stream errors and disconnects

`ClassifyDisconnect()` in `internal/client/disconnect.go` maps `<stream:error>` and `<failure>` nodes to an action. Codes come from observed server behaviour; the code comments note that another client was used as a cross-reference.

`<stream:error code="..."><conflict type="..."/></stream:error>`:

| Condition | Action |
|---|---|
| `code="515"` | `ActionReconnectNow` (restart requested, normally right after pairing) |
| `code="401"` and `conflict type="device_removed"` | `ActionLoggedOut` |
| `conflict type="replaced"` | `ActionReplaced` |
| anything else (503, 500, unknown) | `ActionReconnect` |

`<failure reason="..." message="..."/>` (sent instead of `<success>` at login):

| `reason` | Action |
|---|---|
| 401, 403, 406 | `ActionLoggedOut` |
| 402 | `ActionTempBanned` |
| 405 | `ActionUpgradeRequired` (client version too old) |
| 500, 503 | `ActionReconnect` |
| anything else | `ActionFatal` |

The asymmetry is deliberate: unknown stream errors are usually transient, while an unknown login failure is not retried so the server is not hammered with a login it keeps refusing.

A bare `<xmlstreamend/>` ends the session with `ActionReconnect`.

## Reconnect and backoff

`Client.Run()` (`internal/client/client.go`) loops over `runSession()` and acts on the result:

| Action | Behaviour |
|---|---|
| `ActionReconnectNow` | reset the attempt counter, reconnect immediately |
| `ActionReconnect` | increment attempt, sleep `Backoff.Delay(attempt)`, reconnect |
| `ActionLoggedOut` | delete the stored device, emit `LoggedOut`, return `ErrLoggedOut` |
| `ActionReplaced` | emit `StreamReplaced`, return `ErrReplaced` (no reconnect, or two instances would keep kicking each other off) |
| `ActionUpgradeRequired` | emit `ClientOutdated`, return `ErrClientOutdated` |
| `ActionTempBanned` | emit `TemporaryBan`, return `ErrTempBanned` |
| `ActionFatal` / other | return the error (for example `ErrQRTimeout`, `ErrPairing`) |

Every exit emits a `Disconnected` event first. Dial and handshake errors map to `ActionReconnect`. If the session had been logged in for at least 30 s, the attempt counter is reset before the delay is computed. A database error while loading the device is retried with the same backoff.

The policy is `internal/backoff.Policy` with `DefaultBackoff = {Min: 1s, Max: 5m}`. For attempt `n` the ceiling is `Min * 2^(n-1)` capped at `Max`, and the delay is uniformly random in `[Min/2, ceiling]`. Typical ceilings: 1 s, 2 s, 4 s, ... up to 5 min.

## Version detection

The server rejects clients whose announced version is too old with `<failure reason="405">`. `internal/waversion` models the version as three `uint32`s (`2.3000.<revision>`) and provides:

- `Default`: a built-in version (the comment dates it to 2026-10-15).
- `Parse()`, `String()`, `Less()`, and `BuildHash()` (`MD5` of the dotted string, used in registration).
- `FetchLatest()`: fetches `https://web.whatsapp.com/sw.js` with browser-like headers (`Sec-Fetch-Dest: serviceworker`, `Service-Worker: script`, and others; without them an error page is returned) and extracts `client_revision` with a regex that tolerates JSON escaping. The result is always `2.3000.<client_revision>`: sw.js does not contain the major and minor numbers, so a change of the `2.3000` prefix would go unnoticed. Body size is capped at 4 MiB.
- `Watch()`: polls `FetchLatest()` at an interval and calls back only when the live version is **newer** than the one in use (during staged rollouts edge servers return different revisions, so "different" would flap). It does not check at start.

How it is used:

- `cmd/wax-probe` and `cmd/wax`: `-version` flag if given, else `FetchLatest()`, else `Default`.
- `cmd/wax` runs `Watch()` every 6 h but only logs; the running process keeps its version.
- `internal/manager` runs `Watch()` every 6 h and stores newer versions; `client.Config.Version` is consulted on every connection attempt, so the next reconnect announces the new version without a restart.

## wax-probe

`cmd/wax-probe/main.go` is a minimal end-to-end check of this layer. It generates throwaway keys, sends a registration payload, completes the handshake, logs every node (masked unless `-unmask`), answers `urn:xmpp:ping` and `pair-device` IQs with empty results, sends `w:p` pings, and exits on any disconnect node or after `-duration` (default 90 s). Nothing is persisted or paired.

## In the code

| Concept | File | Symbols |
|---|---|---|
| WebSocket dial, endpoint, prologue | `internal/transport/websocket.go` | `Dial()`, `WSConn`, `DefaultURL` |
| Frame length prefix, reassembly | `internal/transport/frame.go` | `appendFrame()`, `frameSplitter` |
| Connection header | `internal/client/conn.go` | `ConnHeader`, `Connect()` |
| Noise handshake (client) | `internal/transport/handshake.go` | `ClientHandshake()` |
| Noise symmetric state, nonce | `internal/transport/noise.go` | `symmetricState`, `nonceFor()` |
| Certificate chain | `internal/transport/cert.go` | `CertVerifier.Verify()` |
| Responder for tests | `internal/transport/server.go` | `ServerHandshake()` |
| Encrypted frame stream | `internal/transport/conn.go` | `NoiseConn` |
| ClientPayload | `internal/client/payload.go` | `LoginPayload()`, `RegistrationPayload()` |
| Session goroutines | `internal/client/session.go` | `runSession()`, `readLoop()` |
| Keepalive | `internal/client/handlers.go` | `keepAliveLoop()`, `handleIQ()` |
| Disconnect classification | `internal/client/disconnect.go` | `ClassifyDisconnect()` |
| Reconnect loop | `internal/client/client.go` | `Run()`, `payloadFor()` |
| Backoff | `internal/client/backoff.go`, `internal/backoff/backoff.go` | `DefaultBackoff`, `Policy.Delay()` |
| Version detection | `internal/waversion/version.go` | `FetchLatest()`, `Watch()` |
| Probe tool | `cmd/wax-probe/main.go` | `run()`, `keepAlive()` |
