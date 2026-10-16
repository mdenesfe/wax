# The WhatsApp Web multi-device protocol, as implemented by wax

WhatsApp does not publish the protocol its web and desktop clients speak.
These chapters describe it as wax implements it: what is sent, what comes
back, how it is encrypted and how state is kept, with links to the code
that does each part. They are written for people who want to understand the
protocol, implement it, or work on wax.

Two rules apply throughout:

- **Only what the code does.** Every statement is backed by the code it
  links to. Where wax does not implement something, or where the purpose of
  a field is not known, the text says so instead of guessing.
- **It can change.** WhatsApp changes the protocol without notice. A detail
  that is true today may not be tomorrow; the tests and the live behavior
  of the code are the reference.

## The stack

```
  application        messages, receipts, presence, groups, profiles, media
  -------------------------------------------------------------------------
  sync               app state (LTHash)          history sync
  -------------------------------------------------------------------------
  end-to-end         Signal: X3DH, Double Ratchet, sender keys
  -------------------------------------------------------------------------
  stanzas            binary XML nodes: iq, message, receipt, notification
  -------------------------------------------------------------------------
  transport          Noise XX over WebSocket frames
```

## A session, from start to finish

1. **Connect.** Open a WebSocket to WhatsApp, send the connection header
   and run a Noise `XX` handshake. The server proves its identity with a
   certificate chain; the client sends its `ClientPayload`.
   ([01-transport.md](01-transport.md))
2. **Speak in nodes.** From then on both sides exchange binary XML nodes
   (stanzas) inside encrypted frames: requests and answers (`iq`),
   messages, receipts, notifications and acknowledgements.
   ([02-binary-xml.md](02-binary-xml.md))
3. **Pair once.** A new device registers, shows QR codes for the phone to
   scan, verifies the signed `pair-success` and confirms it. Later
   connections log in with the stored identity. Prekeys are uploaded so
   others can start encrypted sessions. ([03-pairing.md](03-pairing.md))
4. **Encrypt end to end.** Messages are encrypted per device with Signal
   sessions (1:1) or a sender key (groups). ([04-signal.md](04-signal.md))
5. **Send and receive.** Find the recipient's devices, encrypt for each,
   send one stanza; on receipt decrypt, unwrap, store, acknowledge and send
   receipts, asking for a resend when decryption fails.
   ([05-messages.md](05-messages.md))
6. **Know who is who.** People have a phone number and a LID; devices,
   profiles, pictures and privacy tokens are looked up with user sync and
   other queries. ([06-users-and-lids.md](06-users-and-lids.md))
7. **Groups and communities.** Group metadata, members and addressing
   mode, and how communities link groups together.
   ([07-groups-and-communities.md](07-groups-and-communities.md))
8. **Stay in sync with the phone.** Contacts, chat settings and names come
   from app state, a set of encrypted, verifiable key-value collections;
   past conversations arrive as history sync blobs.
   ([08-app-state.md](08-app-state.md), [09-history-sync.md](09-history-sync.md))
9. **Media.** Files are encrypted with a per-file key, uploaded to
   WhatsApp's media servers and referenced from the message.
   ([10-media.md](10-media.md))

## Words used in these chapters

| Term | Meaning |
|------|---------|
| JID | An address: `user@server`, for example `905550000001@s.whatsapp.net` (a phone number), `1234@lid`, `120363000000000001@g.us` (a group). |
| PN, LID | A user's phone-number JID and its LID (a number that does not reveal the phone number). WhatsApp increasingly addresses people by LID. |
| Device JID (AD JID) | A JID with a device number, `905550000001:3@s.whatsapp.net`. Device 0 is the phone; linked devices have other numbers. |
| Primary, companion | The phone is the primary device; wax is a companion (linked) device. |
| Node, stanza | A binary XML element; a top-level node sent or received is a stanza. |
| IQ | A request/response stanza, matched by `id`. |
| Prekey | A one-time public key published so that others can start a Signal session without us being online. |
| Sender key | The key a device uses to encrypt group messages once for all members. |
| App state | Encrypted key-value collections shared by a user's devices (contacts, mutes, pins, archive, settings). |
| LTHash | A hash of a set that can be updated by adding and removing elements; used to verify app state. |
| Push name | The name a user chose for themselves, shown as `~name` when not in the address book. |
| Privacy token | A token a contact gives us so that its "my contacts" privacy settings treat us as a contact. |

## How this was written

wax was written from scratch. The protocol was learned from the behavior of
the live service (`cmd/wax-probe` logs every node of a session) and from
reading other open-source implementations as a reference (notably
whatsmeow, for field numbers and message names); no code was copied from
them. Cryptography uses only the Go standard library. Where the
code comments cite a reference, the chapters say so.

Corrections are welcome: if WhatsApp behaves differently from what is
written here, please open an issue with what you observed.
