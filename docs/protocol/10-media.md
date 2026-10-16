# 10. Media

WhatsApp never sends media bytes through the messaging socket. The sender
encrypts the file with a fresh random 32-byte **media key**, uploads the
ciphertext over HTTPS to a WhatsApp media server, and then sends an
end-to-end encrypted message that carries the media key, two SHA-256 hashes,
the length and a **direct path** to the blob. The receiver downloads the blob
from a media host, verifies the hashes and a truncated HMAC, and decrypts it.
Access to the media servers is granted by a short-lived token obtained with
the `media_conn` IQ. wax implements the cryptography in
[`internal/media/media.go`](../../internal/media/media.go), the HTTP transfer in
[`internal/media/transfer.go`](../../internal/media/transfer.go), the
client-level glue (token cache, upload, download, building media messages) in
[`internal/client/mediaops.go`](../../internal/client/mediaops.go), and a
WebM to Ogg Opus rewrapper for browser-recorded voice notes in
[`internal/media/webm.go`](../../internal/media/webm.go).

## Media types

Every kind of blob has a **type**, which selects two things: the HKDF `info`
string used to expand the media key, and the path segment used on the media
servers (the "MMS type").

| `media.Type` constant | HKDF info string          | MMS type (path segment) |
| --------------------- | ------------------------- | ----------------------- |
| `Image`               | `WhatsApp Image Keys`     | `image`                 |
| `Video`               | `WhatsApp Video Keys`     | `video`                 |
| `Audio`               | `WhatsApp Audio Keys`     | `audio`                 |
| `Document`            | `WhatsApp Document Keys`  | `document`              |
| `Sticker`             | `WhatsApp Image Keys`     | `image` (see below)     |
| `History`             | `WhatsApp History Keys`   | `md-msg-hist`           |
| `AppState`            | `WhatsApp App State Keys` | `md-app-state`          |

`media.Type` is a string type whose value *is* the info string. Stickers use
the image keys, so `Sticker` and `Image` are the same value and also share the
`image` MMS type. wax only downloads stickers; it has no API to send them.
Voice notes are `Audio` (the "voice note" property lives in the message, not in
the encryption).

## Key expansion

`DeriveKeys` expands the media key with HKDF-SHA256 into 112 bytes:

```
HKDF-SHA256(ikm = mediaKey[32], salt = none (32 zero bytes), info = <type string>, L = 112)

 0        16                    48                    80                   112
 +--------+---------------------+---------------------+--------------------+
 |  IV    |     cipherKey       |       macKey        |      refKey        |
 | 16 B   |       32 B          |        32 B         |       32 B         |
 +--------+---------------------+---------------------+--------------------+
```

A media key that is not exactly 32 bytes is rejected. The **ref key** is
derived and returned in `media.Keys` but wax does not use it anywhere.

## Encryption format

`Encrypt` produces the blob that is uploaded:

```
ciphertext = AES-256-CBC(cipherKey, IV, PKCS#7-pad(plaintext))
mac        = HMAC-SHA256(macKey, IV || ciphertext)[0:10]
blob       = ciphertext || mac
```

The MAC is truncated to its first 10 bytes. Alongside the blob, `Encrypt`
returns the metadata that the message must carry:

| Field           | Value                                  |
| --------------- | -------------------------------------- |
| `MediaKey`      | the 32 random bytes (`crypto/rand`)    |
| `FileSHA256`    | SHA-256 of the plaintext               |
| `FileEncSHA256` | SHA-256 of the whole blob (ct and mac) |
| `FileLength`    | length of the plaintext in bytes       |

A new media key is generated for every upload; wax never reuses one.

### Verification order on decrypt

`Decrypt(blob, mediaKey, type, fileEncSHA256, fileSHA256)` checks in this
order and stops at the first failure:

1. If `fileEncSHA256` is present: SHA-256 of the blob must match
   (constant-time compare), else `ErrHashMismatch`.
2. Length: the blob must be at least 10 + 16 bytes and `len(blob) - 10` must
   be a multiple of 16, else `ErrInvalidLength`.
3. Derive keys; recompute the truncated HMAC over `IV || ciphertext` and
   compare, else `ErrInvalidMAC`.
4. AES-CBC decrypt and strip PKCS#7 padding; a padding error is reported as
   `ErrInvalidLength`.
5. If `fileSHA256` is present: SHA-256 of the plaintext must match, else
   `ErrHashMismatch`.

Both hashes are optional on input: an empty slice skips that check. The MAC
check is always done.

## Media connection (`media_conn`)

Uploads and downloads need an auth token and a list of hosts. The client asks
for them with an IQ ([`Client.mediaConn`](../../internal/client/mediaops.go)):

```xml
<iq to="s.whatsapp.net" type="set" xmlns="w:m" id="...">
  <media_conn/>
</iq>
```

The answer is read as follows (other attributes and children are ignored):

```xml
<iq type="result" ...>
  <media_conn auth="<token>" ttl="<seconds>">
    <host hostname="<media-host-1>"/>
    <host hostname="<media-host-2>"/>
  </media_conn>
</iq>
```

- `auth` becomes `media.Conn.Auth`.
- `ttl` (seconds) defaults to 300 when missing or not positive.
- Every `<host>` child with a non-empty `hostname` is appended to
  `media.Conn.Hosts`, keeping server order.
- A response with no `media_conn` child, no `auth`, or no host is an error.

### Caching and refresh

The result is cached per client under a mutex (`mediaState`). It expires
`ttl - 30s` after it was fetched, so an upload never starts on a token that is
about to expire. Callers get the cached value until then; the mutex also means
concurrent callers share one IQ instead of each sending their own.

A refresh is forced (bypassing the cache) in one case: an upload answered with
HTTP 401 or 403. `Client.Upload` then fetches a new `media_conn` and retries the
upload once. Downloads do not force a refresh.

## Upload

`Transfer.Upload` sends the blob in one `POST` to the **first** host of the
`media_conn` list. It does not try other hosts.

```
POST https://<hosts[0]>/mms/<mms-type>/<token>?auth=<auth>&token=<token>
Origin:  https://web.whatsapp.com
Referer: https://web.whatsapp.com/
Content-Length: <len(blob)>

<ciphertext || mac>
```

`<token>` is `base64url(fileEncSHA256)` with padding (Go's
`base64.URLEncoding`), used both as the last path segment and as the `token`
query parameter. The query string is built with `url.Values`, so the auth
token is URL-escaped. A type without an MMS path is rejected before any request.

The response must be HTTP 200 with a JSON body (read up to 1 MiB):

```json
{ "url": "https://<media-host>/<path>?<params>", "direct_path": "/<path>?<params>", "handle": "..." }
```

`direct_path` is required; a response without it is an error. `url` and
`direct_path` are stored as returned. `handle` is decoded but not used.

**Retries.** The media layer itself does not retry uploads. The only retry is
the single refresh-and-retry on 401/403 described above. Any other failure is
returned to the caller; when sending through the REST API, the message stays in
the outbox, which has its own retry policy.

## Download

wax always downloads by **direct path** and never fetches the `url` field of a
message. The URL is rebuilt on each media host:

```
GET <scheme>://<host><directPath>?hash=<b64url(fileEncSHA256)>&mms-type=<mms-type>&__wa-mms=
```

If `directPath` already contains a `?`, the extra parameters are appended with
`&` instead. A direct path that does not start with `/` is rejected. The same
`Origin` and `Referer` headers as for uploads are sent. Note that the `auth`
token is **not** added to download requests.

### Host selection and retries

```
for host in media_conn.hosts (server order):
    for attempt in 1..3:
        GET -> 200: return body
        403 / 404 / 410: return error immediately (no other host)
        retryable error: wait backoff(attempt), try again
        other error: move to next host
return "download failed on all hosts" (wrapping the last error)
```

- **Retryable** errors are network errors (`net.Error`), an unexpected EOF,
  HTTP 429 and HTTP 5xx.
- The wait uses the shared [`backoff`](../../internal/backoff) policy
  `DefaultRetry` (Min 1 s, Max 4 s): a jittered delay in
  `[Min/2, Min * 2^(attempt-1)]`, capped at Max. The wait also happens after
  the third attempt, before moving to the next host.
- **403, 404 and 410** mean the file is gone or the path has expired; wax
  assumes another host will not help and returns `*media.HTTPError` at once.
- Context cancellation stops the loop.

### Size limit

A download is read fully into memory, bounded by `Transfer.MaxDownload`
(default `DefaultMaxDownload`, 100 MiB). A body larger than the limit is an
error. The client uses the default limit.

### Error handling at the client level

`Client.Download` unwraps container messages (ephemeral, view once, document
with caption) and picks the first of image, video, audio, document or sticker.
It returns `client.ErrNoMedia` when:

- the message is none of those types, or
- the direct path or the media key is empty.

An HTTP 404 or 410 from the media server is **not** translated into
`ErrNoMedia`; it is returned as a wrapped `*media.HTTPError`. The REST
endpoint that downloads a stored message's media maps `ErrNoMedia` to
404 and any other failure, including expired media, to 502 ("upstream request
failed"). The downloaded bytes are served with `X-Content-Type-Options:
nosniff` and `Content-Disposition: attachment`.

### Keeping tokens out of logs

Go's HTTP client includes the request URL in its errors. Upload URLs contain
the `media_conn` auth token, so `withoutURL` strips the `*url.Error` wrapper
and keeps only the operation and the underlying error. These errors can reach
logs, the outbox API and webhooks.

## Sending a media message

`Client.SendMedia` encrypts, uploads and then builds one of the media message
types. The kind comes from `SendMediaRequest.Kind`:

| Kind       | Media type | Message field     |
| ---------- | ---------- | ----------------- |
| `image`    | Image      | `imageMessage`    |
| `video`    | Video      | `videoMessage`    |
| `audio`    | Audio      | `audioMessage`    |
| `voice`    | Audio      | `audioMessage` with `PTT = true` |
| `document` | Document   | `documentMessage` |

The REST API picks the kind from the MIME type when none is given
(`KindForMIME`: `image/*`, `video/*`, `audio/*`, anything else is a document)
and sniffs the MIME type with `http.DetectContentType` when none is given.

Fields wax fills:

| Field               | image | video | audio / voice | document | Source |
| ------------------- | :---: | :---: | :-----------: | :------: | ------ |
| `url`               | x | x | x | x | upload response |
| `directPath`        | x | x | x | x | upload response |
| `mediaKey`          | x | x | x | x | `Encrypt` |
| `fileSha256`        | x | x | x | x | `Encrypt` |
| `fileEncSha256`     | x | x | x | x | `Encrypt` |
| `fileLength`        | x | x | x | x | plaintext length |
| `mimetype`          | x | x | x | x | request |
| `mediaKeyTimestamp` | x | x | x | x | current Unix time (s) |
| `caption`           | opt | opt | | opt | request, omitted if empty |
| `fileName`, `title` | | | | x | request, default `file` |
| `PTT`               | | | x | | `true` for voice, `false` for audio |
| `jpegThumbnail`     | opt | opt | | | request |
| `width`, `height`   | opt | opt | | | request, omitted if 0 |
| `seconds`           | | opt | opt | | request, omitted if 0 |
| `waveform`          | | | voice only | | request |

wax does not generate previews: no thumbnails, dimensions, durations or
waveforms are computed from the file, except the duration of a rewrapped WebM
voice note (next section). The REST API bounds a thumbnail to 100 KiB and a
waveform to 256 bytes; the documented waveform shape is 64 values in 0..100,
but wax does not check that. Reply and mention context is attached with
`SetContext` after the previews.

## Voice notes: WebM to Ogg Opus

WhatsApp expects voice notes as **Ogg Opus**. Browsers' `MediaRecorder`
usually produces **WebM with Opus**. Both carry the same Opus packets, so
`media.WebMToOgg` moves the packets from one container to the other without
decoding or re-encoding. The REST API calls it when the kind is `voice` and the
MIME type starts with `audio/webm`; the result is sent as
`audio/ogg; codecs=opus`, and its duration fills `seconds` if the caller did
not give one.

```
WebM (EBML)                                   Ogg Opus
-----------                                   --------
Tracks/TrackEntry/CodecID = "A_OPUS"   --->   page 0 (BOS): OpusHead
Tracks/TrackEntry/CodecPrivate         --->   page 1:       OpusTags ("wax")
Tracks/TrackEntry/Audio/Channels              page 2..n:    Opus packets,
Cluster/SimpleBlock  (frames)          --->                 granule = samples so far
Cluster/BlockGroup/Block                      last page (EOS)
```

### EBML walk

`readWebM` checks the EBML magic `1A 45 DF A3` and then walks the file element
by element. Each element is an ID (variable-length integer, length marker
kept) followed by a size (variable-length integer, marker removed). The length
of a vint is the number of leading zero bits of its first byte plus one; a
first byte of zero is invalid and ends the walk.

- **Container elements** (Segment, Cluster, Tracks, TrackEntry, Audio,
  BlockGroup) are *entered*: the walk continues with their first child and
  ignores their size. This is why live recordings with "unknown size" segments
  and clusters work.
- **Leaf elements** are read: `CodecID` (0x86), `CodecPrivate` (0x63A2,
  expected to hold the `OpusHead`), `Channels` (0x9F, accepted when 1..8,
  default 1), `SimpleBlock` (0xA3) and `Block` (0xA1). Others are skipped by
  size.
- A leaf whose size runs past the end of the data ends the walk without error,
  so a recording cut off mid-block keeps everything before it.

For a block, the track number (vint), the 16-bit timecode and the flags byte
are skipped and the rest is the Opus frame. Laced blocks (flags bits 1-2 not
zero) are rejected. The track number is not checked, so only files with a
single (Opus) track are supported. The walk fails if the codec is not `A_OPUS`
or no packet was found.

### Opus packet durations (RFC 6716, section 3.1)

The Ogg granule position counts 48 kHz samples, so wax needs each packet's
duration. `opusSamples` reads the TOC byte:

```
TOC byte:  | config (5 bits) | s (1 bit) | c (2 bits) |
```

| config  | Mode   | Frame sizes (by `config & 3`, hybrid by `config & 1`) |
| ------- | ------ | ----------------------------------------------------- |
| 0..11   | SILK   | 10, 20, 40, 60 ms -> 480, 960, 1920, 2880 samples     |
| 12..15  | Hybrid | 10, 20 ms -> 480, 960 samples                         |
| 16..31  | CELT   | 2.5, 5, 10, 20 ms -> 120, 240, 480, 960 samples       |

The frame count comes from `c`: 0 means one frame, 1 and 2 mean two frames,
3 means the count is in the low 6 bits of the second byte. Samples are frames
times samples per frame. An empty packet, or a code 3 packet with no second
byte, counts as zero.

### OpusHead and OpusTags (RFC 7845)

If `CodecPrivate` starts with `OpusHead` and is at least 19 bytes, it is used
unchanged. Otherwise wax writes a minimal identification header: version 1,
the channel count, a pre-skip of 312 samples (a common libopus encoder delay;
this is an assumption, not read from the file), input rate 48000, output gain
0 and mapping family 0. The pre-skip is then read back from bytes 10-11 of
whichever header is used.

The comment header is fixed: `OpusTags`, vendor string `wax`, zero user
comments.

### Ogg paging

`oggWriter.page` writes one page holding whole packets:

```
offset  size  field
0       4     "OggS"
4       1     version = 0
5       1     header type flags (0x02 BOS, 0x04 EOS)
6       8     granule position (little-endian)
14      4     stream serial = 0x77617821 ("wax!")
18      4     page sequence number (0, 1, 2, ...)
22      4     CRC-32 (computed last, over the whole page with this field zero)
26      1     number of segments
27      n     lacing values: each packet is 255, 255, ..., remainder (< 255)
27+n    ...   packet data
```

- Page 0 holds only `OpusHead` with BOS and granule 0; page 1 holds only
  `OpusTags` with granule 0.
- Audio packets are batched: a page is closed when it reaches 200 lacing
  segments or 50 packets, or at the last packet, which also sets EOS. The
  granule of each page is the running total of samples after its last packet
  (the granule includes the pre-skip samples, as RFC 7845
  requires).
- Packets are never split across pages. The code assumes each packet is well
  under 64 KiB, which holds for Opus.
- The **CRC** is Ogg's CRC-32: polynomial `0x04C11DB7`, processed MSB first,
  no reflection, initial value 0 and no final XOR. It is table-driven
  (`oggTable`).

### Duration

```
seconds = round((finalGranule - preSkip) / 48000)   // implemented as (x + 24000) / 48000
```

It is 0 when the total granule does not exceed the pre-skip.

## Other blobs on the same path

`Client.DownloadMedia(directPath, mediaKey, fileEncSHA256, fileSHA256, type)`
is the shared entry point; message media goes through it, and so do:

- **History sync chunks** that are not inline: downloaded with type `History`,
  then zlib-inflated and decoded. See
  [09-history-sync.md](09-history-sync.md).
- **App state external blobs** (`ExternalBlobReference`), with type
  `AppState`.

**Profile pictures do not use this path.** They are not end-to-end
encrypted. A `w:profile:picture` IQ returns a URL, which wax accepts only if it
is HTTPS on `whatsapp.net` or a subdomain, and fetches with a plain `GET`
(limit 2 MiB, HTTP 404 means no picture). See
[06-users-and-lids.md](06-users-and-lids.md).

## Not implemented

- Sending stickers, GIFs, view once media, or forwarding by re-uploading.
- Generating thumbnails, dimensions, durations or waveforms (only the WebM
  rewrap computes a duration).
- Streaming or ranged transfers: whole files are held in memory.
- Using the ref key, the `handle` from the upload response, or the message
  `url` for downloads.
- Upload failover to other hosts, and re-fetching `media_conn` after a
  download 401/403.

## In the code

| What | Where |
| ---- | ----- |
| Types, info strings, MMS types | `internal/media/media.go`: `Type`, `mmsTypes`, `MMSType` |
| Key expansion | `internal/media/media.go`: `DeriveKeys`, `Keys` |
| Encrypt / decrypt and checks | `internal/media/media.go`: `Encrypt`, `Decrypt`, `ErrInvalidMAC`, `ErrHashMismatch`, `ErrInvalidLength` |
| AES-CBC, HMAC, HKDF helpers | `internal/cryptoutil/cryptoutil.go`: `CBCEncrypt`, `CBCDecrypt`, `HMACSHA256`, `HKDF` |
| Upload and download HTTP | `internal/media/transfer.go`: `Transfer.Upload`, `Transfer.Download`, `retryable`, `withoutURL`, `DefaultRetry`, `DefaultMaxDownload` |
| Backoff policy | `internal/backoff`: `Policy.Delay` |
| `media_conn` IQ and cache | `internal/client/mediaops.go`: `Client.mediaConn`, `mediaState` |
| Upload with auth refresh | `internal/client/mediaops.go`: `Client.Upload` |
| Download message media | `internal/client/mediaops.go`: `Client.Download`, `Client.DownloadMedia`, `ErrNoMedia` |
| Building media messages | `internal/client/mediaops.go`: `Client.SendMedia`, `addPreview`, `KindForMIME` |
| WebM to Ogg Opus | `internal/media/webm.go`: `WebMToOgg`, `readWebM`, `blockFrame`, `vint`, `opusSamples`, `opusHead`, `opusTags`, `oggWriter.page`, `oggCRC` |
| REST media input | `api/handlers.go`: `mediaRequest.media` |
| REST media download | `api/handlers.go`: `Server.downloadMedia` |
| History and app state blobs | `internal/client/history.go`: `session.loadHistory`; `internal/client/appsync.go`: `Client.downloadExternal` |
| Profile pictures | `internal/client/picture.go`: `Client.fetchPicture`, `Client.downloadPicture` |
| Tests | `internal/media/media_test.go`, `internal/media/webm_test.go` |
