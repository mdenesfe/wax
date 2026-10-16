# 08. App state sync

App state ("syncd") is the account data a WhatsApp account shares between its devices: address book names, chat settings such as mute, pin and archive, the user's own push name and a few other values. The server stores it as five named collections of encrypted key-value records. Each collection has a version number and an integrity hash (an LTHash) that the primary device signs with every change, so a companion can detect a missed or corrupted change. The keys that decrypt the records never reach the server: the phone sends them to its companions over the end-to-end encrypted Signal channel. This document describes how wax fetches collections, decrypts and verifies them, what it does with the decoded actions, and how it recovers from missing keys and integrity failures. wax only reads app state; sending changes is not implemented (see "Not implemented").

## Collections

| Name | Constant |
|---|---|
| `critical_block` | `appstate.CriticalBlock` |
| `critical_unblock_low` | `appstate.CriticalUnblockLow` |
| `regular_high` | `appstate.RegularHigh` |
| `regular` | `appstate.Regular` |
| `regular_low` | `appstate.RegularLow` |

`appstate.AllCollections` lists them in the order wax syncs them. wax does not assign meaning to which action lives in which collection; it applies every decoded mutation the same way regardless of collection.

Per collection and instance, wax persists:

- the version and the 128-byte LTHash (`wax_appstate_versions`, plus the `unverified` flag added by migration `007_appstate_unverified.sql`),
- the value MAC of every live record, keyed by its index MAC (`wax_appstate_macs`). These are needed to subtract a record's old contribution from the LTHash when it is overwritten or removed.

## Fetching: the `w:sync:app:state` IQ

`session.fetchAppStatePatches` asks for one collection per IQ. With a known version it asks for patches after that version:

```
<iq to="s.whatsapp.net" type="set" xmlns="w:sync:app:state" id="...">
  <sync>
    <collection name="regular" version="3" return_snapshot="false"/>
  </sync>
</iq>
```

For a full sync (`return_snapshot="true"`) the `version` attribute is omitted. The answer has the shape wax parses:

```
<iq type="result" id="...">
  <sync>
    <collection name="regular" version="..." has_more_patches="false">
      <snapshot>[ExternalBlobReference bytes]</snapshot>
      <patches>
        <patch>[SyncdPatch bytes]</patch>
        ...
      </patches>
    </collection>
  </sync>
</iq>
```

- A `<collection>` with an `<error>` child fails the sync with that error.
- `<snapshot>` never contains the snapshot itself. Its content is an `ExternalBlobReference` (media key, direct path, file hashes). wax downloads it from the media servers and decrypts it with media type `media.AppState` (HKDF info "WhatsApp App State Keys", path segment `md-app-state`), then decodes a `SyncdSnapshot` (the media scheme is described in [10-media.md](10-media.md)).
- Each `<patch>` is an inline `SyncdPatch`. If it carries `externalMutations` instead of inline mutations, `session.readPatch` downloads that blob the same way and decodes it as `SyncdMutations`.
- `has_more_patches="true"` makes `syncAppStateLocked` loop: it asks again from the version it has reached, now without a snapshot.

The `version` attribute of the response `<collection>` is not read; wax takes versions from the snapshot and patch protobufs.

## Snapshot vs patches

```
             version 0 (never synced)            version N > 0
                      |                                |
        return_snapshot=true                 version=N, return_snapshot=false
                      |                                |
   SyncdSnapshot (all live records,          SyncdPatch N+1, N+2, ...
   version V, mac, keyID)                    (mutations SET/REMOVE, snapshotMAC,
                      |                       patchMAC, keyID)
                      +---------------+----------------+
                                      v
                     verify, decrypt, apply in one DB transaction
```

A snapshot is the complete set of live records at version V. Applying it first clears the collection (`Store.ResetAppState` inside the same transaction). A patch moves the collection from version N to the version in the patch and contains `SyncdMutation`s, each an operation (`SET` = 0, `REMOVE` = 1) and a `SyncdRecord`.

Every snapshot or patch is committed in its own transaction (`Client.applyAppState`): the new MAC rows, the derived tables (contacts, chats, push name) and the new version and hash. A crash between patches therefore leaves a consistent state at the last committed version. `AppStateChange` events are emitted only after the commit.

## App state sync keys

### Arrival

The phone shares keys in a protocol message of type `APP_STATE_SYNC_KEY_SHARE`:

```
Message.protocolMessage.appStateSyncKeyShare.keys[] = {
  keyID:   { keyID: <bytes> },
  keyData: { keyData: <32 bytes>, fingerprint: {rawID, currentIndex, deviceIndexes}, timestamp }
}
```

`session.dispatchMessage` only honours it when the message is from our own account (`info.IsFromMe`). `session.handleKeyShare` stores every key with a non-empty id and data (`Store.PutAppStateKey`: key data sealed at rest, fingerprint stored as serialized protobuf, timestamp kept) before the carrying message is acknowledged, then starts `syncAllAppState` in the background. Keys are looked up by their opaque key id; wax does not interpret the fingerprint.

### Expansion

`appstate.ExpandKeys` runs HKDF-SHA256 over the 32-byte key data, no salt, info `"WhatsApp Mutation Keys"`, output 160 bytes, split into five 32-byte keys:

| Bytes | Key | Used for |
|---|---|---|
| 0..31 | index | `IndexMAC` |
| 32..63 | value encryption | AES-256-CBC of the record value |
| 64..95 | value MAC | `ContentMAC` |
| 96..127 | snapshot MAC | `SnapshotMAC` |
| 128..159 | patch MAC | `PatchMAC` |

Records, snapshots and patches each name their key id, so one collection can mix keys. A per-call `keyCache` avoids re-expanding the same key.

## Decoding a mutation

A `SyncdRecord` has `index.blob` (the index MAC), `value.blob` and `keyID`. The value blob is laid out as:

```
+---------+------------------------+----------------+
| IV (16) | AES-CBC ciphertext     | value MAC (32) |
+---------+------------------------+----------------+
```

Blobs shorter than 64 bytes are rejected as malformed. `appstate.decodeMutation` then:

1. Verifies the value MAC: `HMAC-SHA512(valueMACKey, [op+1] || keyID || IV || ciphertext || uint64be(len(keyID)+1))`, truncated to 32 bytes (`appstate.ContentMAC`). `op` is 0 for SET and 1 for REMOVE, so the operation is authenticated too. Failure: `ErrContentMAC`.
2. Decrypts `IV || ciphertext` with the value encryption key (AES-CBC, PKCS#7) into a `SyncActionData`: `index` (JSON array of strings), `value` (`SyncActionValue`), `padding`, `version`.
3. Verifies the index MAC: `HMAC-SHA256(indexKey, indexJSON)` must equal `index.blob` (`appstate.IndexMAC`). Failure: `ErrIndexMAC`. The index MAC both authenticates and blinds the index: the server only sees the MAC, never the JSON.
4. Parses the index, for example `["mute","<jid>"]`. An empty or invalid index is malformed.

`appstate.EncodeMutation` is the exact inverse and is used by the tests to build patches.

## The LTHash

The LTHash is a 128-byte homomorphic set hash over the value MACs of all live records in a collection (`appstate.lthashOp`):

```
expand(m) = HKDF-SHA256(ikm = m, salt = none, info = "WhatsApp Patch Integrity", len = 128)
add(m):      for each 16-bit little-endian word i: state[i] = state[i] + expand(m)[i]  (mod 2^16)
subtract(m): for each 16-bit little-endian word i: state[i] = state[i] - expand(m)[i]  (mod 2^16)
```

Because addition is commutative, the hash depends only on the set of live value MACs, not on the order they were applied. `HashState.SubtractThenAdd(removed, added)` applies a batch.

### Updating the hash for a patch

`appstate.advanceHash` walks the mutations in order. For each mutation it finds the previous value MAC of the same index MAC, first from earlier mutations of the same patch, otherwise from `wax_appstate_macs`. If there was one, it is subtracted. A SET adds its own value MAC; a REMOVE adds nothing. The stored MAC rows are then updated (SET writes, REMOVE deletes).

### Verifying

```
snapshotMAC = HMAC-SHA256(snapshotMACKey, lthash || uint64be(version) || collectionName)
patchMAC    = HMAC-SHA256(patchMACKey, snapshotMAC || valueMAC_1 || ... || valueMAC_n
                                        || uint64be(version) || collectionName)
```

- Snapshot (`appstate.ApplySnapshot`): starting from an empty hash, add every record's value MAC, then compare `SnapshotMAC` with the snapshot's `mac`. All records are also decoded and verified individually.
- Patch (`appstate.ApplyPatch`): compute the next hash with `advanceHash`, check the patch MAC over the patch's own `snapshotMAC` field and every value MAC (`ErrPatchMAC`), then compare the locally computed snapshot MAC with the patch's `snapshotMAC` (`ErrSnapshotMAC`). Then decode each mutation.

The patch MAC does not depend on wax's local hash, which is why it can still be checked when the local hash is known to be unreliable (next section).

## The "unverified" state

On some real accounts a server snapshot's LTHash does not match the sum of the records it delivers, although every record verifies individually. Even the LTHash the phone reports during recovery can differ, so the hash apparently covers records that are never delivered to companions. wax handles this as follows (`appstate.HashState.Unverified`):

- `ApplySnapshot` does not fail on a snapshot MAC mismatch. It marks the state unverified and still applies the records.
- `Client.onUnverified` logs it, emits `AppStateUnverified{Collection, Version}` (webhook `app_state_unverified`) and asks the phone for a recovery copy.
- On an unverified state, patches are still checked by value MAC, index MAC and patch MAC. Only the snapshot MAC comparison is skipped. If a later patch's snapshot MAC happens to match, the state becomes verified again.
- The flag is persisted in `wax_appstate_versions.unverified` (migration 007).

## Index actions and what wax does with them

`Client.applyMutation` updates derived tables inside the same transaction. `index[0]` is the action name; `index[1]`, when present, is the chat or contact JID.

| `index[0]` | `SyncActionValue` field | wax effect |
|---|---|---|
| `contact` | `contactAction` | `wax_contacts` first name, full name, username for `index[1]`; if `pnJID` and `lidJID` form a valid phone number / LID pair, stores the mapping in `wax_lid_map` (see [06-users-and-lids.md](06-users-and-lids.md)) |
| `lid_contact` | `lidContactAction` | same names for `index[1]` (no mapping) |
| `mute` | `muteAction` | `wax_chats.muted_until`: cleared when not muted, year 9999 for a negative end timestamp ("always"), otherwise the end timestamp in milliseconds |
| `pin_v1` | `pinAction` | `wax_chats.pinned` |
| `archive` | `archiveChatAction` | `wax_chats.archived` (the message range is ignored) |
| `markChatAsRead` | `markChatAsReadAction` | `read=true` sets the unread count to 0; `read=false` does nothing |
| `setting_pushName` | `pushNameSetting` | our own push name (`Store.UpdatePushName` and the in-memory value used when sending); an empty name is ignored |
| `nct_salt_sync` | `nctSaltSyncAction` | stores the NCT salt sealed in `wax_devices.nct_salt`; a REMOVE clears it. The salt is stored but not used yet |

Other notes:

- REMOVE mutations update the MAC table and the LTHash but have no effect on derived tables, except for `nct_salt_sync`.
- The protobuf also declares `starAction`, `deleteMessageForMeAction`, `clearChatAction` and `deleteChatAction`, and unknown action names are decoded too. wax does not apply them to its tables. Every decoded mutation is still emitted as an `AppStateChange{Collection, Index, Action, Removed, FromFullSync}` event for the embedding application.
- `SyncActionValue.timestamp` and `SyncActionData.version` are decoded but not used for conflict resolution: the order of mutations is the order of application.

## Sync flow and resets

```
syncAppState(collection, full=false)
  |
  +-- syncAppStateLocked
  |     load state; version 0 => full
  |     loop: fetch -> apply snapshot? -> apply patches -> has_more?
  |
  +-- error is ErrSnapshotMAC / ErrPatchMAC / ErrContentMAC / ErrIndexMAC ?
        |
        +-- ResetAppState(collection)        (delete version and MAC rows)
        +-- syncAppStateLocked(full=true)    (fresh snapshot + patches)
        +-- still ErrPatchMAC ?  => requestRecovery(collection)
```

- Syncs are serialized per client by `appStateSync.mu`.
- An integrity failure on an incremental sync triggers exactly one full resync. If the server's own data fails the patch MAC again, only the phone can help, so a recovery is requested.
- Any failure other than missing keys emits `AppStateSyncError` (webhook `app_state_error`). It is retried on the next `server_sync` notification or reconnect.
- A full sync emits `AppStateSyncComplete{Collection, Version}`. Empty collections complete at version 0.

### What triggers a sync

| Trigger | Code | Scope |
|---|---|---|
| Login completed (after `passive`/`active`) | end of the post-login setup in `handlers.go` | all collections (`Client.syncAllAppState`) |
| Key share received | `session.handleKeyShare` | all collections |
| `server_sync` notification | `session.onServerSync` | named collections |
| Recovery applied | `Client.applyRecovery` | that collection, patches after the recovered version |

The `server_sync` notification lists changed collections:

```
<notification from="s.whatsapp.net" id="..." type="server_sync" t="...">
  <collection name="regular" version="..."/>
</notification>
```

wax reads only the `name` attributes, acknowledges the notification immediately and syncs each named collection in the background from its stored version.

## Missing keys

If a snapshot, patch or record names a key id wax does not have, `storeLookup.Key` records it and decoding fails with `ErrKeyNotFound`; the transaction rolls back. `Client.maybeRequestKeys` then sends our primary device a peer protocol message:

```
Message.protocolMessage = {
  type: APP_STATE_SYNC_KEY_REQUEST,
  appStateSyncKeyRequest: { keyIDs: [ {keyID: <bytes>}, ... ] }
}
```

`Client.sendPeerMessage` encrypts it for the primary device (fetching a prekey bundle if there is no session) and sends a `<message category="peer" type="text" push_priority="high">`. Each key id is requested at most once per minute (`keyRequestInterval`, in memory). The sync returns `errMissingKeys`, which is logged at info level and not reported as an error event. When the phone answers with a key share, `handleKeyShare` stores the keys and re-runs the sync.

## Recovery from the phone

When a collection is unverified, or the server's data fails verification even after a full resync, `Client.requestRecovery` sends:

```
Message.protocolMessage = {
  type: PEER_DATA_OPERATION_REQUEST_MESSAGE,
  peerDataOperationRequestMessage: {
    peerDataOperationRequestType: COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY,
    syncdCollectionFatalRecoveryRequest: { collectionName, timestamp (Unix seconds) }
  }
}
```

It is rate limited to once per hour per collection (in memory). The phone's answer arrives as a `peerDataOperationRequestResponseMessage` from our own account. `session.handlePeerDataResponse` runs in its own goroutine (it must wait for the app state lock, which a sync may hold across network round trips) and ignores other request types. For each result with a `syncdSnapshotFatalRecoveryResponse`:

1. If `isCompressed`, gunzip `collectionSnapshot` (bounded to 256 MiB).
2. Decode a `SyncdSnapshotRecovery`: version, collection name, plaintext records (`SyncActionData`, key id, value MAC) and the 128-byte collection LTHash.
3. `Client.applyRecovery` ignores unknown collections and stale copies: a local version newer than the phone's, or the same version that is already verified.
4. `appstate.ApplyRecovery` adopts the phone's version and LTHash as they are. The records came over the end-to-end channel from our own phone, so there is nothing to check against the server. wax recomputes each index MAC with the record's key (so the key must be present) and stores the phone's value MAC.
5. The collection is reset and rewritten in one transaction, `AppStateSyncComplete` is emitted, and a normal incremental sync follows.

If a key is missing during recovery, the response is dropped silently; the recovery can be requested again later.

## Not implemented

- Sending app state changes (mute, pin, archive from wax). `appstate.EncodeMutation` and `appstate.SealPatch` build valid mutations and patches but are only used by tests; there is no code that uploads a patch.
- Conflict resolution by action timestamp.
- Applying star, delete-for-me, clear-chat and delete-chat actions to stored messages.
- The `fingerprint` and `timestamp` of keys are stored but not used to choose or expire keys.

## In the code

| Topic | Location |
|---|---|
| Collection names, LTHash, key expansion, MAC formulas | `internal/appstate/appstate.go` (`lthashOp`, `HashState`, `ExpandKeys`, `SnapshotMAC`, `PatchMAC`, `ContentMAC`, `IndexMAC`) |
| Mutation decoding, snapshot and patch verification, recovery records | `internal/appstate/decode.go` (`decodeMutation`, `ApplySnapshot`, `advanceHash`, `ApplyPatch`, `ApplyRecovery`) |
| Building mutations and patches (tests) | `internal/appstate/encode.go` (`EncodeMutation`, `SealPatch`) |
| Sync loop, IQ, blob download, derived tables, key requests, key share | `internal/client/appsync.go` (`syncAllAppState`, `syncAppState`, `syncAppStateLocked`, `fetchAppStatePatches`, `readPatch`, `applyAppState`, `applyMutation`, `maybeRequestKeys`, `handleKeyShare`, `sendPeerMessage`) |
| Fatal recovery request and response | `internal/client/recovery.go` (`requestRecovery`, `handlePeerDataResponse`, `applyRecovery`) |
| `server_sync` notification, sync after login | `internal/client/handlers.go` (`onServerSync`, post-login setup) |
| Dispatch of key shares and peer data responses | `internal/client/message.go` (`dispatchMessage`) |
| Events | `internal/client/events.go` (`AppStateChange`, `AppStateSyncComplete`, `AppStateUnverified`, `AppStateSyncError`) |
| Keys, versions, MACs, contacts, chats, NCT salt | `internal/store/sync.go` |
| Tables | `internal/store/migrations/005_sync.sql`, `internal/store/migrations/007_appstate_unverified.sql` |
| Protobufs | `internal/proto/waserversync/waserversync.proto`, `internal/proto/wasyncaction/wasyncaction.proto`, `internal/proto/wae2e/wae2e.proto` (key share and request) |
| End-to-end test (key request, key share, snapshot, patch) | `internal/client/phase5_test.go` |
