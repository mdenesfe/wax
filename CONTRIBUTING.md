# Contributing to wax

Thank you for helping. wax is two things at once: an implementation of the
WhatsApp Web multi-device protocol and a written explanation of it. A good
change usually touches both: the code, its tests, and the chapter in
[docs/protocol](docs/protocol/README.md) that describes it.

## Setting up

You need Go (the version in `go.mod`), Docker, and for the admin interface
Node.js only if you take screenshots.

```bash
docker compose up -d postgres        # PostgreSQL on 127.0.0.1:5433 (also creates wax_test)
cp .env.example .env                 # fill in WAX_MASTER_KEY and WAX_API_KEY
set -a; source .env; set +a
go run ./cmd/wax serve               # the server, http://127.0.0.1:8080
go run ./cmd/wax-demo                # the admin interface with made-up data, :8090
```

Tests:

```bash
go test -race ./...                                    # database tests are skipped
WAX_TEST_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax_test?sslmode=disable go test -race ./...
```

The second form also runs the store tests and the end-to-end tests against
the in-process fake WhatsApp server (`internal/testutil/fakewa`). CI runs
`gofmt`, `go vet` and the full suite on every push.

## Rules of the project

- **No WhatsApp libraries, no copied code.** Other implementations may be
  read to understand the protocol, but everything here is written for wax.
- **Do not guess protocol details.** If you are not sure what a field or
  attribute means, say so in a comment and in the docs rather than
  inventing a meaning. Observed behavior and tests are the reference.
- **Cryptography from the Go standard library** (and `golang.org/x/crypto`
  if ever needed). New dependencies need a good reason: open an issue
  before adding one.
- **All state in PostgreSQL**, secrets sealed with the master key (see
  [docs/architecture.md](docs/architecture.md)). Schema changes are new
  numbered files in `internal/store/migrations`; never edit an applied one.
- **Tests for behavior**, doc comments on exported names, errors wrapped
  with `%w`, `gofmt` clean.
- **No real data.** Never commit real phone numbers, JIDs, keys, QR codes
  or message contents, in code, tests, logs or screenshots. Use the
  `90555...` test numbers. Screenshots come from `cmd/wax-demo`
  (`node scripts/screenshots.mjs`).
- **Keep the docs true.** When behavior changes, update the chapter that
  describes it, and the OpenAPI document (`api/openapi.json`) for API
  changes; a test checks the latter against the code.

## Protobuf schemas

The schemas in `proto/` are subsets of WhatsApp's, with field numbers
checked against the protocol. After editing a `.proto` file, regenerate
with `proto/generate.sh` (needs `protoc` and `protoc-gen-go`).

## Pull requests

Keep a pull request to one topic, describe what it changes and how you
tested it (for protocol changes: against the fake server, and whether you
tried it with a real account). Security problems are not reported in pull
requests or issues; see [SECURITY.md](SECURITY.md).

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
