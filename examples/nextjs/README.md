# wax with Next.js

wax runs as a service next to your application, like a self-hosted
backend: your code calls its REST API and receives every WhatsApp event as
a webhook. This example runs three services with Docker Compose:

| Service | What it is | Address on the compose network |
|---|---|---|
| `postgres` | wax's database | `postgres:5432` |
| `wax` | the wax server, API only (`ghcr.io/mdenesfe/wax`) | `http://wax:8080` |
| `web` | a Next.js application (`web/`) | `http://web:3000`, published on `localhost:3000` |

Only the application is published; wax and PostgreSQL are reachable only
inside the compose network.

## Run it

```bash
cd examples/nextjs
cp .env.example .env     # three secrets, each from: openssl rand -base64 32
docker compose up -d --build
```

There is no interface: wax runs with `-no-ui` and the application only has
API routes. Link an account and fetch the QR code:

```bash
curl -X POST localhost:3000/api/connect          # creates the account, pairing starts
curl localhost:3000/api/status                   # {"instance":{"status":"qr","qr_pending":true,...}}
curl -o qr.png localhost:3000/api/qr             # scan it: WhatsApp, Linked devices, Link a device
```

Then:

```bash
curl localhost:3000/api/status                   # "status":"connected"
curl -X POST localhost:3000/api/send -H "Content-Type: application/json" \
  -d '{"to": "905550000002", "text": "Hello"}'
curl "localhost:3000/api/events?event=message"   # received messages, newest first
```

| Route | What it does |
|---|---|
| `POST /api/connect` | Creates the account with this application's webhook, or starts a new pairing |
| `GET /api/status` | The account's status (`null` before `connect`) |
| `GET /api/qr` | The current QR code as PNG (`404` when none is pending) |
| `POST /api/send` | Sends `{to, text}` |
| `GET /api/events` | Webhook events received so far; `?event=` filters by name |
| `POST /api/wax` | The webhook receiver (called by wax) |

These routes are the example's own and have no authentication; the port is
published on `127.0.0.1` only. Put your application's own access control
in front of them before exposing it.

## How it fits together

```
client ──> web (Next.js) ──REST──> wax ──> WhatsApp
                 ^                   │
                 └──── webhooks ─────┘
```

- **Keys stay in the application.** Clients talk only to the
  application's own routes (`web/app/api/*`); those call wax with the key.
- **Tenant key.** wax's admin key (`WAX_API_KEY`) manages tenants only. On
  first use the application creates the tenant `app`, issues a tenant key
  and keeps it in the `webdata` volume ([web/lib/wax.ts](web/lib/wax.ts)).
  To manage keys yourself, issue one and set `WAX_TENANT_KEY`.
- **Webhooks.** The account is created with
  `webhook_url: http://web:3000/api/wax`. The receiver
  ([web/app/api/wax/route.ts](web/app/api/wax/route.ts)) checks the
  `X-Wax-Signature` HMAC over the raw body, drops repeated deliveries by
  `X-Wax-Delivery`, and answers at once. wax refuses private webhook
  addresses by default, so the `wax` service runs with
  `-allow-private-webhooks`.
- **Sending.** [web/app/api/send/route.ts](web/app/api/send/route.ts)
  posts to `/v1/instances/main/messages` with a `client_id`, so a retried
  request does not send twice.

The events are kept in memory to keep the example short; a real
application stores them or queues the work. See the
[REST API guide](../../docs/usage/api.md) and the
[webhook guide](../../docs/usage/webhooks.md) for everything else.

## Developing the application

Run wax and PostgreSQL in Docker and Next.js on the host:

```bash
docker compose up -d postgres wax
```

wax is then not published, so for development add a port to the `wax`
service (`ports: ["127.0.0.1:8080:8080"]`) and point the application at it:

```bash
cd web && npm install
WAX_URL=http://127.0.0.1:8080 WAX_API_KEY=... WAX_WEBHOOK_SECRET=... \
WAX_WEBHOOK_URL=http://host.docker.internal:3000/api/wax npm run dev
```

The tenant key is written to `/data`, which may not exist on the host: set
`WAX_TENANT_KEY` instead (issue one with
`POST /v1/tenants/{tenant}/keys`).
