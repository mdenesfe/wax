# 03. Pairing (linking a companion device)

A WhatsApp multi-device companion is a separate Signal device attached to the
phone's account. wax creates the identity of that device locally (a Noise key,
a signing-capable identity key, a signed prekey, a registration id and a random
ADV secret), announces the public parts to the server in a registration
`ClientPayload`, and shows a rotating QR code built from server-provided refs.
When the phone scans the code, the server delivers a `pair-success` IQ carrying
an account-signed device identity. wax authenticates it with the ADV secret,
verifies the account signature, countersigns it with its own identity key,
persists the result and answers with `pair-device-sign`. The server then
restarts the stream (code 515) and wax logs in as a paired device, uploads
one-time prekeys and becomes ready. This document describes that flow exactly
as the code implements it.

## 1. The identity of a new device

`store.NewDevice` in `internal/store/device.go` generates everything a fresh,
unpaired instance needs. Nothing is sent anywhere at this point; the device is
written to `wax_devices` by `Store.CreateDevice`.

| Field | Type | How it is created | Used for |
|---|---|---|---|
| `NoiseKey` | X25519 key pair | `keys.NewKeyPair` | Static key of the Noise handshake; also shown in the QR code |
| `IdentityKey` | `keys.SigningKey` | `keys.NewSigningKey` (random 32-byte seed) | Signal identity key; signs the signed prekey and the ADV device signature |
| `SignedPreKey` | X25519 key pair, id `1`, 64-byte signature | `keys.NewKeyPair`, signed with `IdentityKey.SignPublicKey` | X3DH responder key (see [04-signal.md](04-signal.md)) |
| `RegistrationID` | `uint32` | uniform random in `1..16380` | Signal registration id (14-bit range, never zero) |
| `AdvSecret` | 32 random bytes | `crypto/rand` | HMAC key that authenticates the `pair-success` payload |

The identity key is generated from an Ed25519 seed so it can produce
libsignal-compatible Curve25519 signatures (see `internal/keys/signing.go` and
section 1 of [04-signal.md](04-signal.md)). Because of that, the database stores
the seed, not the X25519 private key.

The signed prekey signature covers the serialized public key `0x05 || pub`
(`SigningKey.SignPublicKey`). wax never rotates the signed prekey: its id is
always `1` for the life of the device. Signed prekey rotation is not
implemented.

## 2. The registration payload

On every connection `Client.payloadFor` (`internal/client/client.go`) picks the
payload that is encrypted into the Noise `ClientFinish` message
(`internal/transport/handshake.go`, see [01-transport.md](01-transport.md)):

- unpaired device (`Device.IsPaired()` is false, that is the JID is empty):
  `RegistrationPayload`;
- paired device: `LoginPayload` (section 7).

`RegistrationPayload` in `internal/client/payload.go` builds a `wa6.ClientPayload`
with the common user agent fields (`basePayload`: platform `WEB`, release
channel, app version, placeholder MCC/MNC `000`, locale) plus:

| `ClientPayload` field | Value |
|---|---|
| `passive` | `false` |
| `pull` | `false` |
| `devicePairingData.eRegid` | registration id, 4 bytes big-endian |
| `devicePairingData.eKeytype` | `[0x05]` (Signal DJB key type) |
| `devicePairingData.eIdent` | identity public key, 32 bytes |
| `devicePairingData.eSkeyID` | signed prekey id, 3 bytes big-endian (`keyID3`) |
| `devicePairingData.eSkeyVal` | signed prekey public key, 32 bytes |
| `devicePairingData.eSkeySig` | signed prekey signature, 64 bytes |
| `devicePairingData.buildHash` | MD5 of the version string `a.b.c` (`waversion.Version.BuildHash`) |
| `devicePairingData.deviceProps` | serialized `DeviceProps{os, version 0.1.0, platformType, requireFullSync=false}` |

`DeviceProps.os` is the name shown in the phone's "Linked devices" list
(`DeviceConfig.OSName`, default `wax`) and `platformType` selects the icon
(default `DESKTOP`).

## 3. QR refs and the QR string

After the handshake the server sends an IQ with the refs to encode:

```
<iq from="s.whatsapp.net" type="set" id="...">
  <pair-device>
    <ref>REF_1</ref>
    <ref>REF_2</ref>
    ...
  </pair-device>
</iq>
```

`session.handlePairDevice` (`internal/client/handlers.go`) first answers with an
empty `<iq type="result">`, ignores the IQ if the device is already paired, and
extracts the non-empty `<ref>` payloads in order with `pairing.ParsePairDevice`.
How many refs the server sends is not assumed; wax uses whatever list arrives.

Each ref becomes one QR string, built by `pairing.QRContent`:

```
https://wa.me/settings/linked_devices#REF,B64(noisePub),B64(identityPub),B64(advSecret),CLIENT_TYPE
```

- the three keys are standard base64 (with padding) of 32 bytes each;
- `CLIENT_TYPE` is a single digit derived from `DeviceConfig.Platform` by
  `pairing.ClientTypeFor` (`1` Chrome, `2` Edge, `3` Firefox, `4` IE, `5` Opera,
  `6` Safari, and `9` "other web" for everything else, including the default
  `DESKTOP`).

The URL prefix lets a generic camera app open the linked devices screen.
`pairing.RenderTerminal` and `pairing.RenderPNG` (`internal/pairing/qr.go`) draw
the string with error correction level L.

### Rotation

A goroutine (`qr-rotator`) walks the ref list and emits a `QRCode` event per
ref:

| Ref | Display time |
|---|---|
| first | `FirstRefTimeout` = 60 s |
| each following | `NextRefTimeout` = 20 s |

The rotation stops when `pair-success` arrives (its `done` channel is closed),
when the session ends, or when a new `pair-device` replaces the list. If the
last ref expires, wax emits `QRTimeout` and ends the session with
`ErrQRTimeout` (`ActionFatal`): `Client.Run` returns and does not reconnect.
Requesting more refs from the server is not implemented; a new run starts a new
registration.

## 4. pair-success

When the phone scans a code, the server sends:

```
<iq type="set" id="..." t="1700000000">
  <pair-success>
    <device jid="905550000001:12@s.whatsapp.net" lid="1234:12@lid"/>
    <platform name="android"/>
    <biz name="..."/>                      (optional)
    <device-identity>BYTES</device-identity>
    <client-props>BYTES</client-props>     (optional)
  </pair-success>
</iq>
```

`pairing.ParsePairSuccess` requires `<device jid>` with a user part and a
non-empty `<device-identity>`. `lid`, `<platform>`, `<biz>` and `<client-props>`
are optional; `client-props` is decoded as `ClientPairingProps` on a best-effort
basis and is informational only.

### ADV verification

`<device-identity>` holds three nested protobufs from
`internal/proto/waadv/waadv.proto`:

```
ADVSignedDeviceIdentityHMAC
 ├─ details  ──► ADVSignedDeviceIdentity
 │                ├─ details  ──► ADVDeviceIdentity { rawID, timestamp, keyIndex, accountType, deviceType }
 │                ├─ accountSignatureKey   (phone identity key, 32 bytes)
 │                ├─ accountSignature      (64 bytes)
 │                └─ deviceSignature       (empty, filled in by us)
 ├─ HMAC
 └─ accountType
```

`pairing.Verify` performs three steps. `||` is concatenation.

1. **HMAC.** `HMAC-SHA256(advSecret, prefix || container.details)` must equal
   `container.HMAC` (constant-time compare). `prefix` is `[6, 5]` when
   `container.accountType` is `HOSTED`, empty otherwise. Only the phone that
   scanned our QR knows the ADV secret, so this proves the identity was not
   substituted by the server. Failure: `ErrHMACMismatch`.
2. **Account signature.** The account key must have signed
   `prefix || signed.details || ourIdentityPub`, where `prefix` is `[6, 0]`,
   or `[6, 5]` when `ADVDeviceIdentity.deviceType` is `HOSTED`. The signature is
   checked with `keys.VerifySlice`. This binds our identity key to the account.
   Failure: `ErrSignatureMismatch`. Wrong key or signature lengths are
   `ErrMalformed`.
3. **Device signature.** wax signs
   `[6, 1] || signed.details || ourIdentityPub || accountSignatureKey` with its
   identity key and stores the 64-byte result in `deviceSignature`.

The result (`pairing.Result`) contains:

- `Account`: the full `ADVSignedDeviceIdentity` including our device signature.
  This is persisted and later attached as `<device-identity>` to outgoing
  stanzas that contain a `pkmsg` (see `internal/client/send.go`) and to retry
  receipts that carry keys.
- `PrimaryIdentity`: the phone's identity key (`accountSignatureKey`).
- `KeyIndex`: `ADVDeviceIdentity.keyIndex`.
- the self-signed copy for the reply: the same message with
  `accountSignatureKey` cleared.

wax only verifies its own pairing identity. It does not verify the
`<device-identity>` that other devices attach to their messages, and it does
not process `ADVSignedKeyIndexList` (the proto messages exist, but nothing
consumes them).

### Persist, then confirm

`session.handlePairSuccess` stores the pairing before replying:

1. In one transaction: `Store.SavePairing` writes `jid`, `lid`, `account`,
   `platform`, `business_name` and `paired_at`, and `Store.PutIdentity` stores
   `PrimaryIdentity` in `wax_identities` under the Signal address of the
   primary device (device 0), using the LID when present and the phone number
   JID otherwise (for example `1234_1.0`).
2. Only then the confirmation is sent:

```
<iq to="s.whatsapp.net" type="result" id="(pair-success id)">
  <pair-device-sign>
    <device-identity key-index="KEY_INDEX">SELF_SIGNED_BYTES</device-identity>
  </pair-device-sign>
</iq>
```

The order is deliberate: if the process dies between the two, the server never
completes the pairing and the next login fails, which deletes the half-paired
device. The reverse order could leave a device on the phone whose keys were
never saved. If sending the confirmation fails, the stored pairing is kept;
the next login either succeeds or is refused with 401.

On any verification or persistence error `session.failPairing` answers the IQ
with an error so the phone stops waiting, emits `PairError` and ends the
session as fatal:

| Error | Reply |
|---|---|
| `ErrHMACMismatch` | `<error code="401" text="hmac-mismatch"/>` |
| `ErrSignatureMismatch` | `<error code="401" text="signature-mismatch"/>` |
| anything else | `<error code="500" text="internal-error"/>` |

On success wax emits `PairSuccess` and waits. The server closes the stream with
`<stream:error code="515"/>`, which `ClassifyDisconnect`
(`internal/client/disconnect.go`) maps to `ActionReconnectNow`: reconnect
immediately, without backoff.

## 5. What is persisted

All in `wax_devices` (`internal/store/migrations/001_init.sql`), one row per
instance. Sealed columns are AES-256-GCM encrypted with associated data
`wax_devices|<column>|<instance>` (see `internal/store/sealer.go` and section 7
of [04-signal.md](04-signal.md)).

| Column | Content | Sealed |
|---|---|---|
| `registration_id` | registration id | no |
| `noise_key` | Noise X25519 private key | yes |
| `identity_seed` | 32-byte identity key seed | yes |
| `signed_prekey_id`, `signed_prekey` | id and X25519 private key | key sealed |
| `signed_prekey_sig` | 64-byte signature | no |
| `adv_secret` | 32-byte ADV secret | yes |
| `next_prekey_id` | next one-time prekey id | no |
| `jid`, `lid` | our device JIDs (NULL while unpaired) | no |
| `account` | `ADVSignedDeviceIdentity` with our device signature | no |
| `platform`, `business_name`, `push_name`, `paired_at` | metadata | no |

`Store.LoadDevice` reopens the sealed columns, rebuilds the signing key from the
seed (`keys.SigningKeyFromSeed`) and pre-parses it for DH. A device counts as
paired exactly when `jid` is set. The ADV secret stays stored after pairing but
is not used again.

## 6. One-time prekeys

### Storage

`wax_prekeys` rows hold a sealed private key (associated data
`wax_prekeys|private_key|<instance>|<id>`) and an `uploaded` flag.
`Store.GetOrGenPreKeys` (`internal/store/prekeys.go`) first returns keys that
were generated but not yet uploaded, and generates the rest. It locks the
device row (`SELECT ... FOR UPDATE` on `next_prekey_id`) so concurrent callers
never hand out the same id. Ids are 24-bit (they travel as 3 bytes) and wrap
from `16777215` back to `1`, replacing any stale row with the same id.

### Upload

`Client.uploadPreKeys` (`internal/client/prekeys.go`) sends:

```
<iq to="s.whatsapp.net" type="set" xmlns="encrypt" id="...">
  <registration>REG_ID (4 bytes BE)</registration>
  <type>0x05</type>
  <identity>IDENTITY_PUB (32)</identity>
  <list>
    <key><id>ID (3 bytes BE)</id><value>PUB (32)</value></key>
    ...
  </list>
  <skey><id>00 00 01</id><value>SPK_PUB (32)</value><signature>SIG (64)</signature></skey>
</iq>
```

Keys are marked `uploaded` only after the server answered the IQ. If the upload
fails, the same keys are offered again next time, so database and server never
disagree about which keys exist.

### When uploads happen

| Trigger | Code | Count |
|---|---|---|
| after login, both server count and local uploaded count are 0 (first login after pairing) | `ensurePreKeys` | 812 (`initialPreKeys`) |
| after login, server count or local uploaded count below 5 | `ensurePreKeys` | 50 (`refillPreKeys`) |
| `<notification type="encrypt" from="s.whatsapp.net"><count value="N"/></notification>` | `session.onPreKeyCount` | 50 |

The server count comes from `<iq type="get" xmlns="encrypt"><count/></iq>`,
answered with `<count value="N"/>`. Uploads are serialized by `Client.preKeyMu`.

A prekey is deleted (`Store.RemovePreKey`) in the same transaction that stores
the session a `pkmsg` created with it. A retry receipt with keys hands out one
prekey directly and marks it uploaded (`sendRetryReceipt` in
`internal/client/retry.go`).

## 7. Login after pairing

For a paired device `LoginPayload` sends `username` (the numeric user part of
the stored JID), `device` (the device index), `passive=true` and `pull=true`,
with the same user agent fields as registration and no pairing data.

When the server answers with `<success>`, `session.handleSuccess`:

1. replays stanzas that were deferred earlier;
2. records our own PN to LID mapping and, if `<success lid="...">` carries a
   different LID, stores it with `Store.UpdateLID` (filling in our device index
   when the server omits it);
3. in a background goroutine: starts the history worker, purges old buffers,
   runs `ensurePreKeys`, sends `<iq type="set" xmlns="passive"><active/></iq>`,
   marks the session ready, emits `Connected` and syncs app state.

A `<success>` on an unpaired connection is logged and ignored.

## 8. Logout and removal

- **Local logout.** `Client.Logout` sends
  `<iq type="set" xmlns="md"><remove-companion-device jid="OUR_DEVICE_JID" reason="user_initiated"/></iq>`.
  The server then ends the stream as below.
- **Removed from the phone, or session invalid.** The server ends the stream
  with `<stream:error code="401"><conflict type="device_removed"/></stream:error>`,
  or refuses login with `<failure reason="401|403|406"/>`. Both map to
  `ActionLoggedOut`.

On `ActionLoggedOut` `Client.Run` calls `Store.DeleteDevice`. The
`ON DELETE CASCADE` foreign keys remove all prekeys, identities, sessions,
sender keys and buffers of the instance. wax emits `LoggedOut` and returns
`ErrLoggedOut`; the next run creates a new unpaired device and a new QR.

## 9. State machine

```
             load device (Run)
                   │
      ┌────────────┴─────────────┐
   not found                   found
      │                          │
  NewDevice + CreateDevice   IsPaired()? ──yes──────────────┐
      │                          │ no                       │
      └───────────┬──────────────┘                          │
                  ▼                                         ▼
          [UNPAIRED] Noise handshake              [PAIRED] Noise handshake
          with RegistrationPayload                with LoginPayload
                  │                                         │
          <pair-device> refs                     <success>  │  <failure 401/403/406>
                  ▼                                  │            │
          [SHOWING_QR] 60 s, then 20 s per ref       ▼            ▼
             │             │            │      [LOGGED_IN]   [LOGGED_OUT]
       all refs expire  bad payload   <pair-success>  │       DeleteDevice,
             │          or error         │            │       ErrLoggedOut
             ▼             ▼             ▼            │
        ErrQRTimeout   PairError    Verify OK, persist,   stream:error 401
         (fatal)        (fatal)     pair-device-sign  device_removed ──► [LOGGED_OUT]
                                         │
                                  stream:error 515
                                         │
                                         ▼
                              reconnect now ──► [PAIRED]
```

Transient disconnects (`stream:error` 500/503, network errors) reconnect with
backoff and keep the stored device in every state.

## 10. Not implemented

- Pairing with a phone number code (link code). Only QR pairing exists.
- Signed prekey rotation (the id stays `1`).
- Asking the server for new QR refs after the list is exhausted.
- Verification of other devices' ADV identities and of key index lists.

## In the code

| Concept | Where |
|---|---|
| Fresh device keys, persistence of the device | `internal/store/device.go` (`NewDevice`, `CreateDevice`, `SavePairing`, `LoadDevice`, `DeleteDevice`) |
| Signing identity key, signed prekey signature | `internal/keys/signing.go` (`SigningKey`, `SignPublicKey`) |
| Registration and login payloads | `internal/client/payload.go` (`RegistrationPayload`, `LoginPayload`), `internal/client/client.go` (`payloadFor`) |
| QR string, client type, ref timeouts | `internal/pairing/pairing.go` (`QRContent`, `ClientTypeFor`, `FirstRefTimeout`) |
| QR rendering | `internal/pairing/qr.go` |
| pair-device / pair-success handling | `internal/client/handlers.go` (`handlePairDevice`, `handlePairSuccess`, `failPairing`) |
| ADV verification and reply | `internal/pairing/pairing.go` (`ParsePairSuccess`, `Verify`, `ConfirmNode`, `ErrorNode`) |
| Login and post-login setup | `internal/client/handlers.go` (`handleSuccess`) |
| Prekey generation and storage | `internal/store/prekeys.go` |
| Prekey upload and refill | `internal/client/prekeys.go`, `internal/client/handlers.go` (`onPreKeyCount`) |
| Logout and disconnect classification | `internal/client/client.go` (`Logout`, `Run`), `internal/client/disconnect.go` |
| Table definitions | `internal/store/migrations/001_init.sql` |
