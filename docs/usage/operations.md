# Operations

This page covers running wax in production: configuration, tenants and
limits, clusters of several nodes, metrics, logs, the database and the
master key.

## Configuration model

wax has two sources of configuration, and they never overlap:

- **Environment variables carry secrets only:** `WAX_API_KEY`,
  `WAX_MASTER_KEY` and `WAX_DATABASE_URL`. Keeping them out of the command
  line means they never show up in shell history or `ps` output.
- **Flags carry everything else.**

| Variable | Required | Notes |
|---|---|---|
| `WAX_API_KEY` | `serve` only | Admin bearer token, at least 32 characters (`openssl rand -hex 32`). The server refuses to start with a shorter one. |
| `WAX_MASTER_KEY` | yes | 32 bytes, base64 (`openssl rand -base64 32`; standard or URL-safe, padded or not). |
| `WAX_DATABASE_URL` | no | PostgreSQL DSN. Default: `postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable` (the compose database). |

## `wax serve` flags

`wax serve` is the default command (`wax` alone runs it). `wax serve -h`
prints the same list.

| Flag | Default | Meaning |
|---|---|---|
| `-listen <addr>` | `127.0.0.1:8080` | Address of the API and the admin interface. |
| `-log-level <level>` | `info` | `debug`, `info`, `warn` or `error`. |
| `-version <v>` | (detect) | WhatsApp Web version to announce. By default the live version is detected, falling back to the built-in one. |
| `-no-version-check` | off | Do not query web.whatsapp.com for the current version (neither at start nor periodically). |
| `-allow-private-webhooks` | off | Allow webhook receivers on loopback and private networks. Off by default to prevent SSRF; see [webhooks.md](webhooks.md#private-address-protection). |
| `-node-id <id>` | hostname + random suffix | This node's id when several nodes share the database. |
| `-advertise <url>` | `http://<listen>` | URL where the other nodes reach this node's API, for forwarded requests. |
| `-max-instances <n>` | `0` (no limit) | Most instances this node runs. |
| `-tenant-rps <n>` | `20` | API requests per second per tenant on this node, for tenants without their own `requests_per_second`. |
| `-metrics-listen <addr>` | (off) | Serve Prometheus metrics at `http://<addr>/metrics`. |

Example:

```bash
wax serve -listen 0.0.0.0:8080 -advertise http://10.0.0.1:8080 -node-id n1 \
  -metrics-listen 127.0.0.1:9090 -log-level info
```

The HTTP server allows up to 5 minutes for reading a request and writing a
response (large media uploads and downloads). wax itself serves plain HTTP:
terminate TLS in a reverse proxy or load balancer in front of it.

**WhatsApp Web version.** While the version check is on, the node also
checks for a newer WhatsApp Web version every 6 hours and uses it for
future connections. Instances that WhatsApp rejects as outdated get the
status `outdated` and are retried later.

## Tenants and quotas

The API is multi-tenant. The admin key (`WAX_API_KEY`) manages tenants and
their keys; tenant keys manage that tenant's instances only, and other
tenants' instances are invisible to them (`404`). Tenants are created and
changed through the API or the admin interface (see
[api.md](api.md#tenants-and-keys-admin-key)).

Each tenant has two limits:

- `max_instances`: the instance quota (`0` = none). It is enforced
  atomically in the database, so concurrent creates cannot exceed it.
  Above it, `POST /v1/instances` answers `403`.
- `requests_per_second`: the API rate (`0` = the node's `-tenant-rps`).
  Above it the API answers `429` with `Retry-After`.

Changes through `PATCH /v1/tenants/{tenant}` apply to the next request. A
tenant can be deleted only after all of its instances are deleted. Revoking
a key takes effect immediately because keys are verified against the
database on every request.

Instances that existed before tenants were introduced were moved to a
tenant called `default` by the migration.

## Resource isolation

wax runs many accounts for many tenants in one process. These limits keep
one tenant or one account from starving the others:

| Resource | Limit |
|---|---|
| API rate | Per tenant, token bucket with a burst of two seconds' worth. Enforced per node. |
| Instances | `max_instances` per tenant (cluster-wide, in the database); `-max-instances` per node. |
| Large uploads | Request bodies of 1 MiB or more (media sends) are decoded and encrypted in memory, so at most 4 run at once per node, and at most 2 of them for one tenant. Others wait for a slot. Every request body is capped at 90 MiB. |
| History sync | Processing of history chunks is limited to 2 at a time per node, across all accounts. |
| Sends | Each instance's outbox is paced by its `rate_per_minute` (default 20, burst 3, at most 600). |
| Database connections | Set `pool_max_conns` in the DSN, for example `...?sslmode=disable&pool_max_conns=20`. The default is pgx's: the larger of 4 and the number of CPUs. The node logs the pool size at start (`database connected`, `pool_max_conns`). |

With N nodes behind a load balancer, a tenant can reach up to N times its
API rate. Requests that one node forwards to another carry a signature
keyed from `WAX_MASTER_KEY`, so they are not counted twice, and a client
cannot forge that header to skip the limit.

## Running several nodes

Any number of `wax serve` processes can share one database. Each instance
runs on exactly one node at a time.

```bash
wax serve -listen 10.0.0.1:8080 -node-id n1
wax serve -listen 10.0.0.2:8080 -node-id n2
```

All nodes must use the **same `WAX_MASTER_KEY`** and database. `WAX_API_KEY`
should also be the same, since a client may reach any node.

**Node id.** Each node needs a unique id. If you do not set `-node-id`, a
new one (hostname plus a random suffix) is generated on every start, which
is fine. Never give two running nodes the same id.

**Advertise address.** A request may reach any node. If it is for an
instance that runs on another node, it is forwarded once to the owner at
its advertised URL. `-advertise` defaults to `http://<listen>`, which only
works when `-listen` is an address the other nodes can reach. When you
listen on `0.0.0.0` or behind NAT, set it explicitly:

```bash
wax serve -listen 0.0.0.0:8080 -advertise http://10.0.0.1:8080 -node-id n1
```

The forwarded request keeps the client's `Authorization` header, which the
owner checks again. Keep the network between nodes private, or put TLS
between them (an `https://` advertise URL). If the owner cannot be reached,
the client gets `503` and should retry. Listings (`GET /v1/instances` and
so on) are answered from the database by any node.

**Leases.** A node owns an instance through a lease row in the database:

- Leases last 30 seconds and are renewed every 10 seconds. All times are
  database time, so node clocks do not need to agree.
- Between rounds, a node polls every 2 seconds for free leases when it runs
  fewer instances than its share.
- **Balancing:** each node aims for a fair share, the number of instances
  divided by the number of live nodes, rounded up, and at most
  `-max-instances`. A node that joins takes over the surplus of the others;
  a node above its share releases the extra ones.
- Instances that no node can take (no node alive, or all at capacity) show
  the status `unassigned`.

**Failover.**

- **Graceful stop** (SIGTERM or SIGINT): the node stops its instances and
  releases their leases. Another node takes them over in about 2 seconds
  and reconnects them without a QR code.
- **Crash:** the leases are not released, so takeover waits until they
  expire, at most 30 seconds. It never happens sooner, so an account never
  runs on two nodes at once.
- **Database unreachable:** a node that cannot renew its leases stops all
  its instances one round (10 seconds) before the leases could expire, so
  that another node can take them over safely. When the database is back,
  the node claims its share again.
- **Limit:** a node that freezes for longer than a lease (for example a
  suspended VM) and then resumes may briefly overlap with the new owner.
  WhatsApp then disconnects one of the two connections (`replaced`), and
  that instance waits for `POST /v1/instances/{id}/connect`.

Lease activity is visible in `wax_lease_events_total` and in the logs
(`instance acquired`, `instances released for rebalancing`, `lease lost`,
`database unreachable; stopping instances before their leases expire`).

## Metrics

Start the node with `-metrics-listen` to serve Prometheus metrics:

```bash
wax serve -metrics-listen 127.0.0.1:9090
curl http://127.0.0.1:9090/metrics
```

The metrics endpoint is on its own address and has **no authentication**.
Bind it to an internal address only. Metrics are produced by wax itself (no
client library) in the Prometheus text format. Each node reports the
instances it runs, so scrape every node.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wax_instance_status` | gauge | `tenant`, `instance`, `status` | 1 for the current status of each instance on this node. |
| `wax_instances_running` | gauge | | Instances running on this node. |
| `wax_messages_received_total` | counter | `tenant`, `instance` | Messages received and decrypted. |
| `wax_messages_sent_total` | counter | `tenant`, `instance`, `result` | Outbox items finished (`sent`, `failed`). |
| `wax_decrypt_failures_total` | counter | `tenant`, `instance`, `kind` | Undecryptable messages (`recoverable`, fixed by retry receipts, or `error`). |
| `wax_disconnects_total` | counter | `tenant`, `instance` | Connections that ended. |
| `wax_outbox_pending` | gauge | `tenant`, `instance` | Messages waiting to be sent. |
| `wax_webhooks_pending` | gauge | `tenant`, `instance` | Webhook events waiting for delivery. |
| `wax_webhook_deliveries_total` | counter | `tenant`, `instance`, `result` | Delivery attempts (`ok`, `retry`, `dead`). |
| `wax_webhook_delivery_seconds` | histogram | `tenant` | Duration of webhook HTTP requests. |
| `wax_api_requests_total` | counter | `route`, `code` | API requests by route pattern (for example `GET /v1/instances/{id}`) and status code. |
| `wax_api_request_seconds` | histogram | `route` | API request duration. |
| `wax_api_rate_limited_total` | counter | `tenant` | Requests refused by the tenant rate limit. |
| `wax_lease_events_total` | counter | `event` | `acquired`, `released`, `lost`, `fenced`. |
| `wax_db_connections` | gauge | `state` | Pool connections: `max`, `total`, `acquired`, `idle`. |
| `wax_goroutines` | gauge | | Goroutines of the process. |
| `wax_heap_bytes` | gauge | | Heap memory in use. |

Route labels use patterns, never ids, so their number is fixed.
Per-instance series grow with the number of accounts (a few series each).

Useful alerts: instances not `connected` for a long time,
`wax_webhook_deliveries_total{result="dead"}` increasing,
`wax_webhooks_pending` or `wax_outbox_pending` growing,
`wax_lease_events_total{event=~"lost|fenced"}` increasing, and
`wax_db_connections{state="acquired"}` close to `max`.

## Logging

`wax serve` writes JSON lines to stderr, one object per event, with
`time`, `level`, `msg` and context fields such as `instance`. Choose the
level with `-log-level` (`debug`, `info`, `warn`, `error`). Collect stderr
with your process manager or container runtime.

Errors from WhatsApp or the media servers are logged in full but returned
to API clients only as `upstream request failed`, since they can contain
protocol details. A panic in a request handler or an instance's background
loop is logged with its stack trace and does not take down the process or
other instances.

### Access log

Every API request is logged as `api request` with its route pattern
(`DELETE /v1/instances/{id}`), status, duration in milliseconds, the
client address and the caller: `admin` for the admin key, `key <id>` for a
tenant key (its public id, never the secret) with its `tenant`, plus the
instance or tenant named in the path (`path_id`, `path_tenant`,
`path_key`). Requests that change something (POST, PATCH, DELETE) are
logged at `info`; reads at `debug`, because the admin interface polls.
Paths themselves are not logged, since they can contain chat addresses. A
request forwarded to the node that runs the instance is logged on both
nodes, the second time with `forwarded`.

Deleting an account also logs `instance deleted` with the instance (its
internal id, `tenant:name`) and `unlinked`, saying whether the device was removed from the
phone first.

`wax run` logs as plain text instead, and can dump protocol nodes with
`-debug -dump-nodes` (masked by default).

## Database and migrations

wax needs PostgreSQL. Migrations are embedded in the binary and **applied
automatically on start**: each pending migration runs in its own
transaction, recorded in the `wax_migrations` table. An advisory lock
serializes concurrent starters, so several nodes can boot against the same
database at once. There is no separate migration command.

The database holds everything: device identities and Signal sessions,
tenants and key HMACs, chats and messages, the outbox, the webhook queue
and the leases. Private keys, Signal and sender key sessions, app state
keys, message contents, queued outbox and webhook payloads and webhook
secrets are sealed with AES-256-GCM under a key derived from
`WAX_MASTER_KEY`. Metadata such as chat ids, timestamps and message types
is stored in the clear.

Delivered and dead webhook events are purged automatically after 7 days.

## Backups and the master key

Back up the database with your usual PostgreSQL tooling (`pg_dump`,
continuous archiving, or managed snapshots). A backup restored with the
same `WAX_MASTER_KEY` brings back all accounts without re-pairing,
provided WhatsApp still accepts the sessions.

`WAX_MASTER_KEY` is the other half of every backup:

- Without it, the sealed data in the database cannot be decrypted. Every
  account would have to be linked again with a QR code, and every tenant
  key would have to be reissued (their HMACs are keyed from it).
- Store it **separately** from the database backups (a secrets manager, or
  an offline copy), so that a leaked backup alone does not expose the
  sessions, and a lost server does not take the key with it.
- All nodes of a cluster must use the same key.
- There is no tool to change the master key of an existing database. Treat
  it as permanent for the lifetime of the data.

Restoring a backup also restores the encryption session state of that
moment, which the contacts' devices have since moved past. Prefer recent
backups, and expect some `undecryptable_message` events after a restore
from an old one.

## Upgrading

1. Back up the database.
2. Get the new version: a program from the
   [releases page](https://github.com/mdenesfe/wax/releases) (with
   `checksums.txt` to verify it), the image `ghcr.io/mdenesfe/wax:<version>`,
   or a build of the tag (`docker compose build` after `git pull`).
   `wax version` prints the running version, and the server logs it at
   start (`wax` in the `api listening` line).
3. Stop the old process with SIGTERM and start the new one (with Docker
   Compose: `docker compose up -d`). Migrations run on start.

With several nodes, replace them one at a time. A node that stops
gracefully hands its instances to the others within seconds, and they
reconnect without QR codes. Since the first upgraded node applies the
migrations for everyone, do not leave old and new versions running side by
side for long.

Check after an upgrade that instances return to `connected`
(`GET /v1/instances` or `wax_instance_status`), and read the release notes
for anything that needs attention.

## Shutdown

On SIGINT or SIGTERM the node stops accepting requests, waits up to 20
seconds for running requests, stops its instances, releases their leases
and leaves the cluster. Queued messages and webhook events stay in the
database and continue on whichever node runs the instance next.
