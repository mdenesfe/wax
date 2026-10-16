# wax documentation

## Protocol

How the WhatsApp Web multi-device protocol works, as implemented by wax.
Start with the [overview](protocol/README.md).

1. [Transport: WebSocket, Noise and the connection lifecycle](protocol/01-transport.md)
2. [Binary XML nodes and stanzas](protocol/02-binary-xml.md)
3. [Pairing a companion device](protocol/03-pairing.md)
4. [Signal: end-to-end encryption](protocol/04-signal.md)
5. [Sending and receiving messages](protocol/05-messages.md)
6. [Users, LIDs, profiles and privacy tokens](protocol/06-users-and-lids.md)
7. [Groups and communities](protocol/07-groups-and-communities.md)
8. [App state](protocol/08-app-state.md)
9. [History sync](protocol/09-history-sync.md)
10. [Media](protocol/10-media.md)

## Using wax

- [Getting started](usage/getting-started.md): run the server, link an account, send a message
- [REST API](usage/api.md): every endpoint with examples (the full contract is `GET /openapi.json`)
- [Webhooks](usage/webhooks.md): events, signatures and retries
- [Operations](usage/operations.md): flags, tenants, several nodes, metrics, backups
- [Admin interface](usage/admin-ui.md): the web interface, and a demo with made-up data

## Inside wax

- [Architecture](architecture.md): packages, sessions, storage and sealing, tenants, nodes, the security model
