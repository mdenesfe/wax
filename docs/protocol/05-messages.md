# Messages

A WhatsApp message is a protobuf (`wae2e.Message`) that is padded, encrypted once per recipient device with Signal (or once per group with a sender key), and carried in a `<message>` stanza. This chapter follows that path in both directions as wax implements it: how a phone number becomes a LID, how device lists are fetched and checked, how the per-device `<enc>` nodes and the stanza around them are built, what the server's `<ack>` returns, and, on the receiving side, how a stanza is parsed, decrypted transactionally, unwrapped, stored and acknowledged so that nothing is lost when the process crashes. It ends with the content model wax reads and writes, and with presence, typing and call signalling. Node examples use the form printed by `wabin.Node.Format` (attributes sorted, binary content shown as a byte count and hash); all JIDs and values are placeholders.

## Addresses used in this chapter

| Form | Example | Meaning |
|---|---|---|
| Phone number (PN) user | `905550000001@s.whatsapp.net` | an account, addressed by phone number |
| LID user | `1234@lid` | the same account under its LID |
| Device | `1234:5@lid` | device 5 of that account (device 0 is the primary phone) |
| Group | `120363000000000001@g.us` | a group chat |

Signal sessions are keyed by a string address, `name.device`, built by `signalAddress` in [internal/client/address.go](../../internal/client/address.go). The name is the user part with a domain suffix, so a PN and a LID with the same digits never collide: `_1` for `@lid`, `_128` for hosted devices, `_129` for hosted LID devices. Device 5 of `1234@lid` is `1234_1.5`.

wax prefers LID identities for encryption. When a PN to LID mapping is learned (`learnLID` in [internal/client/usync.go](../../internal/client/usync.go)), the mapping is stored and existing PN sessions are moved to the LID name (`Store.MigrateSessions`).

## Sending a 1:1 message

Entry points are `Client.SendMessage`, `SendMessageWithID` and `SendText` in [internal/client/send.go](../../internal/client/send.go). The recipient must be a user or group JID, never a device JID. `sendDirect` does the rest.

### 1. Resolve the LID

If the recipient is a PN JID, `resolveLID` looks up the stored mapping. When none is stored, it runs a usync query with mode `full` and context `background`, asking for both `devices` and `lid`:

```xml
<iq id="..." to="s.whatsapp.net" type="get" xmlns="usync">
  <usync context="background" index="0" last="true" mode="full" sid="...">
    <query>
      <devices version="2"/>
      <lid/>
    </query>
    <list>
      <user jid="905550000001@s.whatsapp.net"/>
    </list>
  </usync>
</iq>
```

The `<user>` in the response carries `<lid val="1234@lid"/>`. The mapping is stored, the device list in the same response is cached, and the message is addressed to the LID. If the server returns no LID, the send fails with `ErrNotOnWhatsApp`. The original PN is kept and sent as `peer_recipient_pn`. wax also refuses to send 1:1 while its own LID is unknown.

### 2. Fetch device lists

`Client.devices` ([internal/client/usync.go](../../internal/client/usync.go)) is called for two users: the recipient (LID) and our own account (LID), because our other devices must receive a copy. Missing entries are fetched with one usync query, mode `query`, context `message`, query `<devices version="2"/>`. Each `<device id="N"/>` under `<devices><device-list>` becomes a device JID; `is_hosted="true"` moves it to the hosted server (`ServerHosted` or `ServerHostedLID`).

Lists are cached in memory for 10 minutes (`deviceCacheTTL`). The cache entry of a user is dropped when:

- a `devices` notification arrives for that user,
- an `account_sync` notification lists `devices` (our own lists),
- an `encrypt` notification with `<identity>` reports a re-registration (`onIdentityChange`, which also deletes that user's sessions),
- the server's `phash` disagrees with ours after a send (see below).

### 3. Build the payloads

Two plaintexts are marshalled:

- the message itself, for the recipient's devices;
- a `DeviceSentMessage` wrapper `{destinationJID: <recipient LID>, message: <msg>}`, plus the original `messageContextInfo`, for our own other devices.

Before anything is sent, the plain message is stored with `Store.PutSentMessage` under `(chat, id)`, so a later retry receipt can always be answered. Copies older than 7 days are purged on login (`Store.PurgeOld`).

Every plaintext is padded before encryption (`padMessage` in [internal/client/address.go](../../internal/client/address.go)): 1 to 15 bytes, each equal to the padding length. The receiver strips it with `unpadMessage`.

### 4. Encrypt per device

`encryptForDevices` drops our own sending device and all hosted devices (`filterTargets`), maps each remaining device to its LID address when the mapping is known (`encryptionJIDs`), and checks which addresses already have a session (`Store.HasSessions`). For the rest, one `encrypt` IQ fetches prekey bundles (`fetchBundles`):

```xml
<iq id="..." to="s.whatsapp.net" type="get" xmlns="encrypt">
  <key>
    <user jid="1234:5@lid" reason="identity"/>
  </key>
</iq>
```

`parseBundle` reads `<registration>` (4 bytes), `<identity>` (32), `<skey>` with `<id>` (3), `<value>` (32), `<signature>` (64), and an optional one-time `<key>`. A bundle with an `<error>` child is skipped.

Each device is then encrypted in its own database transaction (`encryptForDevice`): load the session row, process the bundle if one was fetched (storing the peer identity), encrypt, save. The result is an `<enc v="2">` with `type="pkmsg"` when the Signal message is a prekey message (a new session) and `type="msg"` otherwise. Media messages also get a `mediatype` attribute on each `<enc>`. A device that fails is logged and skipped; the send fails only if no device at all could be encrypted for.

### 5. The stanza

```xml
<message id="3EB0..." peer_recipient_pn="905550000001@s.whatsapp.net" to="1234@lid" type="text">
  <participants>
    <to jid="1234@lid">
      <enc type="msg" v="2"><!-- 57 bytes sha256:... --></enc>
    </to>
    <to jid="1234:5@lid">
      <enc type="pkmsg" v="2"><!-- 240 bytes sha256:... --></enc>
    </to>
    <to jid="5678:3@lid">
      <enc type="msg" v="2"><!-- 89 bytes sha256:... --></enc>
    </to>
  </participants>
  <device-identity><!-- 140 bytes sha256:... --></device-identity>
  <tctoken><!-- 24 bytes sha256:... --></tctoken>
</message>
```

| Element / attribute | When | Value |
|---|---|---|
| `id` | always | message id (see below) |
| `type` | always | `reaction` for a reaction, `media` for media, otherwise `text` (`messageType`, computed on the unwrapped content) |
| `to` | always | recipient LID user JID |
| `peer_recipient_pn` | the caller passed a PN | the original PN JID |
| `<participants>` / `<to jid>` | always | one `<enc>` per device; our own devices (`5678:3@lid` above) get the DeviceSentMessage |
| `<device-identity>` | any `<enc>` is a `pkmsg` | our signed device identity (`Device.Account`) from pairing |
| `<tctoken>` | we hold a valid privacy token of the recipient | the token bytes |

`mediatype` values (`mediaType`): `image`, `sticker`, `document`, `audio`, `ptt` (audio with `PTT` set), `video`, `gif` (video with `gifPlayback`).

### Privacy tokens (`tctoken`)

Implemented in [internal/client/privacy.go](../../internal/client/privacy.go). A contact's token arrives in a `privacy_token` notification (`<tokens><token type="trusted_contact" t="...">bytes</token></tokens>`) or in history sync conversations, and is stored under the contact's LID when known. A token is valid while its timestamp falls into one of the last four weekly buckets (`tokenCutoff`). After a successful 1:1 send, wax issues its own token to the recipient in the background, at most once per weekly bucket:

```xml
<iq id="..." to="s.whatsapp.net" type="set" xmlns="privacy">
  <tokens>
    <token jid="1234@lid" t="1760000000" type="trusted_contact"/>
  </tokens>
</iq>
```

Expired tokens are pruned on login (`pruneTokens`). Group messages carry no `tctoken`.

### Message ids

Two generators exist, both producing `3EB0` followed by 18 uppercase hex characters:

- `newMessageID` ([internal/client/address.go](../../internal/client/address.go)): the first 9 bytes of SHA-256 over the Unix time (8 bytes big-endian), our user plus `@c.us`, and 16 random bytes.
- `GenerateMessageID` ([internal/client/send.go](../../internal/client/send.go)): 9 random bytes. The REST outbox ([api/handlers.go](../../api/handlers.go), [internal/manager/outbox.go](../../internal/manager/outbox.go)) generates the id once and reuses it on every attempt, so a resend after a crash is the same message for recipients.

### The server ack

`sendMessageNode` waits up to 60 seconds (`sendAckTimeout`) for an `<ack>` with the same `id`. It reads three attributes:

| Attribute | Use |
|---|---|
| `t` | server timestamp, returned as `SendResult.Timestamp` and stored with the message |
| `phash` | the server's hash of the recipient device list |
| `error` | if present and not `0`, the send fails with `ErrServerRejected` |

`phash` is computed locally by `participantListHash`: each device is rendered as `user.agent:device@server`, the strings are sorted and concatenated, hashed with SHA-256, and the result is `"2:"` plus unpadded base64 of the first 6 bytes. For 1:1 sends it is not sent, only compared: if the ack carries a different value, the cached lists of the recipient and of our own account are dropped so the next send fetches fresh ones. The message that was just sent is not re-sent.

After a successful ack the message is stored as a live outgoing message (`storeLiveMessage`) under the LID chat. The whole sequence from device lookup to ack runs under `Client.sendMu`, so sends of one client are serialized.

## Sending to a group

`sendGroup` in [internal/client/send.go](../../internal/client/send.go) encrypts the content once with our sender key and distributes that key over 1:1 sessions only to devices that do not have it yet.

1. **Group info and addressing mode.** `groupInfo` returns participants and `addressing_mode` (`lid` or `pn`). Our sender identity is our LID device (`ownDeviceForGroups`, falling back to the PN device). In a `pn` group our own user is listed by PN.
2. **Device lists** of every participant plus ourselves, via the same cached usync query.
3. **Sender key encryption** (`senderKeyEncrypt`): in one transaction, load or create our sender key record for `(group, our address)`, create a distribution message (SKDM), encrypt the padded plaintext (`skmsg`), save. If the key was just created, the list of devices known to have it is cleared.
4. **Who needs the key** (`senderKeyTargets`): devices (after `filterTargets`) whose address is not in `Store.SenderKeyRecipients`.
5. **Distribution:** a `wae2e.Message{senderKeyDistributionMessage: {groupID, axolotlSenderKeyDistributionMessage}}` is encrypted per device with `encryptForDevices`, exactly as for 1:1 (`pkmsg`/`msg`, no `mediatype`).

```xml
<message addressing_mode="lid" id="3EB0..." phash="2:AbCdEfGh" to="120363000000000001@g.us" type="text">
  <participants>
    <to jid="1234:5@lid">
      <enc type="pkmsg" v="2"><!-- 210 bytes sha256:... --></enc>
    </to>
  </participants>
  <enc type="skmsg" v="2"><!-- 75 bytes sha256:... --></enc>
  <device-identity><!-- 140 bytes sha256:... --></device-identity>
</message>
```

Differences from 1:1: `phash` is sent on the stanza (computed over all devices returned by the device lookup), `addressing_mode="lid"` is added for LID groups, `<participants>` is omitted when everyone already has the key, and the content travels in a single `<enc type="skmsg">` (with `mediatype` for media). No DeviceSentMessage is built: our own other devices receive the SKDM like any member and decrypt the `skmsg` themselves.

Only after the ack are the devices that got an SKDM recorded as having the key (`Store.MarkSenderKeySent`); a failed send therefore redistributes next time. If the ack's `phash` differs, the cached group info and every participant's device list are dropped.

When a `w:gp2` notification reports `remove` or `leave`, `rotateSenderKey` ([internal/client/group.go](../../internal/client/group.go)) deletes our sender key and its recipient list, so the next message creates a new key that the departed member never receives.

## Receiving messages

`handleNode` ([internal/client/handlers.go](../../internal/client/handlers.go)) routes `<message>` to `handleMessage` ([internal/client/message.go](../../internal/client/message.go)). All stanzas are handled one at a time on the session's handler goroutine, in arrival order.

### Parsing (`parseMessageInfo`)

| Case | Chat | Sender | Alternate identity |
|---|---|---|---|
| `from` is a group or broadcast | `from` | `participant` (required) | `participant_pn` or `participant_lid` |
| `from` is our own user (PN or LID) | `recipient` if present, else `from` | `from` | none |
| anything else | `from` without device | `from` | `sender_lid` (PN sender) or `sender_pn` (LID sender) |

`id` is required; `t` is the timestamp, `notify` the sender's push name, `type` is kept (`text`, `media`, `peer_msg`, ...). `IsFromMe` is set for our own account. A stanza that cannot be parsed is answered with `<ack error="487">`.

`sessionAddress` then learns the PN/LID mapping from the alternate identity and picks the address of the Signal session: the sender itself if it is not a PN, else its LID from the stanza or the store, else the PN.

### Stanza shapes that are not decrypted

- Newsletter chats and bot senders: deferred (see below), not implemented.
- No `<enc>` but an `<unavailable>` child: logged and acked. wax does not request the content in another way.
- No `<enc>` at all otherwise: `<ack error="488">`.

### Decryption order

The `<enc>` children are handled in document order by `handleEnc`, so an SKDM carried in a `pkmsg` is stored before a following `skmsg` in the same stanza is decrypted.

| `type` | Handler | Notes |
|---|---|---|
| `pkmsg`, `msg` | `decryptEnc` | pairwise Signal session |
| `skmsg` (groups only) | `decryptGroupEnc` | sender key of `(group, sender address)` |
| anything else | none | counted as unsupported |

`decryptEnc` runs in one database transaction: `Store.LoadDecryptState` reads the buffered event, session row (locked `FOR UPDATE`) and stored identity in one round trip; a `pkmsg` consumes its one-time prekey (`RemovePreKey`) in the same transaction; then the identity, session and decrypted plaintext are written together. Identity keys follow trust on first use; a changed key is accepted and reported as an `IdentityChanged` event. `decryptGroupEnc` works the same way with the sender key record. A per-address in-process lock (`addrLocks`) prevents two goroutines racing on one session.

After decryption the padding is removed, the `v` attribute must be `"2"`, and the plaintext is decoded as `wae2e.Message`. If it carries a `senderKeyDistributionMessage`, `processSKDM` stores the sender key (the group id in the SKDM wins over the stanza's chat). A message that carries only key distribution or `messageContextInfo` (`hasContent` is false) is not dispatched as a message.

### The event buffer (crash safety)

The decrypted, still padded plaintext is stored in `wax_event_buffer` ([internal/store/signal.go](../../internal/store/signal.go)) in the same transaction that advances the session. It is keyed by a hash of the ciphertext:

- `pkmsg`/`msg`: SHA-256 of `ciphertext 0x00 type 0x00 address` (`eventHash`);
- `skmsg`: SHA-256 of `ciphertext 0x00 "skmsg" 0x00 group 0x00 address`.

| Buffer state | Meaning | On redelivery |
|---|---|---|
| no row | never seen | decrypt normally |
| row with plaintext | decrypted, not yet fully handled | reuse the plaintext, no second decryption |
| row with `NULL` plaintext | handled | treated as a duplicate and receipted again |

When dispatch succeeds, `MarkEventHandled` clears the plaintext and keeps the hash for duplicate detection. Handled hashes older than 30 days are purged on login; unhandled rows are never purged. If dispatch fails (for example the database is unavailable), the stanza is left unacknowledged, the server redelivers it, and the buffered plaintext is used even though the Signal ratchet has already moved on.

### Outcome per stanza

| Result | Reply |
|---|---|
| every `<enc>` decrypted and dispatched, or a duplicate | delivery receipt (doubles as the ack) |
| decrypted but invalid content (bad version or protobuf) | `<ack error="491">`; the hash is marked handled |
| bad padding after decryption | the hash is marked handled so it does not loop |
| decryption failed | retry receipt, then a plain `<ack>`, and an `UndecryptableMessage` event |
| only unsupported `enc` types | deferred |
| dispatch failed, or the session is shutting down | nothing; the server redelivers |

A decryption failure stops processing of the remaining `<enc>` children of that stanza.

### Unwrapping (`dispatchMessage`, `Unwrap`)

1. **DeviceSentMessage:** a message sent by our own phone wraps the content. The inner message becomes the content and `destinationJID` becomes the chat.
2. **Containers:** `Unwrap` strips up to four nested levels of `ephemeralMessage`, `viewOnceMessage`, `viewOnceMessageV2`, `viewOnceMessageV2Extension` and `documentWithCaptionMessage`.
3. **View once** is reported if any view once wrapper was seen or the image, video or audio message has its own `viewOnce` flag.

The `Message` event ([internal/client/events.go](../../internal/client/events.go)) carries `Raw` (as decrypted), `Message` (unwrapped), `ViewOnce` and `RetryCount` (the `<enc count>` attribute). The stored copy keeps the ephemeral and view once wrappers (only the DeviceSentMessage is removed), so the view once flag is not lost.

### Protocol messages

`protocolMessage` content is acted on only when `IsFromMe` is set, that is, when it comes from our own account:

| Field | Action |
|---|---|
| `appStateSyncKeyShare` | keys stored (`handleKeyShare`) before the stanza is acknowledged |
| `historySyncNotification` | queued durably (`enqueueHistory`); a background worker downloads it later |
| `peerDataOperationRequestResponseMessage` | processed in a background goroutine (`handlePeerDataResponse`); only app state fatal recovery responses are used |

wax itself sends one kind of protocol message: a peer data operation request for app state recovery (`sendPeerMessage` in [internal/client/appsync.go](../../internal/client/appsync.go)), a `<message category="peer" push_priority="high" type="text">` to our own primary device with a `<meta appdata="default"/>` child.

Revokes (`REVOKE`) and edits (`MESSAGE_EDIT`) are applied to the stored message they target (`key.ID`) and are not stored as rows of their own (`applyChange` in [internal/client/history.go](../../internal/client/history.go), `Store.ChangeMessage`). Only the target's own sender may change it: our own messages when the change is from us, otherwise the same user as the stored sender (any device; a sender seen under its phone number in one place and its LID in the other is not matched). A revoke turns the target into the system message `stub:1` (name `revoke`) and drops its content. An edit replaces the content of a text message with `protocolMessage.editedMessage` and sets `edited_at` (from `timestampMS` when present); edits of media captions are not applied. Changes to messages wax never stored are ignored. Both are still emitted as `message` events of type `protocol`. wax has no API to send revokes, edits or reactions. Other protocol message types are emitted as events but not stored.

### Storage and push names

`storeLiveMessage` ([internal/client/history.go](../../internal/client/history.go)) writes the message and updates the chat's last message time in one batch. The stored `type` is the `mediatype` value for media, otherwise `messageType` (`reaction` or `text`); revokes and edits are applied instead of stored (see above). Inserting an existing `(chat, id)` is a no-op. A non-empty `notify` from someone else is stored as their push name (`Store.PutPushName`) when it differs from the last value seen in memory.

### Receipts we send

`sendMessageReceipt` copies `id`, sets `to` to the stanza's `from`, and copies `recipient` and `participant`:

| `type` | When |
|---|---|
| (absent) | delivered while we announced ourselves available |
| `inactive` | delivered while not available (the default) |
| `sender` | the message came from our own account |
| `peer_msg` | from our own account with stanza `type="peer_msg"` |
| `read` | only explicitly, via `Client.MarkRead` |

`MarkRead` ([internal/client/mediaops.go](../../internal/client/mediaops.go)) sends one receipt for several ids: the first in `id`, the rest as `<list><item id/></list>`, with `t` and, in groups, `participant` set to the author:

```xml
<receipt id="3EB0AAAA" participant="1234@lid" t="1760000000" to="120363000000000001@g.us" type="read">
  <list>
    <item id="3EB0BBBB"/>
  </list>
</receipt>
```

Stanzas that are not receipted are acknowledged with `<ack class="<tag>" id to [participant recipient] [error]>`; for tags other than `message` the `type` attribute is copied too.

### Receipts we receive

`handleReceipt` ([internal/client/retry.go](../../internal/client/retry.go)) emits a `Receipt` event with the ids (the `id` attribute plus any `<list><item>`), chat, sender (`participant` if present) and `type` (empty means delivered; `read`, `played`, `sender`, `retry`, ...). Aggregated group receipts of the form `<participants key="<msg id>"><user jid t/></participants>` become one event per user. Each receipt is acknowledged with `<ack class="receipt">` carrying `id`, `to`, `type` and `participant`.

## Retry receipts

### Asking for a resend

When decryption fails, `sendRetryReceipt` asks the sender to re-encrypt:

```xml
<receipt id="3EB0..." to="1234:5@lid" type="retry">
  <retry count="1" id="3EB0..." t="1760000000" v="1"/>
  <registration><!-- 4 bytes sha256:... --></registration>
</receipt>
```

`recipient` and `participant` are copied from the message; `category="peer"` is added for our own `peer_msg` stanzas. The count lives in memory per message id, seeded from the `<enc count>` of a redelivered message. From count 2 on, a `<keys>` child is attached with `<type>`, `<identity>`, a fresh one-time `<key>`, the signed `<skey>` and `<device-identity>`, so the sender can build a new session from scratch. At count 5 wax stops asking (`maxRetryReceipts`). The original stanza is then acked normally.

### Answering one

A retry receipt from another device is handled off the handler goroutine (`handleRetryReceipt`); the `Receipt` event and its ack follow the resend. Steps:

1. Refuse when `<retry count>` is 10 or more.
2. Load the stored plaintext from `wax_sent_messages`, trying the LID chat when the receipt names a PN chat.
3. Build the payload (`retryPayload`): in groups, the content plus a fresh SKDM (the device lost our key); to our own devices, a DeviceSentMessage; otherwise the content.
4. Choose the session (`retryBundle`): a `<keys>` bundle in the receipt starts a new session; otherwise from count 2 on a fresh bundle is fetched; otherwise the existing session is used.
5. Send one `<enc>` with a `count` attribute directly to the failing device:

```xml
<message device_fanout="false" id="3EB0..." t="1760000000" to="1234:5@lid" type="text">
  <enc count="2" type="pkmsg" v="2"><!-- 230 bytes sha256:... --></enc>
  <device-identity><!-- 140 bytes sha256:... --></device-identity>
</message>
```

`device_fanout="false"` is set for non-group chats; `participant` and `recipient` are copied from the receipt. Group retries are sent as a pairwise `pkmsg`/`msg` too, not as `skmsg`. The resend does not set `mediatype` and does not wait for an ack. Copies older than 7 days are purged, so older messages cannot be resent.

## Deferred stanzas

Leaving a stanza unacknowledged is not an option: the server delivers its offline queue in windows and stops at an unacknowledged stanza. `deferStanza` ([internal/client/handlers.go](../../internal/client/handlers.go)) therefore stores what wax cannot process yet (newsletter and bot messages, messages with only unsupported `enc` types, unknown notification types) in `wax_deferred_stanzas` ([internal/store/deferred.go](../../internal/store/deferred.go)), encrypted at rest, and acks it only after the write succeeded. Storing the same `(tag, id)` twice is a no-op.

On every login, before live stanzas, `replayDeferred` ([internal/client/replay.go](../../internal/client/replay.go)) runs up to 500 stored stanzas oldest first through the normal handler. During replay no acks are sent (they were sent when stored), but delivery receipts are. A stanza that is still unsupported stays in the table; the rest are deleted.

## Message content model

wax uses a subset of WhatsApp Web's E2E protobuf, defined in [internal/proto/wae2e/wae2e.proto](../../internal/proto/wae2e/wae2e.proto). Field numbers match WhatsApp Web, and unknown fields survive decoding and re-encoding.

| Field (number) | Use in wax |
|---|---|
| `conversation` (1) | plain text |
| `senderKeyDistributionMessage` (2) | group key distribution |
| `imageMessage` (3), `videoMessage` (9), `audioMessage` (8), `documentMessage` (7), `stickerMessage` (26) | media |
| `extendedTextMessage` (6) | text with `contextInfo` |
| `protocolMessage` (12) | see above |
| `deviceSentMessage` (31) | copy for our own devices |
| `messageContextInfo` (35) | copied onto the DeviceSentMessage |
| `viewOnceMessage` (37), `viewOnceMessageV2` (55), `viewOnceMessageV2Extension` (59), `ephemeralMessage` (40), `documentWithCaptionMessage` (53) | wrappers removed by `Unwrap` |
| `reactionMessage` (46) | received only: `key.ID` of the target and the emoji in `text` |
| `editedMessage` (58) | declared, not interpreted |

**Context info.** `ContextInfo` carries `stanzaID` and `participant` of a quoted message, `quotedMessage`, `mentionedJID`, `forwardingScore` and `isForwarded`. `ContextOf` and `SetContext` in [internal/client/context.go](../../internal/client/context.go) read and write it on extended text, image, video, audio, document and sticker messages; setting a context on a plain `conversation` converts it to an `extendedTextMessage`. The API builds replies from the stored quoted message (unwrapped, with its own context removed), mentions as non-device user JIDs, and forwards by setting `isForwarded` and incrementing `forwardingScore`; view once messages cannot be forwarded ([api/handlers.go](../../api/handlers.go)).

**Media.** `SendMedia` ([internal/client/mediaops.go](../../internal/client/mediaops.go)) uploads the file, then fills `URL`, `directPath`, `mediaKey`, `fileSHA256`, `fileEncSHA256`, `fileLength`, `mimetype` and `mediaKeyTimestamp`, plus caption, file name, thumbnail, size, duration and (voice notes, `PTT` set) waveform where given. Upload and download are covered in [10-media.md](10-media.md).

**JSON view.** `DescribeMessage` in [internal/manager/events.go](../../internal/manager/events.go) turns an unwrapped message into the webhook and API shape: type `text`, `reaction`, `protocol` or a media kind (`image`, `video`, `audio`, `voice`, `document`, `sticker`), text or caption, quoted message, mentions, forwarding and view once.

**System ("stub") messages.** History sync entries without a message body are stored with type `stub:<n>` (the `WebMessageInfo` stub type) and their parameters as a JSON array (`historyMessage` in [internal/client/history.go](../../internal/client/history.go)). `DescribeStub` in [internal/manager/stub.go](../../internal/manager/stub.go) names common types (group changes, missed calls, security notices) and lists parameters that parse as user JIDs as people. Stub rows come from history sync, plus `stub:1` for a message its sender deleted while wax was running (see revokes above).

## Presence, chat state and calls

Implemented in [internal/client/presence.go](../../internal/client/presence.go) and [internal/client/call.go](../../internal/client/call.go).

| Direction | Node | Notes |
|---|---|---|
| out | `<presence name="..." type="available">` / `type="unavailable"` | requires a push name; while available, delivery receipts have no `type` |
| out | `<presence to="1234@lid" type="subscribe"/>` | asks for a contact's presence |
| out | `<chatstate to="..."><composing/></chatstate>` | `<composing media="audio"/>` while recording, `<paused/>` to stop |
| in | `<presence from type last>` | `Presence` event; `last` becomes the last seen time |
| in | `<chatstate from participant>` | `ChatPresence` event with the child tag (`composing`, `paused`) and `media` |
| in | `<call from t platform>` | acked; first child tag is the event type (`offer`, `accept`, `reject`, `terminate`, ...), with `call-id`, `call-creator`, `group-jid`, `reason` and video detection |
| out | `<call from to id><reject call-creator call-id count="0"/></call>` | `RejectCall`; `from` is our LID when the caller is a LID |

wax is not a calling device: it cannot accept or place calls.

## In the code

| Topic | Location |
|---|---|
| 1:1 and group send, per-device encryption, ack | [internal/client/send.go](../../internal/client/send.go): `sendDirect`, `sendGroup`, `encryptForDevices`, `encryptForDevice`, `senderKeyEncrypt`, `sendMessageNode`, `messageType`, `mediaType` |
| LID resolution, device lists, prekey bundles | [internal/client/usync.go](../../internal/client/usync.go): `resolveLID`, `devices`, `learnLID`, `fetchBundles`, `parseBundle` |
| Signal addresses, phash, message ids, padding | [internal/client/address.go](../../internal/client/address.go): `signalAddress`, `participantListHash`, `newMessageID`, `padMessage` |
| Receiving, decryption, unwrapping, receipts | [internal/client/message.go](../../internal/client/message.go): `parseMessageInfo`, `handleMessage`, `handleEnc`, `decryptEnc`, `decryptGroupEnc`, `dispatchMessage`, `Unwrap`, `sendMessageReceipt` |
| Retry receipts, incoming receipts | [internal/client/retry.go](../../internal/client/retry.go): `sendRetryReceipt`, `handleReceipt`, `handleRetryReceipt`, `retryPayload`, `retryBundle` |
| Acks, deferral, notifications | [internal/client/handlers.go](../../internal/client/handlers.go): `nack`, `deferStanza`, `handleNotification` |
| Deferred replay | [internal/client/replay.go](../../internal/client/replay.go): `replayDeferred`; [internal/store/deferred.go](../../internal/store/deferred.go) |
| Event buffer, sent copies | [internal/store/signal.go](../../internal/store/signal.go): `LoadDecryptState`, `PutBufferedEvent`, `MarkEventHandled`, `PurgeOld`, `PutSentMessage` |
| Privacy tokens | [internal/client/privacy.go](../../internal/client/privacy.go): `privacyToken`, `onPrivacyToken`, `issueToken` |
| Context info | [internal/client/context.go](../../internal/client/context.go): `ContextOf`, `SetContext` |
| Media send, read receipts | [internal/client/mediaops.go](../../internal/client/mediaops.go): `SendMedia`, `MarkRead` |
| Live storage, stubs from history | [internal/client/history.go](../../internal/client/history.go): `storeLiveMessage`, `historyMessage` |
| Sender key rotation | [internal/client/group.go](../../internal/client/group.go): `rotateSenderKey`, `ownDeviceForGroups` |
| Presence, chat state, calls | [internal/client/presence.go](../../internal/client/presence.go), [internal/client/call.go](../../internal/client/call.go) |
| Events | [internal/client/events.go](../../internal/client/events.go): `Message`, `UndecryptableMessage`, `Receipt` |
| JSON view, stubs | [internal/manager/events.go](../../internal/manager/events.go): `DescribeMessage`; [internal/manager/stub.go](../../internal/manager/stub.go): `DescribeStub` |
| Protobuf schema | [internal/proto/wae2e/wae2e.proto](../../internal/proto/wae2e/wae2e.proto) |
