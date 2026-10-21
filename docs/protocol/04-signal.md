# 04. Signal: end-to-end encryption

wax implements the Signal protocol version 3 from scratch in
`internal/signal`, on top of Go standard library primitives only (X25519,
Ed25519, SHA-256/512, HKDF, HMAC, AES). One-to-one messages use X3DH to start a
session from a prekey bundle and the Double Ratchet afterwards; groups use
sender keys, a per-sender symmetric chain distributed over the 1:1 sessions and
authenticated with a per-chain signing key. The `signal` package is a pure
library: it operates on `Record` and `SenderKeyRecord` values, and the client
loads, mutates and stores them inside PostgreSQL transactions where every
secret is sealed with AES-256-GCM. This document describes the key types, the
KDFs and their info strings, the wire formats and the storage, as implemented.
The package comment states that behaviour follows libsignal-protocol-java 2.x;
the code does not include or port any of it.

## 1. Keys and signatures

### Key types

| Type | Package | Content |
|---|---|---|
| `keys.KeyPair` | `internal/keys/keys.go` | X25519 private and public key, 32 bytes each |
| `keys.SigningKey` | `internal/keys/signing.go` | a `KeyPair` that can also sign; persisted as a 32-byte seed |

Public keys are serialized Signal style as `0x05 || key` (33 bytes,
`keys.DJBType`). `signal.decodeKey` also accepts the bare 32-byte form, since
the server sends raw keys in some places (prekey bundles, for example).

`KeyPair.DH` uses `crypto/ecdh`, which rejects low-order public keys (an
all-zero shared secret), so every DH in the protocol fails closed on such keys.

### Curve25519 signatures

Signal identities are Montgomery (X25519) keys, but signatures are Edwards
signatures. libsignal uses an XEdDSA-style format: a 64-byte Ed25519-shaped
signature whose top bit of the last byte carries the sign bit of the Edwards
public key. wax produces and checks this format without implementing curve
arithmetic:

**Key generation** (`keys.SigningKeyFromSeed`)

1. Start from a random 32-byte Ed25519 seed.
2. The X25519 private key is `clamp(SHA-512(seed)[0:32])`, the same scalar
   Ed25519 derives from the seed.
3. The X25519 public key is the Montgomery form of the Ed25519 public key,
   `u = (1 + y) / (1 - y) mod p` (`edwardsToMontgomery`).

**Signing** (`SigningKey.Sign`)

1. `sig = Ed25519.Sign(seed key, msg)` (deterministic, standard library).
2. `sig[63] = (sig[63] & 0x7f) | (edPub[31] & 0x80)`: embed the Edwards sign
   bit. The top bit of `S` is always zero, so no information is lost.

**Verification** (`keys.Verify`)

1. Rebuild the Edwards public key from the Montgomery `u`:
   `y = (u - 1) / (u + 1) mod p`, with the sign bit taken from `sig[63] >> 7`.
2. Clear that bit in a copy of the signature and call `ed25519.Verify`.

The field arithmetic for the conversions uses `math/big` on public values
only. Note the consequence documented in `signing.go`: a signing key exists
only as a seed. A bare X25519 private key generated elsewhere cannot be turned
into a `SigningKey`, which is why the device stores `identity_seed` and sender
key records store `signingSeed`. Unlike the XEdDSA specification, signing is
deterministic (no random nonce), because it delegates to Ed25519.

`SignPublicKey(pub)` signs `0x05 || pub`; this is the signed prekey signature.

## 2. Primitives and KDF labels

All symmetric helpers live in `internal/cryptoutil/cryptoutil.go`.

| Use | Primitive | Salt | Info | Output |
|---|---|---|---|---|
| X3DH master secret to first root/chain key | HKDF-SHA256 | none (zeros) | `WhisperText` | 64 bytes: root 32, chain 32 |
| DH ratchet step | HKDF-SHA256 | current root key | `WhisperRatchet` | 64 bytes: root 32, chain 32 |
| Message keys | HKDF-SHA256 over `HMAC(ck, 0x01)` | none | `WhisperMessageKeys` | 80 bytes: cipher 32, MAC 32, IV 16 |
| Next chain key | `HMAC-SHA256(ck, 0x02)` | | | 32 bytes |
| Sender key message keys | HKDF-SHA256 over `HMAC(seed, 0x01)` | none | `WhisperGroup` | 48 bytes: IV 16, cipher 32 |
| Body encryption | AES-256-CBC, PKCS#7 | | | |

Constants are in `internal/signal/kdf.go`.

## 3. X3DH session setup

### Fetching bundles

Before the first message to a device without a session, `session.fetchBundles`
(`internal/client/usync.go`) asks the server:

```
<iq to="s.whatsapp.net" type="get" xmlns="encrypt" id="...">
  <key>
    <user jid="905550000001:3@s.whatsapp.net" reason="identity"/>
    ...
  </key>
</iq>
```

`parseBundle` reads each `<user>` of the `<list>` in the answer:
`<registration>` (4 bytes), then from `<keys>` (or the user node itself)
`<identity>` (32), `<skey>` with `<id>` (3), `<value>` (32), `<signature>` (64),
and an optional `<key>` one-time prekey. A user with an `<error>` child is
skipped and logged.

### Initiator ("Alice"), `Record.ProcessBundle`

1. Verify `SignedPreKeySignature` over `0x05 || SignedPreKey` with the bundle's
   identity key. Failure: `ErrInvalidSignature`.
2. Generate a base key `EK` and a first sending ratchet key.
3. `initAlice` computes, in this order:

```
master = 0xFF * 32
      || DH(IK_ours,  SPK_theirs)
      || DH(EK,       IK_theirs)
      || DH(EK,       SPK_theirs)
     [|| DH(EK,       OPK_theirs)]     only if the bundle had a one-time prekey
```

4. `(root, chain) = HKDF(master, "WhisperText")`. The chain becomes the
   receiving chain for the peer's ratchet key, which is their signed prekey.
5. One DH ratchet step right away: `createChain(SPK_theirs, sendingKey)` gives
   the new root and the sending chain.
6. Store a "pending prekey" (signed prekey id, optional one-time prekey id,
   base key), both registration ids and `aliceBaseKey`. The previous current
   session, if any, is archived.

While a pending prekey exists, every `Encrypt` output is wrapped in a
PreKeySignalMessage (`pkmsg`).

### Responder ("Bob"), `Record.DecryptPreKeyMessage`

1. Parse the `pkmsg`. If the record (current or archived) already holds a
   session with the same version and `aliceBaseKey`, it is reused: the peer
   simply resent before seeing a reply.
2. Otherwise look up our signed prekey by id and, if present, the one-time
   prekey (`store.PreKeyStore`). The signed prekey id must equal ours, else
   `ErrMissingPreKey`.
3. `initBob` computes the mirror image:

```
master = 0xFF * 32
      || DH(SPK_ours, IK_theirs)
      || DH(IK_ours,  EK_theirs)
      || DH(SPK_ours, EK_theirs)
     [|| DH(OPK_ours, EK_theirs)]
```

4. `(root, chain) = HKDF(master, "WhisperText")`; our sending chain starts with
   that chain and our signed prekey as ratchet key.
5. Decrypt the inner SignalMessage, which triggers a DH ratchet step (section 4).

All of this happens on a clone of the record; the record changes only if
decryption succeeds. `PreKeyResult.UsedPreKeyID` tells the caller which
one-time prekey to delete, which `session.decryptSession`
(`internal/client/message.go`) does in the same transaction that saves the
session.

## 4. Double Ratchet

### Session state (`internal/signal/session.go`)

```
Record
 ├─ current:  sessionState
 └─ previous: []sessionState    newest first, at most 40 (maxArchivedState)

sessionState
 ├─ version, localIdentity, remoteIdentity, localRegID, remoteRegID, aliceBaseKey
 ├─ root key
 ├─ sender chain:     our ratchet key pair + chain key (index)
 ├─ receiver chains:  peer ratchet pub + chain key + skipped message keys
 │                    at most 5 chains (maxReceiverChain), oldest dropped
 ├─ previousCounter
 └─ pending prekey    (initiator only, until the peer replies)
```

### Sending (`Record.Encrypt`)

1. `mk = messageKeys(senderChain)`; encrypt with AES-256-CBC (`mk.cipher`,
   `mk.iv`).
2. Build a SignalMessage with `counter = senderChain.index` and
   `previousCounter`, MAC it (section 5).
3. If a pending prekey exists, wrap it in a PreKeySignalMessage.
4. Advance the sending chain (`HMAC(ck, 0x02)`, index + 1).

### Receiving (`decryptWithState`)

1. Check the message version against the session version.
2. `getOrCreateReceiver`: if the ratchet key is new, perform a DH ratchet step:
   - `createChain(theirRatchet, ourCurrentSendingKey)` gives the receiving chain;
   - generate a new sending key, `createChain(theirRatchet, newKey)` gives the
     new root and sending chain;
   - `previousCounter = oldSendingChain.index - 1` (0 if the index is 0).
3. `getOrCreateMessageKeys` for the message counter:
   - counter below the chain index: use and remove a stored skipped key, or
     fail with `ErrDuplicateMessage`;
   - counter more than 2000 ahead (`maxForwardJump`): `ErrInvalidMessage`;
   - otherwise derive and store keys for every skipped counter (at most 2000
     per receiver chain, `maxMessageKeys`; oldest are dropped), then advance.
4. Verify the MAC, decrypt, and clear the pending prekey: the peer evidently
   received our session, so plain `msg` is enough from now on.

`Record.decrypt` tries the current session first and then each archived one;
an archived session that decrypts is promoted to current. A duplicate
counter stops the search immediately. The client treats `ErrDuplicateMessage`
as an already handled message.

## 5. Wire formats

The version byte is `0x33`: message version in the high nibble, current
version in the low nibble. Parsers look only at the high nibble and reject
anything other than 3 (`ErrLegacyVersion` below, `ErrInvalidMessage` above).
Field definitions: `proto/signalwire/signalwire.proto`.

### `msg`: SignalMessage

```
+------+--------------------------------------------+---------+
| 0x33 | SignalMessage protobuf                     | MAC (8) |
+------+--------------------------------------------+---------+
         ratchetKey      = 0x05 || 32 bytes
         counter, previousCounter
         ciphertext      = AES-256-CBC(PKCS#7)

MAC = HMAC-SHA256(macKey, 0x05||senderIdentity || 0x05||receiverIdentity || 0x33||protobuf)[0:8]
```

### `pkmsg`: PreKeySignalMessage

```
+------+---------------------------------------------------+
| 0x33 | PreKeySignalMessage protobuf                      |
+------+---------------------------------------------------+
         registrationId, preKeyId (optional), signedPreKeyId
         baseKey      = 0x05 || EK
         identityKey  = 0x05 || IK_sender
         message      = complete SignalMessage (version, protobuf, MAC)
```

The outer message has no MAC of its own; it is authenticated through the inner
message's MAC, which binds both identity keys.

### Stanza

`Client.encryptForDevice` (`internal/client/send.go`) places the ciphertext in

```
<enc v="2" type="pkmsg|msg" [mediatype="..."]>CIPHERTEXT</enc>
```

inside `<participants><to jid="...">`. When any participant got a `pkmsg`, the
stanza also carries `<device-identity>` with our stored ADV account identity
(see [03-pairing.md](03-pairing.md)).

## 6. Sender keys (groups)

Implemented in `internal/signal/senderkey.go`. A `SenderKeyRecord` holds up to
5 states (`maxSenderKeyStates`), newest first, for one (group, sender device)
pair.

### Own chain and distribution

`SenderKeyRecord.CreateDistribution` creates our chain on first use: a random
32-byte chain seed, a random signing seed (a `SigningKey`), and a random key id
masked to 31 bits. It returns:

```
0x33 || SenderKeyDistributionMessage { id, iteration, chainKey (32), signingKey (0x05||pub) }
```

`Client.sendGroup` wraps it in
`wae2e.Message{senderKeyDistributionMessage{groupId, axolotlSenderKeyDistributionMessage}}`
and encrypts that over the 1:1 sessions to the devices that do not have our
key yet (tracked in `wax_sender_key_sent`). When a new own key was created,
all delivery tracking for the group is reset so everyone gets it again.

On receipt, `session.processSKDM` (`internal/client/message.go`) stores it with
`ProcessDistribution`. A distribution for a key id already present is ignored,
so a replayed distribution can never rewind the chain or drop skipped keys.

### Chain and messages

```
messageSeed(n) = HMAC-SHA256(chainSeed(n), 0x01)
chainSeed(n+1) = HMAC-SHA256(chainSeed(n), 0x02)
(iv, key)      = HKDF(messageSeed, info "WhisperGroup", 48)  ->  iv = [0:16], key = [16:48]
```

`EncryptGroup` produces the `skmsg`:

```
+------+------------------------------------------+----------------+
| 0x33 | SenderKeyMessage { id, iteration, ct }   | signature (64) |
+------+------------------------------------------+----------------+
          signature = Sign(signingKey, 0x33 || protobuf)
```

`DecryptGroup` finds the state by key id (`ErrNoSession` if unknown), verifies
the signature, then derives the message key with the same rules as 1:1
chains: earlier iterations come from at most 2000 skipped keys, and a jump of
more than 2000 iterations is rejected. The record changes only on success.

The stanza carries one `<enc v="2" type="skmsg">` for the whole group next to
the `<participants>` with the per-device distribution messages.

## 7. Message padding

Before any Signal encryption (1:1 and `skmsg`) the serialized `wae2e.Message`
is padded by `padMessage` (`internal/client/address.go`): `n` bytes, each with
the value `n`, where `n` is the low nibble of a random byte, and `15` if that
nibble is `0`. So `1 <= n <= 15`. `unpadMessage` checks that the last `n` bytes
all equal `n` and strips them. A padding error after successful decryption
marks the event handled so it is not redelivered forever. This padding is on
top of the PKCS#7 padding that AES-CBC adds.

## 8. Storage in PostgreSQL

### Addresses

A session is keyed by a Signal address `name.device`
(`signalAddress` in `internal/client/address.go`; LID handling is covered in
[06-users-and-lids.md](06-users-and-lids.md)). `name` is the JID user with
a domain suffix so phone numbers and LIDs never collide:

| JID | Address |
|---|---|
| `905550000001:3@s.whatsapp.net` | `905550000001.3` |
| `1234:3@lid` | `1234_1.3` |
| hosted / hosted LID | `user_128.N` / `user_129.N` |

When the PN to LID mapping of a user becomes known, `Store.MigrateSessions`
moves sessions and identities to the LID name, re-sealing each record for its
new address.

### Tables

| Table | Key | Content | Sealed |
|---|---|---|---|
| `wax_sessions` | instance, address | `signal.Record` (protobuf `signalstore.SessionRecord`) | yes |
| `wax_identities` | instance, address | remote identity public key (32 bytes) | no (public) |
| `wax_sender_keys` | instance, group, sender address | `signal.SenderKeyRecord` | yes |
| `wax_sender_key_sent` | instance, group, device address | who has our sender key | no |
| `wax_prekeys` | instance, key id | one-time prekey private key | yes |
| `wax_event_buffer` | instance, ciphertext hash | decrypted plaintext until handled | yes |

Serialized records contain chain keys, ratchet private keys and signing seeds,
so they are sealed (`internal/store/sealer.go`):

```
dataKey = HKDF-SHA256(masterKey, info "wax/store/secrets/v1", 32)
sealed  = 0x01 || nonce (12, random) || AES-256-GCM(dataKey, nonce, plaintext, AD)
AD      = "table|column|instance[|rowKey...]"   e.g. "wax_sessions|record|inst1|1234_1.3"
```

The associated data binds each value to its row and column: a ciphertext
copied elsewhere fails to open (`ErrSealed`).

### Transactions and identity trust

- `Client.encryptForDevice` and `session.decryptEnc` take an in-process lock
  per address and run in one transaction; inside a transaction `LoadSession`
  and `LoadSenderKey` use `SELECT ... FOR UPDATE`. Session update, prekey
  deletion, identity update and the decrypted plaintext (`wax_event_buffer`)
  commit together, so a crash between decrypting and acknowledging cannot lose
  a message: the redelivered ciphertext finds its plaintext by hash.
- Identities are trust on first use. A changed identity on decryption is
  accepted, stored and reported as an `IdentityChanged` event. A
  `<notification type="encrypt"><identity/></notification>` deletes all
  sessions and identities of that user (`Store.DeleteUserSessions`).
- Starting a session from a bundle stores the bundle's identity key.

## 9. Decryption failures

When decryption fails (no session, missing prekey, bad MAC, unknown sender
key), `session.handleEnc` sends a retry receipt, acknowledges the stanza and
emits `UndecryptableMessage`. From the second retry on, the receipt includes a
fresh prekey bundle so the sender can build a new session. Senders answer
retries from `wax_sent_messages`. Details are in
[05-messages.md](05-messages.md).

## 10. Not implemented or not verified

- Only protocol version 3; older versions are rejected.
- No signed prekey rotation; only signed prekey id `1` is accepted.
- No user-facing identity verification (safety numbers); trust is automatic.
- Interoperability is checked with published known-answer values, not by
  running libsignal: `internal/keys/keys_test.go` verifies a libsignal
  signature, and `internal/signal/signal_test.go` checks the responder side
  of X3DH (`initBob`) against a libsignal-protocol-java chain key. The
  initiator side is covered by round-trip tests only (the test file explains
  why the upstream initiator values cannot be reproduced).

## In the code

| Concept | Where |
|---|---|
| X25519 key pairs, DH | `internal/keys/keys.go` |
| Curve25519 (XEdDSA-format) signatures | `internal/keys/signing.go` (`SigningKeyFromSeed`, `Sign`, `Verify`) |
| HKDF, HMAC, AES-CBC, AES-GCM | `internal/cryptoutil/cryptoutil.go` |
| KDF constants, chain and message keys | `internal/signal/kdf.go` |
| X3DH, encrypt, decrypt | `internal/signal/cipher.go` (`ProcessBundle`, `initAlice`, `initBob`, `Encrypt`, `DecryptMessage`, `DecryptPreKeyMessage`) |
| Session state, limits, serialization | `internal/signal/session.go` |
| Wire formats, MAC | `internal/signal/message.go`, `proto/signalwire/signalwire.proto` |
| Sender keys | `internal/signal/senderkey.go` |
| Bundle fetch and parsing | `internal/client/usync.go` (`fetchBundles`, `parseBundle`) |
| Encrypt call sites | `internal/client/send.go` (`encryptForDevices`, `encryptForDevice`, `senderKeyEncrypt`, `sendGroup`) |
| Decrypt call sites | `internal/client/message.go` (`decryptEnc`, `decryptSession`, `decryptGroupEnc`, `processSKDM`) |
| Addresses, padding | `internal/client/address.go` |
| Sealed storage | `internal/store/sealer.go`, `internal/store/signal.go`, `internal/store/senderkeys.go` |
| Retry receipts | `internal/client/retry.go` |
