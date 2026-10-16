# 06. Users, LIDs and profiles

A WhatsApp user can be addressed in two ways: by phone number (`<digits>@s.whatsapp.net`) or by a LID (`<digits>@lid`), an opaque identifier that does not reveal the number. wax treats the LID as the primary identity for end-to-end encryption, learns phone number to LID mappings from every place the server exposes them, stores them in `wax_lid_map`, and uses them to merge the two chats one contact can end up with. This chapter also covers the user sync (`usync`) query that resolves devices and LIDs, the profile lookups built on it (about text, business profile, profile pictures), privacy ("trusted contact") tokens, the rate limit wax observes for profile lookups, and where push names and usernames come from.

## JID kinds

A JID has the textual form `user[.agent][:device]@server` ([internal/wabin/jid.go](../../internal/wabin/jid.go), `JID`). The servers that matter for people are:

| Server | Meaning | Example |
|---|---|---|
| `s.whatsapp.net` | Phone number user ("PN") | `905550000001@s.whatsapp.net` |
| `lid` | LID user | `1234@lid` |
| `hosted` | Hosted device of a PN user | `905550000001:99@hosted` |
| `hosted.lid` | Hosted device of a LID user | `1234:99@hosted.lid` |

A JID without a device part (device 0 is not printed) names the user and its primary device (the phone). A JID with `:<n>` is a device ("AD") JID, for example `1234:3@lid` is device 3 of LID user 1234. `JID.ToNonAD` strips the device and agent.

In the binary encoding, AD JIDs carry an agent byte; wax maps agent 1 to `lid`, 128 to `hosted` and 129 to `hosted.lid` (`newADJID`, `JID.agent`). Any other agent on `s.whatsapp.net` is kept in `RawAgent` and printed as `user.agent:device`.

### Signal address names

Because a phone number and a LID may consist of the same digits, the Signal session name is suffixed by domain ([internal/client/address.go](../../internal/client/address.go), `signalAddressName`):

| JID server | Signal name | Session address |
|---|---|---|
| `s.whatsapp.net` | `905550000001` (plus `_<agent>` if `RawAgent` is set) | `905550000001.0` |
| `lid` | `1234_1` | `1234_1.3` |
| `hosted` | `905550000001_128` | |
| `hosted.lid` | `1234_129` | |

### Hosted devices

A device listed with `is_hosted="true"` in a device list is rewritten to the `hosted` or `hosted.lid` server (`parseDeviceList`). wax never encrypts to hosted devices: `filterTargets` in [internal/client/send.go](../../internal/client/send.go) drops them from every fan-out. What hosted devices are beyond this flag is not modelled in the code.

## Why LIDs matter to wax

The code shows WhatsApp encrypting to LID identities:

- `sendDirect` resolves a phone number recipient to its LID before sending and addresses the `<message>` to the LID, keeping the number in `peer_recipient_pn`.
- Our own sending device is our LID device (`s.dev.LID`); a direct send fails with "own LID unknown" if the LID is not known yet.
- `encryptionJIDs` maps every PN device to the matching LID device when the mapping is known, so the same Signal session is used whichever address the server reports.
- After pairing, the primary device's identity key is stored under its LID device address when available (`handlePairSuccess`).
- Group sender keys use our LID device (`ownDeviceForGroups`), see [07-groups-and-communities.md](07-groups-and-communities.md).

Our own LID arrives in pair-success and in the `lid` attribute of `<success>`; `handleSuccess` stores it (filling in our device index if the server sent the LID without one) and records our own PN to LID mapping.

## Learning PN to LID mappings

All mappings go through `Client.learnLID` ([internal/client/usync.go](../../internal/client/usync.go)) or, inside batched writes, `storeLIDMapping` ([internal/client/appsync.go](../../internal/client/appsync.go)). Both ignore pairs whose servers are not `s.whatsapp.net` and `lid`, or whose user part is empty.

| Source | Where | What is read |
|---|---|---|
| usync `<lid>` | `resolveLID`, `Profile` | `<lid val="1234@lid"/>` in a `<user>` result |
| History sync | `storeHistory` | `phoneNumberToLidMappings` (`pnJID`, `lidJID`) |
| App state `contact` action | `applyMutation` | `pnJID` and `lidJID` of `ContactAction` |
| Group participants | `cacheGroup` | `jid` plus `phone_number` or `lid` attributes |
| 1:1 message attributes | `parseMessageInfo`, `sessionAddress` | `sender_lid` (sender is a PN) or `sender_pn` (sender is a LID) |
| Group message attributes | `parseMessageInfo`, `sessionAddress` | `participant_pn` or `participant_lid` |
| Our own account | `handleSuccess` | `dev.JID` and `dev.LID` |

`learnLID` keeps a small in-memory "already written" cache (`lidSeen`) so the mapping repeated on every message does not cost a database write. When the stored mapping actually changes, existing Signal sessions and identities stored under the phone number name are moved to the LID name (`Store.MigrateSessions`); rows that already exist under the LID name win.

### Storage: `wax_lid_map`

Created in migration 002 ([internal/store/migrations/002_signal.sql](../../internal/store/migrations/002_signal.sql)):

| Column | Notes |
|---|---|
| `instance_id` | Part of the primary key |
| `pn_user` | Phone number digits, primary key with `instance_id` |
| `lid_user` | LID digits, indexed for reverse lookups |
| `updated_at` | Set on every change |

`PutLIDMapping` upserts and reports whether the row changed; `LIDForPN` and `PNForLID` look up either direction (`PNForLID` takes the most recently updated row if several numbers point at one LID) ([internal/store/signal.go](../../internal/store/signal.go)).

### Merging a contact's two chats

Messages from one contact may be stored under `905550000001@s.whatsapp.net` and under `1234@lid`. The store resolves this at read time ([internal/store/api.go](../../internal/store/api.go)):

- `aliasCTE` expands a chat JID into itself plus the contact's other address, so message listing and `GetMessage` return the union of both chats.
- `identityJoin` resolves any user JID into phone digits, LID, address book name, push name and username, looking at contact rows stored under either address. `Identities` runs it for a batch of JIDs.
- `ListChats` computes the phone number of each chat with the same join, then `mergeAliases` folds rows with the same phone number into one: the most recent chat keeps its address, unread counts add up, `pinned` is ORed and an empty name is filled from the other row.

## The usync query

`session.usync` and `session.usyncList` build a user sync IQ:

```xml
<iq to="s.whatsapp.net" type="get" xmlns="usync" id="...">
  <usync sid="<id>" mode="query|full" last="true" index="0" context="message|background">
    <query>
      <devices version="2"/>
      <lid/>
      ...
    </query>
    <list>
      <user jid="905550000001@s.whatsapp.net"/>
      <user jid="1234@lid"><tctoken>...</tctoken></user>
    </list>
  </usync>
</iq>
```

`usync` always sends the user JID without device. `usyncList` accepts prepared `<user>` elements so a privacy token can be attached. The response is read as `<usync><list>` and the function returns the `<list>`; each answer is a `<user jid="...">` with one child per query element.

wax uses three combinations:

| Caller | mode | context | Query children |
|---|---|---|---|
| `Client.devices` (device lists for sending) | `query` | `message` | `devices version="2"` |
| `Client.resolveLID` (PN recipient without known LID) | `full` | `background` | `devices version="2"`, `lid` |
| `Client.Profile` | `full` | `background` | `business` (with `verified_name`), `status`, `devices version="2"`, `lid` |

wax does not send a `picture` child in usync; profile pictures use their own IQ (below). The meaning of `mode` and `context` beyond these combinations is not interpreted by the code.

### Parsing results

- **Devices.** `parseDeviceList` reads `<devices><device-list><device id="N" is_hosted="true"?/></device-list></devices>`. Ids outside 0 to 65535 are skipped. Other attributes of `<device>` (such as key indexes) are ignored.
- **LID.** `<lid val="1234@lid"/>`; only values on the `lid` server are accepted.
- **Status.** `<status t="<unix>">about text</status>`; a `code` attribute of `401` or `403` sets `AboutHidden`.
- **Business.** The presence of `<business><verified_name .../></business>` marks a business account.

If `resolveLID` gets no usable `<lid>` for the user it asked about, it returns `ErrNotOnWhatsApp`. A `Profile` answer may name the user by its other address, so `readUser` uses the first `<user>` regardless of its `jid`.

## Device list cache

`Client.devCache` maps a user (non-AD JID) to its devices with a 10 minute TTL (`deviceCacheTTL`). `devices` serves cached users and queries only the missing ones in a single usync. `resolveLID` also fills the cache from its `<devices>` answer.

The cache is invalidated by:

| Trigger | Handler |
|---|---|
| `<notification type="devices" from="<user>">` | `handleNotification`: drops `from` |
| `<notification type="account_sync">` with a `<devices>` child | `onAccountSync`: drops our own PN and LID entries |
| `<notification type="encrypt">` with `<identity>` | `onIdentityChange`: drops the user and deletes its sessions |
| Send answer whose `phash` differs from our hash | `sendDirect` / `sendGroup`: drop the affected users |

The phash is `participantListHash`: `"2:"` plus base64 (no padding) of the first 6 bytes of SHA-256 over the sorted `user.agent:device@server` strings. wax does not parse the content of a `devices` notification; it only invalidates.

## Profiles

`Client.Profile` ([internal/client/profile.go](../../internal/client/profile.go)) returns a `UserProfile`: about text and its time, whether it is hidden, LID, device count (including the phone), business flag and business profile. Results are cached for 10 minutes (`profileTTL`); a `<notification type="status">` from the user invalidates the entry.

The `<user>` element carries the user's privacy token when one is valid, because a "my contacts" privacy setting can otherwise hide the about text. A hidden about text is reported with `code="401"` (or `403`) on `<status>`.

If the usync returns a LID for a phone number user, the mapping is learned. For business accounts a second IQ follows:

```xml
<iq to="s.whatsapp.net" type="get" xmlns="w:biz">
  <business_profile v="244"><profile jid="905550000001@s.whatsapp.net"/></business_profile>
</iq>
```

`readBusiness` reads the `<profile>` answer: `<categories><category>name</category></categories>`, `<business_hours timezone="...">` with `<business_hours_config day_of_week mode open_time close_time/>`, and every other child with text content into `Fields[tag]` (a tag may repeat, for example several `website` elements). A failing business query is logged and leaves `Business` empty; it does not fail the profile.

The API's profile handler (`profile` in [api/handlers.go](../../api/handlers.go)) asks WhatsApp by phone number when the store knows it, and adds `CommonGroups`, which scans the cached joined group list (2 minute TTL) for participants matching any of the contact's addresses.

## Profile pictures

`Client.ProfilePicture` ([internal/client/picture.go](../../internal/client/picture.go)) works for users and groups:

```xml
<iq to="s.whatsapp.net" target="905550000001@s.whatsapp.net" type="get" xmlns="w:profile:picture">
  <picture type="preview" query="url"><tctoken>...</tctoken></picture>
</iq>
```

The `<tctoken>` child is present only when a valid token is stored. The answer is `<picture url="..." direct_path="..." id="..."/>`; wax only uses `url`.

| Outcome | Meaning in wax |
|---|---|
| IQ error 404 or 401 | No picture, or hidden from us: `ErrNoPicture` |
| `<picture>` without `url` | `ErrNoPicture` |
| URL not `https` on `whatsapp.net` or a subdomain | Error, not downloaded (`trustedPictureURL`) |
| HTTP 404 on download | `ErrNoPicture` |
| Body over 2 MiB or not sniffed as `image/*` | Error |

Only the preview size is requested. Both pictures and their absence are cached for 6 hours; a `<notification type="picture">` from the user or group invalidates the entry. At most 3 lookups run concurrently (`maxPictureLookups`). Download errors strip the signed URL before logging.

## Privacy ("trusted contact") tokens

A contact that writes to us gives us a token. Sending that token back with profile and picture queries and with messages lets the contact's "my contacts" settings treat us as a contact. wax implements this in [internal/client/privacy.go](../../internal/client/privacy.go) and [internal/store/privacy.go](../../internal/store/privacy.go).

### Receiving

From a notification (`onPrivacyToken`):

```xml
<notification from="905550000001@s.whatsapp.net" sender_lid="1234@lid" type="privacy_token" id="...">
  <tokens><token type="trusted_contact" t="1700000000">...bytes...</token></tokens>
</notification>
```

The user is `sender_lid` if present and on `lid`, otherwise `from`. Tokens of other types, empty tokens and tokens without `t` are skipped. The notification is always acknowledged.

From history sync (`storeHistoryToken`): for each 1:1 conversation, `tcToken` with `tcTokenTimestamp` is stored as a received token, and `tcTokenSenderTimestamp` is recorded as the time we last issued ours.

### Storage

Table `wax_privacy_tokens` (migration 012): `(instance_id, user_jid)` key, `token` (sealed with the instance's sealer, bound to table, column, instance and user), `token_at`, `sent_at`. Tokens are stored under the user's LID when the mapping is known, otherwise under the phone number (`tokenKeys`). `PutPrivacyToken` never replaces a newer token with an older one. `GetPrivacyToken` searches all given addresses and returns the row with the newest `token_at`.

### Validity

Time is divided into 7 day buckets counted from the Unix epoch. A token is valid while its timestamp is in the current bucket or one of the three before it (`tokenCutoff`), so it lives between 21 and 28 days. `pruneTokens` runs after each login and deletes rows whose `token_at` and `sent_at` are both older than the cutoff.

### Sending

A valid token is attached as `<tctoken>bytes</tctoken>`:

| Where | Element |
|---|---|
| Profile usync | child of `<user>` |
| Picture IQ | child of `<picture>` |
| 1:1 message (`sendDirect`) | child of `<message>`, after `<participants>` and `<device-identity>` |

Group messages do not carry a token.

### Issuing our own

After a successful 1:1 send, `issueToken` runs in the background:

```xml
<iq to="s.whatsapp.net" type="set" xmlns="privacy">
  <tokens><token jid="1234@lid" t="1700000000" type="trusted_contact"/></tokens>
</iq>
```

It is skipped if `sent_at` falls in the current bucket, for non-user JIDs, and for user `0` (WhatsApp's own announcements). On success `sent_at` is updated.

## Rate limiting

Profile lookups run one at a time (`profileSlot`). If the profile usync fails with IQ error code `429` (rate-overlimit), wax stores a hold of one minute (`profileHold`) and returns `ErrRateLimited`; every profile lookup during the hold fails immediately without asking the server. Cached profiles are still served. There is no equivalent hold for picture lookups or for the device and LID usyncs used when sending; those only see an ordinary IQ error. The REST API maps such upstream failures to HTTP 502.

## Push names and usernames

| Field | Source | Stored by |
|---|---|---|
| Push name of a contact | `notify` attribute of incoming messages | `handleMessage` via `PutPushName` (deduplicated in memory by `nameSeen`) |
| Push name of a contact | `pushnames` in history sync | `storeHistory` |
| Address book first/full name, username | App state `contact` action (keyed by the index JID) | `applyMutation` via `PutContactName` |
| Same, for LID contacts | App state `lid_contact` action | `applyMutation` |
| Our own push name | App state `setting_pushName` | `UpdatePushName` |

All contact names live in `wax_contacts`; migration 011 added the `username` column (without the leading `@`). Because names may be stored under either address, readers go through `identityJoin`, which takes the non-empty value from the rows of the phone number and the LID. wax does not query usernames from the server.

## In the code

| Topic | Location |
|---|---|
| JID type, servers, agents | `internal/wabin/jid.go`: `JID`, `ToNonAD`, `newADJID`, `ParseRecipient` |
| Signal names, phash | `internal/client/address.go`: `signalAddressName`, `signalAddress`, `participantListHash` |
| usync, devices, LID resolution | `internal/client/usync.go`: `usync`, `usyncList`, `parseDeviceList`, `devices`, `resolveLID`, `learnLID` |
| Mappings from messages | `internal/client/message.go`: `parseMessageInfo`, `sessionAddress` |
| Mappings and names from app state | `internal/client/appsync.go`: `applyMutation`, `storeLIDMapping` |
| Mappings, push names, tokens from history | `internal/client/history.go`: `storeHistory`, `storeHistoryToken` |
| Notifications and cache invalidation | `internal/client/handlers.go`: `handleNotification`, `onAccountSync`, `onIdentityChange`, `handleSuccess` |
| Profiles and business profile | `internal/client/profile.go`: `Profile`, `readUser`, `businessProfile`, `readBusiness`, `CommonGroups` |
| Profile pictures | `internal/client/picture.go`: `ProfilePicture`, `fetchPicture`, `trustedPictureURL` |
| Privacy tokens | `internal/client/privacy.go`: `tokenCutoff`, `tokenKeys`, `privacyToken`, `tokenContent`, `onPrivacyToken`, `issueToken`, `pruneTokens` |
| Token storage | `internal/store/privacy.go`, `internal/store/migrations/012_privacy_tokens.sql` |
| LID map storage, session migration | `internal/store/signal.go`: `PutLIDMapping`, `LIDForPN`, `PNForLID`, `MigrateSessions` |
| Chat merging, identities | `internal/store/api.go`: `aliasCTE`, `identityJoin`, `Identities`, `ListChats`, `mergeAliases` |
| Contact names, usernames | `internal/store/sync.go`: `PutContactName`, `PutPushName`; `internal/store/migrations/011_usernames.sql` |
| Sending with LIDs and tokens | `internal/client/send.go`: `sendDirect`, `encryptionJIDs`, `filterTargets` |
