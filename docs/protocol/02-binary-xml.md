# 02. Binary XML ("wabin")

After the Noise handshake (see [01-transport.md](01-transport.md)) every decrypted frame carries one node in WhatsApp's binary XML format. A node is an XML-like element (tag, attributes, and content that is either child nodes or raw bytes) serialized as a list of compact values: common strings become one- or two-byte dictionary tokens, digit and hex strings are packed two characters per byte, and JIDs have dedicated encodings. This document describes the format as implemented in `internal/wabin`, then the stanza kinds `internal/client` handles and how it acknowledges them.

## Frame payload

The decrypted frame starts with a one-byte flags field, followed by the encoded node:

```
+-------+------------------------------+
| flags | node (possibly zlib)         |
+-------+------------------------------+
```

| Flag bit | Meaning |
|---|---|
| `0x02` | the rest of the frame is a zlib stream (`flagCompressed`) |

`wabin.Unpack()` strips the flags byte and inflates the payload if bit `0x02` is set, capped at `MaxDecompressedSize` (64 MiB) as zip-bomb protection. Other flag bits are ignored. `wabin.Marshal()` always writes flags `0x00`: wax never compresses outgoing stanzas. `wabin.Unmarshal()` then decodes exactly one node and reports `ErrTrailingBytes` if anything is left over.

## Node model

```go
type Node struct {
    Tag     string
    Attrs   Attrs // map[string]any
    Content any   // nil, []Node, []byte, string (or, rarely, JID)
}
```

(`internal/wabin/node.go`). Decoded attribute values are `string` or `JID`. When encoding, integers and bools are also accepted and written as their decimal or textual form, and attributes whose value is `nil` or `""` are omitted entirely (the protocol has no empty attribute).

Throughout these docs nodes are shown in the form printed by `Node.Format()` (`internal/wabin/format.go`): attributes sorted by key, binary content shown as UTF-8 text or `0x...` hex, and in masked mode JID users reduced to their last four characters and binary payloads replaced by length plus a hash prefix.

```
<iq id="3fa1-17" to="s.whatsapp.net" type="get" xmlns="w:p">
  <ping/>
</iq>
```

## Wire values

Every value starts with one tag byte (`internal/wabin/tags.go`):

| Byte | Name | Followed by |
|---|---|---|
| `0x00` | `LIST_EMPTY` | nothing (empty list, nil value, or absent JID user) |
| `0x01`..`0xEB` | single-byte token | nothing; value is `singleByteTokens[b]` |
| `0xEC`..`0xEF` | `DICTIONARY_0`..`3` | 1 index byte into `doubleByteTokens[b - 0xEC]` |
| `0xF0`..`0xF4` | unassigned | rejected by the decoder (`ErrInvalidToken`) |
| `0xF5` | `INTEROP_JID` | user, device (u16), integrator (u16), server |
| `0xF6` | `FB_JID` | user, device (u16), server |
| `0xF7` | `AD_JID` | agent (u8), device (u8), user |
| `0xF8` | `LIST_8` | size (u8), then items |
| `0xF9` | `LIST_16` | size (u16 big-endian), then items |
| `0xFA` | `JID_PAIR` | user (or `LIST_EMPTY`), server |
| `0xFB` | `HEX_8` | packed length byte, packed bytes |
| `0xFC` | `BINARY_8` | length (u8), bytes |
| `0xFD` | `BINARY_20` | length (3 bytes, top 4 bits zero), bytes |
| `0xFE` | `BINARY_32` | length (u32 big-endian), bytes |
| `0xFF` | `NIBBLE_8` | packed length byte, packed bytes |

All multi-byte integers are big-endian.

## Node encoding

A node is a list whose items are: the tag, then each attribute as a key/value pair, then optionally the content.

```
LIST_8 size | tag | key1 val1 | key2 val2 | ... | [content]

size = 1 + 2 * attribute_count + (1 if content present)
```

The decoder derives structure from the size alone (`decoder.readNode()`): `(size - 1) / 2` attributes, and content exists if and only if `size` is even. A size of 0 is invalid. The tag must decode to a non-empty string and attribute keys must be strings (`ErrNonStringKey`).

`encoder.writeListStart()` uses `LIST_EMPTY` for 0, `LIST_8` below 256 and `LIST_16` below 65536; larger lists fail with `ErrTooLarge`.

Content is one of:

- **child nodes**: a list (`LIST_8`/`LIST_16`) whose items are nodes, decoded as `[]Node`;
- **binary**: `BINARY_8/20/32`, decoded as `[]byte`;
- **a token or packed string**: decoded as `string` (`Node.Bytes()` accepts this too, since the server may tokenize short values);
- **a JID**: rare.

Attribute values and JID parts that arrive as `BINARY_*` are decoded as `string`; content arriving as `BINARY_*` is decoded as `[]byte` (the `asString` parameter of `decoder.readValue()`).

Attribute order on the wire follows Go map iteration and is therefore not stable between encodings. The decoder does not depend on order.

Nesting is limited to `maxDepth` (64) on both sides, including chains of nested JID tags, so malformed input cannot exhaust the stack (`ErrTooDeep`). The decoder copies all byte slices, so the input buffer may be reused.

## Token dictionaries

`internal/wabin/tokens.go` holds two generated tables for dictionary version 3 (`DictVersion`, also sent as the fourth byte of the connection header):

- `singleByteTokens`: 236 entries. Index 0 is unused (it collides with `LIST_EMPTY`), so byte values `0x01`..`0xEB` map to strings such as `0x03 = s.whatsapp.net`, `0x19 = iq`, `0x04 = type`, `0x29 = get`. An `init()` check panics if the table would overlap the structural tags.
- `doubleByteTokens`: four dictionaries of 256 entries each, addressed as `DICTIONARY_n index`. For example `active` is `EC 01` and `pair-device` is `ED EE`.

The order of entries defines the wire values, so the tables must match WhatsApp Web exactly and are never edited by hand. If WhatsApp bumps the dictionary version, the tables and `DictVersion` must be regenerated together.

## String encoding

`encoder.writeString()` picks the most compact form, in this order:

1. `""` is `BINARY_8` with length 0: `FC 00`.
2. A single-byte token.
3. A double-byte token.
4. `NIBBLE_8` if every character is `0-9`, `-` or `.` and the length is at most 127.
5. `HEX_8` if every character is `0-9` or `A-F` (uppercase only) and the length is at most 127.
6. Raw bytes: `BINARY_8` (< 256 bytes), `BINARY_20` (< 2^20), else `BINARY_32`.

Because digits match both packed forms, an all-digit string always uses `NIBBLE_8`.

### Packed strings

Each byte holds two 4-bit codes, high nibble first. The length byte holds the number of packed bytes, `ceil(len / 2)`, in its low 7 bits; the top bit is set when the string length is odd, in which case the final low nibble is the padding value `0xF` and is dropped by the decoder.

| Code | `NIBBLE_8` | `HEX_8` |
|---|---|---|
| 0-9 | `0`..`9` | `0`..`9` |
| 10 | `-` | `A` |
| 11 | `.` | `B` |
| 12-14 | invalid | `C`..`E` |
| 15 | padding | `F` |

Examples (the first is `TestPackedEncoding`):

```
"123"       -> FF 82 12 3F           NIBBLE_8, 2 bytes | odd, "12", "3"+pad
"0123-1"    -> FF 03 01 23 A1        '-' is nibble 0xA
"DEADBEEF"  -> FB 04 DE AD BE EF     HEX_8, 4 bytes, even
```

Note that stanza ids from `Conn.NextID()` contain lowercase hex (for example `3fa1-17`), which matches neither packed alphabet, so they are sent as `BINARY_8`. Only an id whose prefix happens to be all digits gets nibble-packed.

## JIDs

A JID is `user[.agent][:device]@server` (`internal/wabin/jid.go`). `JID` has the fields `User`, `RawAgent`, `Device`, `Integrator` and `Server`. Known servers include `s.whatsapp.net` (phone-number users, also the server's own JID with an empty user), `lid`, `g.us`, `broadcast`, `newsletter`, `msgr`, `interop`, `hosted`, `hosted.lid`, `bot` and the legacy `c.us`.

`encoder.writeJID()` chooses the wire form:

| JID | Wire form |
|---|---|
| `s.whatsapp.net` or `lid` with `Device > 0` | `AD_JID agent device user` |
| any `hosted` or `hosted.lid` JID (device may be 0) | `AD_JID agent device user` |
| server `msgr` | `FB_JID user device(u16) server` |
| server `interop` | `INTEROP_JID user device(u16) integrator(u16) server` |
| everything else | `JID_PAIR user server` (`user` is `LIST_EMPTY` if empty) |

In `AD_JID` the server is implied by the agent byte (`JID.agent()`, `newADJID()`):

| Agent | Server |
|---|---|
| 0 | `s.whatsapp.net` |
| 1 | `lid` |
| 128 | `hosted` |
| 129 | `hosted.lid` |
| other | `s.whatsapp.net`, value kept in `RawAgent` and printed as `user.agent` |

`AD_JID` stores the device in one byte, so encoding a device above 255 fails with `ErrInvalidJID`. A `s.whatsapp.net` JID with a `RawAgent` but device 0 is written as `JID_PAIR`, which does not carry the agent. When parsing text, `ParseJID()` only recognizes the `.agent` suffix for `s.whatsapp.net`.

Encoded examples (bytes follow from the rules above):

```
s.whatsapp.net                     FA 00 03
1234@lid                           FA FF 02 12 34 76          (lid = token 0x76)
1234:3@lid                         F7 01 03 FF 02 12 34
905550000001:12@s.whatsapp.net     F7 00 0C FF 06 90 55 50 00 00 01
```

## Worked example

The keepalive ping, with attributes in the order `id`, `to`, `type`, `xmlns` (the actual order varies, see above) and the id `0123-1`:

```
00                 flags: not compressed
F8 0A              LIST_8, 10 items = tag + 4 pairs + content
19                 "iq"
08 FF 03 01 23 A1  "id"    = "0123-1" (NIBBLE_8)
11 FA 00 03        "to"    = JID_PAIR(no user, "s.whatsapp.net")
04 29              "type"  = "get"
16 57              "xmlns" = "w:p"
F8 01              content: list of 1 node
  F8 01 56         <ping/> (LIST_8, 1 item, token "ping")
```

The smallest examples from `TestTokensAreUsed`:

```
<iq type="get"/>   ->  00 F8 03 19 04 29
<pair-device/>     ->  00 F8 01 ED EE
```

## Stanzas handled by wax

`session.handleNode()` in `internal/client/handlers.go` dispatches on the tag of each top-level node that is not a pending response. Unknown tags are logged at debug level and ignored; they never end the session.

| Tag | Handling |
|---|---|
| `stream:error`, `failure` | classified by `ClassifyDisconnect()`, ends the session (see 01-transport) |
| `xmlstreamend` | ends the session, reconnect |
| `success` | login complete; post-login setup (prekeys, `<iq xmlns="passive"><active/></iq>`, app state sync) |
| `iq` | requests from the server; results/errors are matched to waiters (below) |
| `message` | decrypted and acknowledged by receipt (`handleMessage()`, `internal/client/message.go`) |
| `receipt` | delivery/read/retry receipts for our messages, then acked (`internal/client/retry.go`) |
| `notification` | dispatched on `type`, mostly acked (below) |
| `ib` | `offline_preview` and `offline` children are logged; nothing is sent |
| `call` | turned into a `CallEvent` and acked (`internal/client/call.go`) |
| `presence` | `Presence` event (`type="unavailable"`, optional `last`); no ack is sent |
| `chatstate` | `ChatPresence` event from the child tag (`composing`, `paused`) and its `media`; no ack is sent |

### IQ requests and responses

An IQ is `<iq id type xmlns to>` with `type` one of `get`, `set`, `result`, `error`. `session.sendIQ()` (`internal/client/session.go`) assigns an id with `Conn.NextID()` if none is set, registers a waiter under that id, sends, and waits (default 30 s, `defaultIQTimeout`).

`readLoop()` calls `deliverResponse()` on every node before queueing it. An `iq` with `type="result"` or `type="error"`, or any `ack`, whose `id` has a registered waiter goes straight to that waiter and never reaches the handler queue. This matters because handlers may themselves block on IQs; only the reader can deliver their responses. A response with no waiter falls through to `handleNode()`.

An error response is returned as `*IQError` with `code` and `text` taken from its `<error>` child:

```
<iq from="s.whatsapp.net" id="3fa1-18" type="error">
  <error code="404" text="item-not-found"/>
</iq>
```

When the session ends, all waiters are released with `ErrNotConnected`.

Server requests handled in `handleIQ()`: `get` with `xmlns="urn:xmpp:ping"` (answered with an empty result), and `set` with a `pair-device` or `pair-success` child (QR pairing, documented separately). Other server IQs are logged and not answered.

### Acks

The server keeps stanzas in an offline queue until they are acknowledged and delivers that queue in windows, so wax acknowledges everything it receives, including stanzas it cannot process yet. `session.nack()` builds the ack:

```
<ack class="<tag>" id="<id>" to="<from>" [participant] [recipient] [type] [error]/>
```

- `class` is the tag of the stanza being acknowledged.
- `id` and `to` are copied from the stanza's `id` and `from`.
- `participant` and `recipient` are copied if present.
- `type` is copied if present, except for `message`.
- `error` is set for a negative ack. Codes used for messages: 487 (parse error), 488 (unrecognized stanza, for example no `<enc>`), 491 (invalid protobuf).

Example for a picture notification:

```
<notification from="1234@lid" id="1700000001" t="1700000000" type="picture"/>
<ack class="notification" id="1700000001" to="1234@lid" type="picture"/>
```

Receipts are acked by `ackReceipt()` with `class="receipt"`, `id`, `to`, and `type` and `participant` copied.

Messages are normally confirmed with a `<receipt>` rather than an `<ack>` (`sendMessageReceipt()`); the receipt doubles as the stanza ack. It carries `id`, `to` (the message's `from`), copied `recipient`/`participant`, and `type="sender"` (or `peer_msg`) for our own messages from other devices, or `type="inactive"` when we have not announced ourselves available. The message is acknowledged only after it has been decrypted, stored and delivered as an event, so a crash before that leads to redelivery.

Stanzas wax cannot process yet (unimplemented notification types, newsletter and bot messages, unsupported `enc` types) are serialized with `wabin.Marshal()`, stored, and acked only after the write succeeded (`deferStanza()`). They are replayed after the next `<success>`; replayed stanzas are not acked again.

### Notifications

`handleNotification()` dispatches on `type`:

| `type` | Handling |
|---|---|
| `w:gp2` | group change |
| `server_sync` | app state collections changed; ack, then sync each `<collection name>` |
| `account_sync` | `AccountSync` event, device cache invalidated on `<devices>`, ack |
| `devices` | device list cache for `from` invalidated, ack |
| `encrypt` from the server with `<count>` | prekey count low, ack, upload prekeys |
| `encrypt` from a user with `<identity>` | drop that user's Signal sessions, ack (left unacked if the delete fails) |
| `privacy_token` | privacy tokens |
| `picture`, `status` | profile caches invalidated, ack |
| `disappearing_mode`, `mex` | acked, otherwise ignored |
| anything else | deferred and acked |

## In the code

| Concept | File | Symbols |
|---|---|---|
| Node, attributes, accessors | `internal/wabin/node.go` | `Node`, `Attrs`, `AttrString()`, `AttrJID()` |
| Wire tag values | `internal/wabin/tags.go` | `tagList8`, `tagADJID`, ... |
| Token tables | `internal/wabin/tokens.go` | `singleByteTokens`, `doubleByteTokens`, `DictVersion` |
| Encoder | `internal/wabin/encoder.go` | `Marshal()`, `writeString()`, `writeJID()`, `writePacked()` |
| Decoder, flags, zlib | `internal/wabin/decoder.go` | `Unpack()`, `Unmarshal()`, `readNode()`, `readValue()` |
| JIDs | `internal/wabin/jid.go` | `JID`, `ParseJID()`, `newADJID()` |
| Log formatting, masking | `internal/wabin/format.go` | `Node.Format()`, `MaskJID()` |
| Tests with byte examples | `internal/wabin/binary_test.go` | `TestTokensAreUsed`, `TestPackedEncoding` |
| Node send/receive | `internal/client/conn.go` | `SendNode()`, `ReadNode()`, `NextID()` |
| IQ matching | `internal/client/session.go` | `sendIQ()`, `deliverResponse()` |
| Dispatch, acks, notifications | `internal/client/handlers.go` | `handleNode()`, `nack()`, `deferStanza()` |
| Messages, receipts, calls, presence | `internal/client/message.go`, `retry.go`, `call.go`, `presence.go` | `handleMessage()`, `handleReceipt()`, `handleCall()`, `handlePresence()` |
