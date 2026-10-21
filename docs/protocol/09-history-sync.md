# 09. History sync

After a companion is paired, the primary device sends it past conversations: chat metadata, messages, contacts' push names, phone number to LID mappings and a few account values. The phone does not send this data inline over the socket. It uploads compressed, encrypted chunks to the media servers and sends the companion one end-to-end encrypted protocol message per chunk, describing where the chunk is and how to decrypt it. Some notifications instead carry the chunk inline. This document describes the notification, how wax downloads, decrypts and inflates a chunk, which parts of the `HistorySync` payload it uses, and how it stores the result durably and idempotently through a queue and a background worker.

## Overview

```
phone                        server / media CDN                     wax
  |                                  |                               |
  |-- upload encrypted chunk ------->|                               |
  |-- Message{protocolMessage:       |                               |
  |     historySyncNotification} ---(Signal, from our own account)->|
  |                                  |      dispatchMessage          |
  |                                  |      enqueueHistory (DB) -----+--> wax_history_queue
  |                                  |      ack the message          |
  |                                  |                               |
  |                                  |<-- download (md-msg-hist) ----+  historyWorker
  |                                  |                               |  decrypt, zlib inflate,
  |                                  |                               |  decode HistorySync,
  |                                  |                               |  store in one batch,
  |                                  |                               |  delete job, emit event
```

## The notification

The notification is a protocol message inside a normal encrypted `<message>`:

```
Message.protocolMessage.historySyncNotification = {
  fileSHA256, fileLength, mediaKey, fileEncSHA256, directPath,
  syncType, chunkOrder, originalMessageID, progress,
  oldestMsgInChunkTimestampSec, initialHistBootstrapInlinePayload,
  peerDataRequestSessionID, encHandle
}
```

`session.dispatchMessage` only accepts it when the message is from our own account (`info.IsFromMe`). It calls `session.enqueueHistory`, which serializes the notification and stores it in `wax_history_queue` (sealed at rest) before the carrying message is acknowledged. If storing fails, the message is not acknowledged and the server redelivers it. The notification itself is not written to `wax_messages` (protocol messages other than revokes and edits are not stored).

Fields wax uses from the notification: `directPath`, `mediaKey`, `fileEncSHA256`, `fileSHA256` and `initialHistBootstrapInlinePayload`. The sync type, chunk order and progress that wax reports are taken from the decoded payload, not from the notification. `fileLength`, `originalMessageID`, `oldestMsgInChunkTimestampSec`, `peerDataRequestSessionID` and `encHandle` are decoded but unused.

## Downloading and decrypting a chunk

`session.loadHistory`:

1. If `initialHistBootstrapInlinePayload` is set, it is the compressed chunk; no download happens.
2. Otherwise `Client.DownloadMedia` fetches `directPath` from a media host with media type `media.History` (path segment `md-msg-hist`, HKDF info "WhatsApp History Keys") and decrypts it like any other media file: check the SHA-256 of the encrypted blob against `fileEncSHA256`, derive cipher key, IV and MAC key from `mediaKey`, verify the 10-byte truncated HMAC, AES-CBC decrypt, then check the plaintext SHA-256 against `fileSHA256` (see [10-media.md](10-media.md)).
3. The plaintext is zlib-compressed. wax inflates it with `compress/zlib`, bounded to 256 MiB (`maxHistorySize`); a larger chunk is rejected.
4. The result is decoded as `wahistorysync.HistorySync`.

```
encrypted blob --(media decrypt, "WhatsApp History Keys")--> zlib stream --(inflate)--> HistorySync protobuf
inline payload ------------------------------------------>  zlib stream --(inflate)--> HistorySync protobuf
```

## Sync types and chunks

`HistorySync.syncType` values declared in `wahistorysync.proto`:

| Value | Name |
|---|---|
| 0 | `INITIAL_BOOTSTRAP` |
| 1 | `INITIAL_STATUS_V3` |
| 2 | `FULL` |
| 3 | `RECENT` |
| 4 | `PUSH_NAME` |
| 5 | `NON_BLOCKING_DATA` |
| 6 | `ON_DEMAND` |

wax treats all types the same way: whatever the chunk contains is stored. The type name, `chunkOrder` and `progress` are only reported in the `HistorySync` event. Chunks are processed in the order their notifications were queued, not by `chunkOrder`; since every write is an idempotent upsert, order does not affect correctness except for chat fields (see "Chats").

How much history the phone sends is decided by the phone. At registration wax sends `DeviceProps` with `requireFullSync=false` and no `historySyncConfig`. wax does not send on-demand history requests (the `HISTORY_SYNC_ON_DEMAND` peer data request types exist in the protobuf but are not used).

## The HistorySync payload

wax declares a subset of the upstream message. Some upstream fields are `required`; they are `optional` here, which is wire compatible. Enum fields that are only stored are declared as `int32`.

```
HistorySync
  syncType                  (1)
  conversations[]           (2)  Conversation
  statusV3Messages[]        (3)  WebMessageInfo      -- not stored
  chunkOrder                (5)
  progress                  (6)
  pushnames[]               (7)  {ID, pushname}
  phoneNumberToLidMappings[] (15) {pnJID, lidJID}
  nctSalt                   (19)
```

### Conversation

| Field | Used by wax |
|---|---|
| `ID` | chat JID; conversations without an ID are skipped |
| `messages[]` | `HistorySyncMsg{message: WebMessageInfo, msgOrderID}`; `msgOrderID` is unused |
| `unreadCount`, `archived`, `pinned` (>0 means pinned) | written to `wax_chats` |
| `conversationTimestamp` | `last_message_at` (Unix seconds) |
| `muteEndTime` | `muted_until`, read as Unix seconds; 0 leaves it unchanged |
| `name`, then `displayName` | chat name, first non-empty |
| `tcToken`, `tcTokenTimestamp`, `tcTokenSenderTimestamp` | privacy tokens (below) |
| `newJID`, `oldJID`, `lastMsgTimestamp`, `readOnly`, `markedAsUnread`, `pnJID`, `lidJID` | declared, not used |

### WebMessageInfo

| Field | Stored as |
|---|---|
| `key.ID` | message id; messages without one are skipped |
| `key.fromMe` | `from_me` |
| `key.participant`, then `participant` | `sender`; if both are empty and the message is not ours, the chat JID |
| `messageTimestamp` | `sent_at` (Unix seconds) |
| `pushName` | `push_name` of the row |
| `message` | `content`: the serialized `wae2e.Message`, with its wrappers |
| `messageStubType`, `messageStubParameters` | stub messages (below) |
| `status`, `starred` | declared, not stored |

The message `type` column is the media type when the message contains media (`image`, `sticker`, `document`, `audio`, `ptt`, `video`, `gif`), otherwise `text`. `source` is `history`.

### Stub messages

A `WebMessageInfo` without a `message` is a system message: a group change, a security notice, a missed call and so on. Its only content is `messageStubType` and the string list `messageStubParameters` (for example participant JIDs or a new subject). wax stores it with type `stub:<n>` and, if there are parameters, a JSON array of them as content:

```
type    = "stub:27"
content = ["<participant-jid>"]
```

When serving messages, `manager.DescribeStub` turns this into a `StubJSON`: the number, a name from a table of common stub types (for example 27 is `group_participant_add`, 21 is `group_change_subject`, 39 is `e2e_encrypted`), the parameters, and every parameter that parses as a user or LID JID listed as a person. Unknown stub numbers are reported by number only. The name table is maintained by hand and covers only common types.

### Push names, LID mappings, NCT salt

- `pushnames[]`: each `{ID, pushname}` with both values non-empty is upserted into `wax_contacts.push_name`. The row is only rewritten when the name changed.
- `phoneNumberToLidMappings[]`: each pair goes through `storeLIDMapping` (shared with app state): both must parse, the phone number side must not be a LID and the LID side must not be a phone number JID; incomplete or mismatched pairs are ignored. Valid pairs go into `wax_lid_map` (see [06-users-and-lids.md](06-users-and-lids.md)).
- `nctSalt`: stored sealed in `wax_devices.nct_salt`, the same column that the `nct_salt_sync` app state action writes. It is not used yet.

### Privacy tokens

For 1:1 conversations (user or LID JIDs), `session.storeHistoryToken`:

- if `tcToken` is present and `tcTokenTimestamp` > 0, stores the contact's token with that timestamp (`Store.PutPrivacyToken`);
- if `tcTokenSenderTimestamp` > 0, records when we last issued our own token to them (`Store.MarkPrivacyTokenSent`).

The token is stored under the contact's LID when one is known, otherwise under the conversation JID. The LID comes from the conversation's own `lidJID`, then from the chunk's phone number to LID mappings (kept in memory, because the chunk's writes are not committed yet), then from the database (`storeHistoryToken`).

## Storage: queue, worker, batching

### Queue

`wax_history_queue` holds one row per notification: id, instance, the sealed notification, `attempts`, `last_error`. `Store.EnqueueHistory` inserts the row and then writes the sealed notification, because the sealing associated data includes the generated row id.

### Worker

`session.historyWorker` is started once per session after login. It loops:

1. Take the oldest job with `attempts < 5` (`Store.NextHistoryJob`).
2. Run it while holding a slot of `Config.HeavySlots`, a process-wide semaphore that bounds how many clients of one node process history at the same time (`manager` sets it from its `HeavyWork` setting). Without the semaphore there is no limit.
3. On success, continue immediately with the next job.
4. On failure, increment `attempts` and store the error (`Store.FailHistoryJob`), then wait 15 seconds. The fifth failure deletes the job instead. With an empty queue it waits one minute or until `kickHistory` signals a new job.

A notification that cannot be decoded is deleted at once. A job that fails 5 times (for example because the blob expired on the media server) is deleted and the error is reported; the rest of the history is unaffected, and wax does not request the chunk again.

### One batch per chunk

`session.storeHistory` writes the whole chunk with `Store.Batch`: all statements are queued and sent in one round trip, which PostgreSQL runs as one implicit transaction. The batch includes deleting the job. Either the chunk is stored and its job removed, or nothing changes and the job is retried. A crash never stores a chunk twice or loses it.

Order inside the batch: LID mappings, push names, NCT salt, then each conversation (chat row, privacy token, messages).

### Idempotency

| Table | Write | Effect of a repeat |
|---|---|---|
| `wax_messages` | `INSERT ... ON CONFLICT (instance_id, chat, message_id) DO NOTHING` | existing rows win, including a row already stored live |
| `wax_contacts.push_name` | upsert only when different | no change |
| `wax_lid_map` | upsert only when different | no change |
| `wax_chats` | `Store.UpdateChat` upsert | see below |

### Chats

`Store.UpdateChat` takes optional fields; absent ones keep the stored value. `last_message_at` only moves forward (`GREATEST`). A history conversation always provides `unread_count`, `archived` and `pinned`. History can be older than app state, so its archive, pin and mute values (`ChatUpdate.FromHistory`) are applied only to chats whose settings app state has not set yet: an app state archive, pin or mute change records `settings_at` on the chat (migration 013), and from then on history leaves those three alone. The unread count is always taken from history. The name and mute end are only written when present.

## Progress events

After a chunk is committed, the worker logs it and emits `HistorySync`:

| Field | Source |
|---|---|
| `Type` | `syncType` name, for example `INITIAL_BOOTSTRAP` |
| `Chunk` | `chunkOrder` |
| `Progress` | `progress` as sent by the phone (wax does not interpret it; it is apparently a percentage) |
| `Conversations` | conversations stored (those with an ID) |
| `Messages` | messages stored or already present (rows skipped for lacking an ID are not counted) |
| `PushNames` | length of `pushnames[]`, including skipped entries |

The manager forwards it as the `history_sync` webhook event with keys `type`, `chunk`, `progress`, `conversations`, `messages`, `push_names`. There is no separate "history complete" event; `Store.HistoryQueueLength` reports how many chunks are still queued.

## Not implemented

- On-demand history requests to the phone, and configuring the amount of history at pairing.
- `statusV3Messages` (status updates) are decoded but not stored.
- Message status, starred flag and `msgOrderID` are not stored.
- Conversation JID changes (`oldJID`, `newJID`) are not applied.

## In the code

| Topic | Location |
|---|---|
| Notification dispatch (own account only, stored before ack) | `internal/client/message.go` (`dispatchMessage`) |
| Queue, worker, download, inflate, storing a chunk | `internal/client/history.go` (`enqueueHistory`, `kickHistory`, `historyWorker`, `withHeavySlot`, `processNextHistory`, `loadHistory`, `storeHistory`, `storeConversation`, `storeHistoryToken`, `historyMessage`) |
| Worker start after login | `internal/client/handlers.go` (post-login setup) |
| LID mapping validation (shared with app state) | `internal/client/appsync.go` (`storeLIDMapping`) |
| Media download and decryption | `internal/client/mediaops.go` (`DownloadMedia`), `internal/media/media.go` (`History`, `Decrypt`) |
| Queue, messages, chats, contacts, NCT salt | `internal/store/sync.go` (`EnqueueHistory`, `NextHistoryJob`, `FailHistoryJob`, `DeleteHistoryJob`, `PutMessage`, `UpdateChat`, `PutPushName`, `PutNCTSalt`, `HistoryQueueLength`) |
| Batched writes | `internal/store/store.go` (`Batch`) |
| Tables | `internal/store/migrations/005_sync.sql` |
| Stub message names and description | `internal/manager/stub.go` (`DescribeStub`) |
| Event and webhook | `internal/client/events.go` (`HistorySync`), `internal/manager/events.go` |
| Protobufs | `proto/wahistorysync/wahistorysync.proto`, `proto/wae2e/wae2e.proto` (`HistorySyncNotification`) |
