# 07. Groups and communities

Groups live on the `g.us` server (`120363000000000001@g.us`). wax reads group metadata with `w:g2` IQs, either for one group (`<query request="interactive"/>`) or for every group the account is in (`<participating/>`), parses the `<group>` element into `GroupInfo`, caches it for sending, learns participants' phone number to LID mappings from it, and invalidates it on `w:gp2` notifications. The group's `addressing_mode` decides whether wax addresses itself by LID or by phone number when it fans a message out. Communities are groups too: a parent group, linked groups that point at it, and an announcement group. wax only reads groups; creating, editing, joining and leaving groups are not implemented.

## Fetching one group

`Client.GetGroupInfo` (always asks the server) and `Client.groupInfo` (cache first, used by `sendGroup`) both end in `fetchGroupInfo` ([internal/client/group.go](../../internal/client/group.go)):

```xml
<iq to="120363000000000001@g.us" type="get" xmlns="w:g2" id="...">
  <query request="interactive"/>
</iq>
```

The answer is an `<iq type="result">` with one `<group>` child. Error handling:

| IQ error code | Result |
|---|---|
| 404, 403, 401 | `ErrGroupNotFound` (unknown group, or we are not a member) |
| other | wrapped as `group info: ...` |
| result without `<group>` | error |

A JID that is not on `g.us` is rejected locally with `ErrUnsupportedChat` before any request.

## Listing joined groups

`Client.JoinedGroups` lists every group and community the account is in with a single request:

```xml
<iq to="g.us" type="get" xmlns="w:g2" id="...">
  <participating>
    <participants/>
    <description/>
  </participating>
</iq>
```

The answer is `<groups>` with one `<group>` per group, in the same format as the single group query. Each one is parsed, cached in the group cache, and its participant mappings are learned. The whole list is also kept for 2 minutes (`joinedTTL`) for `CommonGroups` (see [06-users-and-lids.md](06-users-and-lids.md)). Children of `<groups>` that are not `<group>` are ignored.

## The `<group>` element

A trimmed example of what `parseGroup` understands:

```xml
<group id="120363000000000001" subject="Team" s_t="1700000000" s_o="1234@lid"
       creation="1690000000" creator="1234@lid" creator_pn="905550000001@s.whatsapp.net"
       addressing_mode="lid" size="3">
  <participant jid="1234@lid" type="superadmin" phone_number="905550000001@s.whatsapp.net"/>
  <participant jid="5678@lid" type="admin" phone_number="905550000002@s.whatsapp.net"/>
  <participant jid="9012@lid"/>
  <description id="..." participant="1234@lid" t="1700000100">
    <body>Group description</body>
  </description>
  <ephemeral expiration="604800"/>
  <member_add_mode>admin_add</member_add_mode>
  <membership_approval_mode/>
  <announcement/>
  <locked/>
</group>
```

### Attributes

| Attribute | `GroupInfo` field | Notes |
|---|---|---|
| `id` | `JID` | User part; the server is always `g.us` |
| `subject` | `Name` | |
| `s_t` | `NameSetAt` | Unix seconds |
| `s_o` | `NameSetBy` | JID of who set the subject |
| `creation` | `Created` | Unix seconds |
| `creator` | `Owner` | |
| `creator_pn` | `OwnerPN` | Creator's phone number in LID groups |
| `addressing_mode` | `AddressingMode` | `lid` or `pn`; empty if absent |
| `size` | `Size` | Participant count as reported by the server |

### Children

| Child | Effect |
|---|---|
| `<participant>` | Appended to `Participants` (below) |
| `<description participant t>` with `<body>` | `Topic` from `<body>` text, `TopicSetBy`, `TopicSetAt`. A `<description>` without `<body>` leaves `Topic` empty (covered by `TestParseGroupDescriptionWithoutBody`) |
| `<ephemeral expiration="N"/>` | `Ephemeral` (disappearing timer in seconds) when N is positive |
| `<member_add_mode>text</member_add_mode>` | `MemberAddMode`, the text as sent (for example `admin_add`, `all_member_add`) |
| `<membership_approval_mode>` | `JoinApproval = true` (presence only; its content is not parsed) |
| `<suspended/>` | `Suspended = true` |
| `<announcement/>` | `Announce = true`: only admins may send |
| `<locked/>` | `Locked = true`: only admins may edit group info |
| `<parent/>` | `IsCommunity = true`: this group is a community |
| `<linked_parent jid="..."/>` | `Community`: the community this group belongs to |
| `<default_sub_group/>` | `DefaultSubGroup = true`: the community's announcement group |

Any other child is ignored. The attributes of `<parent>` and the content of `<membership_approval_mode>` are not read.

### Participants

`parseParticipant` reads `jid` and `type`:

| `type` | `IsAdmin` | `IsSuperAdmin` |
|---|---|---|
| absent | false | false |
| `admin` | true | false |
| `superadmin` | true | true |

`JID` is the participant as addressed in the group. The other address comes from an attribute:

| `jid` server | `LID` | `PhoneNumber` |
|---|---|---|
| `lid` | `jid` | `phone_number` attribute |
| `s.whatsapp.net` | `lid` attribute | `jid` |

`cacheGroup` passes every participant with both addresses to `learnLID`, so fetching groups is one of the main sources of PN to LID mappings ([06-users-and-lids.md](06-users-and-lids.md)).

## Group cache and `w:gp2` notifications

Parsed groups are kept in `Client.groups`, keyed by group JID, for one hour (`groupCacheTTL`). The TTL only bounds staleness when a notification was missed; normally a `w:gp2` notification invalidates the entry:

```xml
<notification from="120363000000000001@g.us" type="w:gp2" id="..." participant="1234@lid">
  <remove>...</remove>
</notification>
```

`handleGroupNotification` ([internal/client/group.go](../../internal/client/group.go)):

1. Drops the cached group.
2. Collects the tags of all children (for example `add`, `remove`, `subject`) into `Changes`. Their content is not parsed.
3. If a child is `remove` or `leave`, rotates our sender key for the group: deletes our sender key record and forgets which devices received it, so the next send creates a new key and distributes it to the remaining members. If this fails, the notification is not acknowledged and the server redelivers it.
4. Emits `GroupUpdate{JID, Changes}` and acknowledges.

The 2 minute joined group list is not invalidated by `w:gp2`; it expires on its own.

A send whose server answer carries a `phash` different from ours also drops the group from the cache, together with the cached device lists of all its members (`sendGroup`).

## Addressing mode and sending

`sendGroup` ([internal/client/send.go](../../internal/client/send.go)) uses the cached metadata:

- **Own identity.** Our sending device is always our LID device when known (`ownDeviceForGroups`); sender keys are stored under its Signal address. The own user whose devices are included in the fan-out is the LID user in `lid` groups and our phone number user otherwise.
- **Recipients.** Every participant's `JID` (as addressed in the group) is taken without device, except entries whose user part equals our LID, our own user or our phone number. Device lists for all of them come from usync ([06-users-and-lids.md](06-users-and-lids.md)).
- **Encryption.** The content is encrypted once with our sender key (`<enc type="skmsg" v="2">`). The sender key distribution message is encrypted 1:1 only for devices that do not have it yet (`senderKeyTargets`); `encryptionJIDs` uses the LID session for a PN device when its mapping is known. Hosted devices and our own sending device are skipped (`filterTargets`).
- **Stanza.** The `<message>` carries `to` (the group), `phash` over all devices of all members, and `addressing_mode="lid"` only when the group uses LIDs.

```xml
<message id="..." type="text" to="120363000000000001@g.us" phash="2:..." addressing_mode="lid">
  <participants>
    <to jid="5678:2@lid"><enc type="pkmsg" v="2">...</enc></to>
  </participants>
  <enc type="skmsg" v="2">...</enc>
  <device-identity>...</device-identity>
</message>
```

`<participants>` is omitted when every device already has our key; `<device-identity>` is included when any 1:1 payload is a `pkmsg`. Group messages carry no privacy token.

On the receiving side, a group message names its sender in `participant`, with the other address in `participant_pn` or `participant_lid`; wax learns the mapping and decrypts with the LID session when known (`parseMessageInfo`, `sessionAddress` in [internal/client/message.go](../../internal/client/message.go)).

## Communities

A community appears in the joined group list as ordinary `<group>` elements linked to each other:

| Role | How wax recognizes it |
|---|---|
| Community (parent) | `<parent/>` child: `IsCommunity` |
| Announcement group | `<linked_parent jid="<community>"/>` and `<default_sub_group/>`: `Community` set, `DefaultSubGroup` true (typically also `<announcement/>`) |
| Linked group | `<linked_parent jid="<community>"/>` only: `Community` set |

`TestJoinedGroups` ([internal/client/groups_test.go](../../internal/client/groups_test.go)) covers this shape. wax does not query a community's sub groups separately and has no community specific IQs; it only reports the links found in the listing.

## REST API

| Route | Handler | Output |
|---|---|---|
| `GET /v1/instances/{id}/groups` | `listGroups` | `JoinedGroups` reduced to JID, name, topic, community flags, announce and participant count |
| `GET /v1/instances/{id}/groups/{group}` | `groupInfo` | `GetGroupInfo` (fresh from the server) passed through `describeGroup` |

`describeGroup` ([api/handlers.go](../../api/handlers.go)) resolves all participants with one `Store.Identities` call and adds phone number, address book name, push name and username, using the participant's `phone_number` attribute when the store has no phone for a LID. Upstream failures, including `ErrGroupNotFound`, are answered as HTTP 502 by `upstream`.

## Not implemented

The code contains no IQs for any of the following; they are out of scope at present:

- Creating a group or community, or linking and unlinking groups to a community.
- Changing subject, description, settings (announce, locked, ephemeral, member add mode, approval mode) or the group picture.
- Adding, removing, promoting or demoting participants; leaving a group.
- Invite links and codes (querying, revoking, joining), and handling join requests.
- Parsing the content of `w:gp2` notifications (who was added or removed, new subject, and so on). Only the child tags are reported.
- Querying community sub groups or participants of a community as a whole.

Broadcast lists (`broadcast` server) are treated as groups when receiving but cannot be sent to.

## In the code

| Topic | Location |
|---|---|
| Types and errors | `internal/client/group.go`: `GroupInfo`, `GroupParticipant`, `ErrGroupNotFound` |
| Single group query | `internal/client/group.go`: `GetGroupInfo`, `groupInfo`, `fetchGroupInfo` |
| Joined groups | `internal/client/group.go`: `JoinedGroups` |
| Parsing | `internal/client/group.go`: `parseGroup`, `parseParticipant` |
| Cache and mappings | `internal/client/group.go`: `cacheGroup`, `groupCacheTTL`; `internal/client/client.go`: `groups`, `joined` |
| Notifications | `internal/client/handlers.go`: `handleNotification`; `internal/client/group.go`: `handleGroupNotification`, `rotateSenderKey` |
| Own group identity | `internal/client/group.go`: `ownDeviceForGroups` |
| Sending | `internal/client/send.go`: `sendGroup`, `senderKeyEncrypt`, `senderKeyTargets`, `filterTargets`, `encryptionJIDs` |
| Receiving sender addresses | `internal/client/message.go`: `parseMessageInfo`, `sessionAddress` |
| Common groups | `internal/client/profile.go`: `CommonGroups` |
| Events | `internal/client/events.go`: `GroupUpdate` |
| Tests | `internal/client/groups_test.go`: `TestJoinedGroups`; `internal/client/group_test.go`: `TestParseGroupDescriptionWithoutBody` |
| REST | `api/handlers.go`: `listGroups`, `groupInfo`, `describeGroup` |
