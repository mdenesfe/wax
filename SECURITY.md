# Security policy

wax holds the keys of WhatsApp accounts and the messages they exchange, so
security problems matter. Please report them privately.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: on the repository's
**Security** tab, choose **Report a vulnerability**. Please do not open a
public issue or pull request for a security problem.

Include what you found, how to reproduce it, and what an attacker could do
with it. You will get an answer as soon as possible; once a fix is
released, the advisory is published with credit to you, unless you prefer
otherwise.

## Scope

In scope, for example:

- reading or changing another tenant's accounts or data through the API;
- getting at keys, sessions or messages without the master key or an API
  key;
- forging webhook signatures or forwarded requests between nodes;
- code execution, injection, or bypassing the admin interface's
  protections (Content Security Policy, text-only rendering);
- weaknesses in the cryptography as wax implements it.

Out of scope: WhatsApp's own servers and apps, an account being restricted
or banned by WhatsApp, and attacks that need the master key, an admin key,
or access to the database or the host.

## Supported versions

Fixes are made on the latest release. wax is young (before 1.0); please
upgrade to the latest release before reporting.

## Running wax safely

- Keep `WAX_MASTER_KEY`, `WAX_API_KEY` and the database credentials secret,
  and keep the master key outside database backups.
- Put TLS in front of wax when it is reachable beyond localhost, and keep
  the metrics address and the node-to-node network private.
- See [docs/architecture.md](docs/architecture.md#security-model) for the
  security model.
