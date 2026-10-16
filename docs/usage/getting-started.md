# Getting started

This guide takes you from a fresh checkout to a linked WhatsApp account that
sends a message and delivers incoming events to a webhook. It uses the
development PostgreSQL from `docker-compose.yml` and runs everything on
`localhost`.

## Requirements

- **Go 1.25 or newer** (`go.mod` declares `go 1.25.0`).
- **Docker** with Compose, for the development PostgreSQL. Any PostgreSQL
  you can reach works too; Docker is only the quickest way to get one.
- A phone with WhatsApp, to scan the QR code (WhatsApp, Settings, Linked
  devices, Link a device).

`protoc` and `protoc-gen-go` are needed only if you change the `.proto`
files; using wax does not need them.

## 1. Start PostgreSQL

```bash
docker compose up -d
```

This starts PostgreSQL 17 on `127.0.0.1:5433` with user, password and
database all set to `wax`, and also creates a separate `wax_test` database
for the test suite. The data lives in the `wax-pg` Docker volume.

## 2. Set the three secrets

wax reads **only secrets** from the environment. Everything else is a
command-line flag, so `.env` holds these values and nothing more:

| Variable | What it is | How to generate it |
|---|---|---|
| `WAX_API_KEY` | The admin key: a bearer token that manages tenants and their keys. At least 32 characters. | `openssl rand -hex 32` |
| `WAX_MASTER_KEY` | 32 random bytes, base64 encoded. Encrypts all private keys, sessions and other secrets at rest, and keys the HMAC of tenant API keys. | `openssl rand -base64 32` |
| `WAX_DATABASE_URL` | PostgreSQL DSN. It contains the database password, which is why it counts as a secret. | Your database; the compose default is below. |

Copy the template and fill it in:

```bash
cp .env.example .env
```

```dotenv
WAX_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable
WAX_MASTER_KEY=<output of: openssl rand -base64 32>
WAX_API_KEY=<output of: openssl rand -hex 32>
```

(`.env.example` also lists `WAX_TEST_DATABASE_URL`, which only the test
suite reads.)

If `WAX_DATABASE_URL` is empty, wax falls back to the compose database
above. `WAX_MASTER_KEY` has no default: the process refuses to start without
a valid one.

> **Keep `WAX_MASTER_KEY` safe and separate from database backups.** Without
> it the sealed data in the database cannot be decrypted: every account has
> to be linked again and every tenant key reissued. See
> [operations.md](operations.md#backups-and-the-master-key).

## 3. Run the server

```bash
set -a; source .env; set +a
go run ./cmd/wax serve -listen 127.0.0.1:8080 -log-level info
```

Or build a binary first:

```bash
go build -o wax ./cmd/wax
./wax serve -listen 127.0.0.1:8080
```

`serve` is the default command, so `./wax` alone does the same. On start the
server connects to the database, applies any pending migrations, detects
the current WhatsApp Web version and starts listening. Logs are JSON lines
on stderr. `Ctrl+C` (SIGINT) or SIGTERM shuts it down gracefully.

Check that it is up:

```bash
curl http://localhost:8080/healthz
# {"status":"ok"}
```

All flags are described in [operations.md](operations.md#wax-serve-flags).

## 4. Open the admin interface

Open <http://localhost:8080/> in a browser (it redirects to `/ui/`). Sign in
with the value of `WAX_API_KEY`. The admin key can only manage tenants and
keys, so the next step is to create a tenant.

## 5. Create a tenant and a tenant key

In the admin interface: **New tenant**, enter an id such as `acme`, then
open the tenant and click **Create key**. The key is shown once; copy it.

The same with curl:

```bash
export ADMIN_KEY=<your WAX_API_KEY>

curl -s -X POST http://localhost:8080/v1/tenants \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"id": "acme", "name": "Acme Inc."}'

curl -s -X POST http://localhost:8080/v1/tenants/acme/keys \
  -H "Authorization: Bearer $ADMIN_KEY"
# {"id":"0123456789abcdef","key":"wax_0123456789abcdef_<secret>","created_at":"..."}
```

Store the `key` value now: the server keeps only an HMAC of it and cannot
show it again.

```bash
export WAX_KEY=wax_0123456789abcdef_<secret>
```

## 6. Add a WhatsApp account and scan the QR code

An *instance* is one linked device, that is, one WhatsApp account.

**In the admin interface:** sign out, sign in again with the tenant key,
click **Add WhatsApp account**, give it a name (for example `main`) and scan
the QR code that appears.

**With curl:**

```bash
curl -s -X POST http://localhost:8080/v1/instances \
  -H "Authorization: Bearer $WAX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"id": "main"}'
```

The instance starts right away and asks WhatsApp for a pairing code. Fetch
it as a PNG and open it:

```bash
curl -s -H "Authorization: Bearer $WAX_KEY" \
  "http://localhost:8080/v1/instances/main/qr?format=png" -o qr.png
```

Scan it from the phone (Linked devices, Link a device). Codes rotate, so
fetch a fresh one if the scan fails. Without `?format=png` the endpoint
returns JSON with the raw `code` and a base64 PNG. If nobody scans in time
the status becomes `qr_timeout`; start again with
`POST /v1/instances/main/connect`.

Wait for the status to become `connected`:

```bash
curl -s -H "Authorization: Bearer $WAX_KEY" http://localhost:8080/v1/instances/main
# {"id":"main","status":"connected","jid":"905550000001@s.whatsapp.net",...}
```

After this, restarts reconnect without a QR code. The history sync from the
phone continues in the background for a while.

## 7. Send a first message

```bash
curl -s -X POST http://localhost:8080/v1/instances/main/messages \
  -H "Authorization: Bearer $WAX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"to": "905550000002", "text": "Hello from wax", "client_id": "hello-1"}'
# {"outbox_id":1,"message_id":"3EB0...","status":"pending","duplicate":false}
```

`to` is a phone number in international format (digits, optional `+`) or a
JID. The message is queued (`202 Accepted`) and sent by the instance's
outbox. Check its progress:

```bash
curl -s -H "Authorization: Bearer $WAX_KEY" http://localhost:8080/v1/instances/main/outbox/1
```

The status moves from `pending` to `sent` (or `failed`, with `last_error`).
Sending the same `client_id` again returns the first result with
`"duplicate": true` instead of sending twice. More send options (media,
voice notes, replies, mentions, forwarding) are in [api.md](api.md#messages).

## 8. Receive webhooks

Give the instance a webhook URL and a secret:

```bash
curl -s -X PATCH http://localhost:8080/v1/instances/main \
  -H "Authorization: Bearer $WAX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"webhook_url": "https://hooks.example.com/wax", "webhook_secret": "<a long random string>"}'
```

You can also pass `webhook_url` and `webhook_secret` when creating the
instance. Every event is then POSTed to that URL as JSON:

```json
{
  "event": "message",
  "instance": "main",
  "timestamp": "2026-01-01T12:00:00Z",
  "data": { "id": "3EB0...", "chat": "905550000002@s.whatsapp.net", "type": "text", "text": "Hi!", "from_me": false, "...": "..." }
}
```

with an `X-Wax-Signature: sha256=<hex>` header you should verify. Answer
with any `2xx` status; anything else is retried.

**Receiver on the same machine.** By default wax refuses to deliver to
loopback and private addresses (protection against server-side request
forgery). For local testing, start the server with `-allow-private-webhooks`
and use a URL such as `http://localhost:9000/hook`. Do not enable it in
production unless the receiver really is on a private network.

A quick receiver for testing, in one line of Python:

```bash
python3 -c 'import http.server as h
class H(h.BaseHTTPRequestHandler):
  def do_POST(s):
    print(s.headers.get("X-Wax-Signature"), s.rfile.read(int(s.headers["Content-Length"])).decode()); s.send_response(204); s.end_headers()
h.HTTPServer(("127.0.0.1", 9000), H).serve_forever()'
```

Signature verification, retries and the full event list are in
[webhooks.md](webhooks.md).

## Single instance on the console: `wax run`

`wax run` runs one account in the terminal, without the REST API. It is
meant for debugging and for exploring the protocol. It needs
`WAX_MASTER_KEY` and the database (`WAX_DATABASE_URL`, or the compose
default), but not `WAX_API_KEY`.

```bash
set -a; source .env; set +a
go run ./cmd/wax run -instance test
```

On first start it prints a QR code in the terminal; after pairing, running
the same command logs in without one. Flags:

| Flag | Meaning |
|---|---|
| `-instance <id>` | Instance id (default `default`). One linked device per id. |
| `-debug` | Debug logging. |
| `-dump-nodes` | With `-debug`: log every received protocol node, masked. |
| `-unmask` | With `-dump-nodes`: do not mask payloads and JIDs. Logs then contain message contents and phone numbers. |
| `-version <v>` | WhatsApp Web version to announce, for example `2.3000.X` (default: detect). |
| `-no-version-check` | Do not query web.whatsapp.com for the current version. |
| `-qr-png <file>` | Also write the current QR code to this PNG file. |
| `-once` | Exit instead of starting a new pairing after logout or QR timeout. |
| `-send-to <number or JID>` | After connecting, send `-text` once to this recipient. |
| `-text <text>` | Text for `-send-to` (the caption with `-send-file`). |
| `-send-file <path>` | With `-send-to`: send this file instead of a text. |
| `-media-dir <dir>` | Save received media files into this directory. |
| `-reject-calls` | Reject incoming calls automatically. |
| `-push-name <name>` | Your display name (needed for presence until app state has synced). |

While it runs, stdin accepts:

```
/send <number|jid> <text>
/media <number|jid> <file> [caption]
/group <jid>
```

`wax run` logs as plain text, not JSON. It stores its account in the same
database as `wax serve` but does not take part in the leases that
`wax serve` nodes use, so do not point it at an account a server is
running.
