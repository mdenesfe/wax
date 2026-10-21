# REST API guide

This is a practical tour of the wax REST API. The complete contract (every
route, request and response schema, and webhook event) is the OpenAPI 3.1
document served by every node:

```bash
curl http://localhost:8080/openapi.json
```

It needs no key. A test compares it with the code (routes, key types, JSON
field names, webhook events), so it does not drift from the implementation.
Use it to generate clients or to check a field this guide does not mention.

## Basics

**Base URL.** The API is served at the address given with `-listen`, for
example `http://localhost:8080`. All API routes are under `/v1/`. wax
speaks plain HTTP; put TLS in front of it (a reverse proxy or load
balancer) for anything beyond localhost.

**Unauthenticated routes.**

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness: `{"status":"ok"}` |
| `GET /openapi.json` | The API contract |
| `GET /` and `GET /ui/` | The admin interface (see [admin-ui.md](admin-ui.md)); not served with `-no-ui` |

**JSON.** Request bodies are JSON and unknown fields are rejected with
`400`, so a typo in a field name fails loudly instead of being ignored.
Request bodies are limited to 90 MiB (base64 media included).

**Identifiers.**

- Tenant ids and instance names: 1 to 64 characters of `a-z`, `0-9`, `-`
  and `_`.
- Recipients (`to`, `mentions`, and `{chat}` where noted): a phone number
  in international format (digits, an optional leading `+`) or a JID.
- JIDs look like `905550000001@s.whatsapp.net` (a user by phone number),
  `123456789012345@lid` (a user by LID), `120363000000000001@g.us` (a group)
  or `status@broadcast` (status updates).

## Authentication

Every `/v1/` request carries a bearer token:

```
Authorization: Bearer <key>
```

There are two kinds of keys, and each can do only its own job:

| Key | Where it comes from | What it can do |
|---|---|---|
| **Admin key** | `WAX_API_KEY` in the server environment | Manage tenants and their keys (`/v1/tenants...`). It cannot read or control any instance. |
| **Tenant key** | Issued with `POST /v1/tenants/{tenant}/keys` | Manage that tenant's instances (`/v1/instances...`). Nothing else. |

Tenant keys have the form `wax_<id>_<secret>`, for example
`wax_0123456789abcdef_<secret>`. The `<id>` part identifies the key (it is
what `GET /v1/tenants/{tenant}/keys` lists and what you revoke). The key is
returned exactly once, when it is issued; the database stores only an HMAC
of the secret. Keys are checked against the database on every request, so a
revoked key stops working immediately.

**Tenant isolation.** A tenant key sees only its own tenant's instances. An
instance of another tenant answers `404`, exactly like one that does not
exist. Instance names are unique per tenant, so two tenants can both have
an instance called `main`.

Using the wrong kind of key returns `403`: the admin key on an instance
route, or a tenant key on a tenant route.

## Errors and status codes

Every error body has the same shape:

```json
{"error": "chat and ids are required"}
```

| Status | Meaning |
|---|---|
| `200` / `201` | OK / created. |
| `202` | Accepted: a message was queued, or a reconnect was requested. |
| `204` | Done, no body (deletes, read receipts, presence, call reject). |
| `400` | Invalid input: bad JSON, unknown field, invalid id or recipient, missing field. The message says what is wrong. Also returned when the body exceeds the size limit. |
| `401` | Missing or invalid API key. |
| `403` | Wrong kind of key for this route, or the tenant's instance quota is reached. |
| `404` | Not found, including other tenants' instances. Also: no QR code pending, message has no media, no profile picture. |
| `409` | Already exists (tenant or instance id), or a tenant that still has instances is being deleted. |
| `429` | The tenant's API rate limit was exceeded. See below. |
| `502` | WhatsApp or the media servers failed the request. Details are logged on the server, not returned. |
| `503` | The instance is not connected (or not running on any node yet), or the node that runs it is unreachable. Retry shortly. |
| `500` | Unexpected error; details are in the server log. |

## Rate limits

Each tenant has an API rate in requests per second (`requests_per_second`
on the tenant; `0` means the node default, which is `-tenant-rps`, 20 by
default). The burst is two seconds' worth. Above it the API answers:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1

{"error":"rate limit exceeded"}
```

Wait the number of seconds in `Retry-After` before retrying. One tenant
hitting its limit does not affect other tenants. Limits are enforced per
node: behind a load balancer with N nodes a tenant can get up to N times its
rate.

This is separate from the **send rate** of an instance
(`rate_per_minute`), which paces how fast queued messages go out to
WhatsApp. That one never rejects requests; messages simply wait in the
outbox.

## Idempotency

`POST /v1/instances/{id}/messages` accepts an optional `client_id`. If a
message with the same `client_id` was already queued for that instance, the
request does not queue a second one: it returns the first request's
`outbox_id`, `message_id` and current `status` with `"duplicate": true`.
Use a unique `client_id` per logical message (for example your own database
id) and you can retry a send safely after a timeout or network error.
`client_id` is also included in the `send_status` webhook.

## Tenants and keys (admin key)

| Method and path | Purpose |
|---|---|
| `POST /v1/tenants` | Create `{id, name, max_instances, requests_per_second}` |
| `GET /v1/tenants` | List tenants |
| `GET /v1/tenants/{tenant}` | Show one |
| `PATCH /v1/tenants/{tenant}` | Change `name`, `max_instances` or `requests_per_second`; absent fields keep their value |
| `DELETE /v1/tenants/{tenant}` | Delete; `409` while it still has instances |
| `POST /v1/tenants/{tenant}/keys` | Issue a key (shown only in this response) |
| `GET /v1/tenants/{tenant}/keys` | List key ids and creation times |
| `DELETE /v1/tenants/{tenant}/keys/{key}` | Revoke a key by its id |

`max_instances` is the tenant's instance quota (`0` = no quota).
`requests_per_second` is its API rate (`0` = node default, at most 10000).

```bash
curl -X POST http://localhost:8080/v1/tenants \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"id": "acme", "name": "Acme Inc.", "max_instances": 10, "requests_per_second": 50}'

curl -X PATCH http://localhost:8080/v1/tenants/acme \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"max_instances": 20}'

curl -X POST http://localhost:8080/v1/tenants/acme/keys -H "Authorization: Bearer $ADMIN_KEY"
# {"id":"0123456789abcdef","key":"wax_0123456789abcdef_<secret>","created_at":"..."}

curl -X DELETE http://localhost:8080/v1/tenants/acme/keys/0123456789abcdef \
  -H "Authorization: Bearer $ADMIN_KEY"
```

Instances that existed before tenants were introduced belong to the tenant
`default`.

All routes from here on use a **tenant key**. The examples assume:

```bash
export WAX=http://localhost:8080
export WAX_KEY=wax_0123456789abcdef_<secret>
```

## Instances

| Method and path | Purpose |
|---|---|
| `POST /v1/instances` | Create `{id, webhook_url, webhook_secret, rate_per_minute}` and start pairing |
| `GET /v1/instances` | List the tenant's instances |
| `GET /v1/instances/{id}` | Status of one instance |
| `PATCH /v1/instances/{id}` | Change `webhook_url`, `webhook_secret` or `rate_per_minute`; absent fields keep their value |
| `DELETE /v1/instances/{id}` | Unlink the device from the phone (best effort) and delete all of its data |
| `POST /v1/instances/{id}/connect` | Start a new pairing or reconnect (after `qr_timeout` or `replaced`); resume a stopped instance |
| `POST /v1/instances/{id}/stop` | Stop without unlinking: the device and data stay, `connect` resumes without a QR code |

Fields:

- `id`: the instance name, unique within the tenant.
- `webhook_url`: absolute `http` or `https` URL without credentials. Empty
  means no webhooks. See [webhooks.md](webhooks.md).
- `webhook_secret`: key for the `X-Wax-Signature` HMAC. Write-only: it is
  never returned. Omitting it in a `PATCH` keeps the current secret.
- `rate_per_minute`: send rate of the outbox, up to 600 (`0` = default,
  20 per minute, with a burst of 3).

```bash
curl -X POST $WAX/v1/instances -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"id": "main", "webhook_url": "https://hooks.example.com/wax", "webhook_secret": "<random>", "rate_per_minute": 30}'

curl $WAX/v1/instances/main -H "Authorization: Bearer $WAX_KEY"
# {"id":"main","status":"connected","jid":"905550000001@s.whatsapp.net","lid":"123456789012345@lid",
#  "webhook_url":"https://hooks.example.com/wax","rate_per_minute":30,"qr_pending":false}
```

Creating an instance beyond the tenant's quota returns `403`.

**Stopping and deleting.** `POST /v1/instances/{id}/stop` (`204`) ends the
connection but keeps everything: the device stays linked on the phone, and
the keys, sessions and messages stay in the database. No node runs a
stopped instance, also after restarts, until `POST /v1/instances/{id}/connect`
starts it again with the stored session, without a QR code. Messages sent
while it is stopped are queued and go out after it resumes; webhook events
it queued wait as well. `DELETE` instead unlinks the device from the phone
and deletes all data; it can unlink only a running instance, so to remove a
stopped one completely, `connect` it first.

**Statuses.**

| Status | Meaning |
|---|---|
| `starting` | Just started. |
| `qr` | Waiting for the QR code to be scanned (`qr_pending` is true). |
| `qr_timeout` | No code was scanned in time; call `connect` to try again. |
| `connecting` | Connecting or reconnecting. |
| `connected` | Logged in and online. |
| `logged_out` | Unlinked from the phone; a new QR pairing starts. |
| `replaced` | Another connection took over this session; call `connect`. |
| `outdated` | WhatsApp rejected the client version; retried later. |
| `banned` | Temporarily banned by WhatsApp; retried later. |
| `failed` | Unexpected error; retried later. |
| `stopped` | Stopped through `POST /v1/instances/{id}/stop`; `connect` resumes it. |
| `unassigned` | No node runs it at the moment (none alive, or all at capacity). |

## QR code

```bash
# JSON: {"code": "<raw QR content>", "png_base64": "<PNG image>"}
curl $WAX/v1/instances/main/qr -H "Authorization: Bearer $WAX_KEY"

# PNG image
curl "$WAX/v1/instances/main/qr?format=png" -H "Authorization: Bearer $WAX_KEY" -o qr.png
```

`404` means no code is pending (the error message includes the current
status). Codes rotate while pairing; fetch a new one rather than caching
it. The same codes also arrive as `qr` webhooks.

## Messages

`POST /v1/instances/{id}/messages` queues a message and answers `202`:

```json
{"outbox_id": 42, "message_id": "3EB0C0FFEE...", "status": "pending", "duplicate": false}
```

A request contains `to` and exactly one of `text`, `media` or `forward`.
`reply_to` and `mentions` go with `text` or `media`. `client_id` is
optional (see [Idempotency](#idempotency)).

### Text

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"to": "905550000002", "text": "Hello!", "client_id": "order-1001-confirm"}'
```

Sending to a group: use its JID as `to`, for example
`"to": "120363000000000001@g.us"`.

### Media

```json
{
  "to": "905550000002",
  "media": {
    "kind": "image",
    "mimetype": "image/jpeg",
    "caption": "Your invoice",
    "file_name": "invoice.jpg",
    "data_base64": "<base64 of the file>"
  }
}
```

| Field | Notes |
|---|---|
| `data_base64` | Required. The file, standard base64. |
| `kind` | `image`, `video`, `audio`, `voice` (voice note), `document` or `sticker`. Default: derived from the MIME type (a WebP image is sent as an image unless `kind` is `sticker`). |
| `mimetype` | Default: detected from the content. |
| `caption` | Shown under images, videos and documents. Put text here, not in `text`. |
| `file_name` | File name shown for documents. |
| `thumbnail_base64` | Optional small JPEG preview of an image or video, at most 100 KiB. |
| `width`, `height` | Optional image or video dimensions, for the preview. |
| `seconds` | Optional length of a video or audio. |
| `waveform_base64` | Optional voice note waveform: 64 bytes, each 0 to 100 (at most 256 bytes accepted). |

The previews are optional; without them the message still arrives, but the
recipient's chat shows less before downloading.

A **sticker** is a WebP image (static or animated), sent with
`"kind": "sticker"`; it has no caption or file name. wax reads its size and
whether it is animated from the file, and refuses other formats with `400`.
WhatsApp's own stickers are 512 by 512 pixels.

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d "{\"to\": \"905550000002\", \"media\": {\"kind\": \"document\", \"mimetype\": \"application/pdf\",
       \"file_name\": \"invoice.pdf\", \"data_base64\": \"$(base64 < invoice.pdf | tr -d '\n')\"}}"
```

### Voice notes

Use `"kind": "voice"`. WhatsApp expects Ogg Opus (`audio/ogg; codecs=opus`).
Recordings in **WebM Opus** (`audio/webm`, what browsers' `MediaRecorder`
produces) are converted to Ogg Opus on the server, and if `seconds` is not
given it is taken from the file.

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d "{\"to\": \"905550000002\", \"media\": {\"kind\": \"voice\", \"mimetype\": \"audio/ogg; codecs=opus\",
       \"data_base64\": \"$(base64 < note.ogg | tr -d '\n')\"}}"
```

### Replies

`reply_to` is the id of a message in the same chat. The quoted message is
taken from the stored history, so it must be a message wax has seen (live
or through history sync).

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"to": "905550000002", "text": "Yes, confirmed.", "reply_to": "3EB0A1B2C3D4E5F6"}'
```

### Mentions

`mentions` lists the users tagged in the text or caption (phone numbers or
user JIDs). The text should contain `@<number>` for each of them, which is
how WhatsApp renders the mention.

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"to": "120363000000000001@g.us", "text": "@905550000002 please check this", "mentions": ["905550000002"]}'
```

### Forwarding

`forward` sends a copy of a stored message, marked as forwarded. It cannot be
combined with `text`, `media`, `reply_to` or `mentions`. View once messages,
reactions, edits and system messages cannot be forwarded.

```bash
curl -X POST $WAX/v1/instances/main/messages -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"to": "905550000003", "forward": {"chat": "905550000002@s.whatsapp.net", "message_id": "3EB0A1B2C3D4E5F6"}}'
```

### Outbox status

Messages go out in order, paced by the instance's `rate_per_minute`, while
the instance is connected. Order is kept per chat: a message waiting for a
retry holds back later messages to the same chat, not to other chats.
Failed sends are retried with backoff; errors
that cannot be fixed by retrying (for example a number that is not on
WhatsApp) fail at once.

```bash
curl $WAX/v1/instances/main/outbox/42 -H "Authorization: Bearer $WAX_KEY"
# {"outbox_id":42,"client_id":"order-1001-confirm","message_id":"3EB0C0FFEE...","chat":"905550000002@s.whatsapp.net",
#  "status":"sent","attempts":0,"last_error":"","server_timestamp":"2026-01-01T12:00:01Z","created_at":"2026-01-01T12:00:00Z"}
```

`status` is `pending`, `sent` or `failed`. The `send_status` webhook reports
the same transition, so polling is optional.

## Chats and messages

```bash
# Chats, most recent first, each with its newest message
curl "$WAX/v1/instances/main/chats?limit=50&offset=0" -H "Authorization: Bearer $WAX_KEY"
```

`limit` defaults to 50 (at most 500). Each chat has `jid`, `name`,
`phone`, `push_name`, `username`, `unread_count`, `archived`, `pinned`,
`muted_until`, `last_message_at` and `last_message`.

```bash
# Messages of a chat, newest first
curl "$WAX/v1/instances/main/chats/905550000002@s.whatsapp.net/messages?limit=50" -H "Authorization: Bearer $WAX_KEY"

# Older page: messages before a point in time (RFC 3339)
curl "$WAX/v1/instances/main/chats/905550000002@s.whatsapp.net/messages?before=2026-01-01T00:00:00Z" -H "Authorization: Bearer $WAX_KEY"

# Only some types
curl "$WAX/v1/instances/main/chats/120363000000000001@g.us/messages?types=image,video,document" -H "Authorization: Bearer $WAX_KEY"
```

| Parameter | Meaning |
|---|---|
| `limit` | Page size, default 50, at most 500. |
| `before` | Only messages sent before this RFC 3339 time. To page back, pass the `timestamp` of the oldest message you have. |
| `types` | Comma-separated stored message types to keep: `text`, `reaction`, `image`, `video`, `gif`, `audio`, `ptt` (voice notes), `document`, `sticker`, or `stub:<n>` for system messages. |

Here `{chat}` is a chat JID. A contact's chat may exist under both its
phone-number JID and its LID; the listing returns both together.

In listings, `type` is the stored type (so voice notes appear as `ptt`).
Otherwise messages have the same shape as the `message` webhook (`id`, `chat`,
`sender`, `from_me`, `timestamp`, `type`, `text`, `media`, `quoted`,
`mentions`, `forwarded`, `view_once`, `stub` for system messages, and so
on). Listings also add what is known about the sender: `sender_phone`,
`sender_name` (address book) and `sender_username`.

## Media download

A message with media has `media.download`, an API path to fetch and decrypt
it:

```bash
curl "$WAX/v1/instances/main/chats/905550000002@s.whatsapp.net/messages/3EB0A1B2C3D4E5F6/media" \
  -H "Authorization: Bearer $WAX_KEY" -o photo.jpg
```

The instance must be connected (`503` otherwise). `404` means the message
has no media. The response is sent as an attachment with a sniffed content
type and `X-Content-Type-Options: nosniff`, because the bytes come from
another user.

## Profile pictures

```bash
curl "$WAX/v1/instances/main/chats/905550000002/picture" -H "Authorization: Bearer $WAX_KEY" -o avatar.jpg
curl "$WAX/v1/instances/main/chats/120363000000000001@g.us/picture" -H "Authorization: Bearer $WAX_KEY" -o group.jpg
```

Works for contacts and groups; `{chat}` may be a phone number or a JID.
`404` when there is no picture or it is hidden by privacy settings. Needs a
connected instance. Responses may be cached for an hour.

## Checking phone numbers

```bash
curl -X POST $WAX/v1/instances/main/contacts/check -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"phones": ["905550000002", "+905550000003"]}'
# [{"phone":"905550000002","on_whatsapp":true,"jid":"905550000002@s.whatsapp.net","lid":"123456789012345@lid"},
#  {"phone":"905550000003","on_whatsapp":false}]
```

Up to 50 numbers per request; one result per number, in order. It is the
same check a send makes: a number is on WhatsApp when the server returns
its LID. Numbers whose LID is already known are answered without asking
WhatsApp. The account must be connected (`503` otherwise).

Checks run one at a time and share the limit of contact profile lookups.
When WhatsApp asks to slow down, the request fails with `429`,
`Retry-After: 60` and the error `rate limited by WhatsApp: try again in a
minute`, and further checks fail the same way for a minute without asking
WhatsApp. Check numbers when you need them (for example before starting a
new chat), not in bulk: sending to a number that is not on WhatsApp fails
on its own with the code `not_on_whatsapp`.

## Contact profiles

```bash
curl "$WAX/v1/instances/main/chats/905550000002/profile" -H "Authorization: Bearer $WAX_KEY"
```

Returns everything known about a contact: `jid`, `phone`, `lid`, `name`
(address book), `push_name`, `username`, and when WhatsApp answers
(`"live": true`) also `about`, `about_set_at`, `about_hidden`, `devices`,
`is_business` and `business` (the business profile). `common_groups` lists
joined groups the contact is in, and `messages` counts the stored messages
of the chat by type.

WhatsApp allows only a few live lookups in a row; when it asks to slow down,
the response still comes back with `"live": false` and the stored fields.
For a group JID only `messages` is filled in (use the group endpoint below
for the rest).

## Groups and communities

```bash
# Joined groups and communities (no participant lists)
curl $WAX/v1/instances/main/groups -H "Authorization: Bearer $WAX_KEY"
# [{"jid":"120363000000000001@g.us","name":"Team","is_community":false,"community":"120363000000000009@g.us",
#   "default_sub_group":false,"announce":false,"participants":12}, ...]

# One group's metadata with participants
curl $WAX/v1/instances/main/groups/120363000000000001@g.us -H "Authorization: Bearer $WAX_KEY"
```

In the list, `is_community` marks a community (the parent), `community`
names the community a group belongs to, and `default_sub_group` marks a
community's announcement group.

The group endpoint returns the subject (`name`), `topic`, `owner`,
`created`, `addressing_mode` (`lid` or `pn`), `announce`, `locked`,
`ephemeral` (disappearing messages timer in seconds), `member_add_mode`,
`join_approval`, `size` and `participants`. Each participant includes what
is known about it: `phone`, `name`, `push_name` and `username`. Both
endpoints need a connected instance.

## Read receipts

```bash
curl -X POST $WAX/v1/instances/main/read -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"chat": "905550000002@s.whatsapp.net", "ids": ["3EB0A1B2C3D4E5F6"]}'
```

In a group, also pass `sender`, the JID of the author of those messages:

```bash
  -d '{"chat": "120363000000000001@g.us", "sender": "905550000002@s.whatsapp.net", "ids": ["3EB0A1B2C3D4E5F6"]}'
```

Answers `204`.

## Presence

Sets the account's own availability (online or offline):

```bash
curl -X POST $WAX/v1/instances/main/presence -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"available": true}'
```

Answers `204`. Contacts' presence and typing arrive as `presence` and
`chat_presence` webhooks.

## Calls

wax cannot take calls, but it can reject them. Incoming calls arrive as
`call` webhooks with `call_id` and `from`; reject one with:

```bash
curl -X POST $WAX/v1/instances/main/calls/<call_id>/reject -H "Authorization: Bearer $WAX_KEY" -H "Content-Type: application/json" \
  -d '{"from": "905550000002@s.whatsapp.net"}'
```

`from` is the caller's JID from the webhook. Answers `204`.

## Clusters and routing

When several nodes share a database, a request can be sent to any of them.
A request for an instance that runs on another node is forwarded to that
node once; listings are answered by any node from the database. If the
owning node is unreachable, the API answers `503`; retry shortly. See
[operations.md](operations.md#running-several-nodes).
