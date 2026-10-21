# Using wax as a Go library

The REST server is one way to use wax; the other is to build the client
into your own Go program. The package `github.com/mdenesfe/wax` links an
account, sends and receives messages and gives you every event; message
contents are the protobuf types of `github.com/mdenesfe/wax/proto/wae2e`.

```bash
go get github.com/mdenesfe/wax
```

The API is young: until version 1.0 it may change between releases.

## What you need

- **PostgreSQL.** wax keeps an account's keys, Signal sessions, contacts
  and history in the database, so a restarted program logs in again
  without a QR code. `wax.Open` creates or migrates the schema. One
  database can hold any number of accounts.
- **A master key**: 32 random bytes in base64 (`openssl rand -base64 32`).
  Every key and session is sealed with it. Keep it outside the database
  backups; without it the account must be linked again.

## A complete program

[examples/echo](../../examples/echo/main.go) links an account and answers
every text message with the same text, quoting it:

```bash
export WAX_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable
export WAX_MASTER_KEY=$(openssl rand -base64 32)
go run ./examples/echo
```

The essential parts:

```go
c, err := wax.Open(ctx, wax.Config{
	DatabaseURL: os.Getenv("WAX_DATABASE_URL"),
	MasterKey:   os.Getenv("WAX_MASTER_KEY"),
	Account:     "main",
	OnEvent: func(e wax.Event) {
		switch e := e.(type) {
		case wax.QRCode:
			qr, _ := wax.QRTerminal(e.Content) // or wax.QRPNG
			fmt.Println(qr)
		case wax.Message:
			fmt.Println(e.Info.Chat, e.Info.PushName, e.Message.GetConversation())
		}
	},
})
if err != nil {
	log.Fatal(err)
}
defer c.Close()

for {
	err := c.Run(ctx) // connects, pairs if needed, reconnects on its own
	if errors.Is(err, wax.ErrLoggedOut) || errors.Is(err, wax.ErrQRTimeout) {
		continue // unlinked or not scanned: show new QR codes
	}
	return err
}
```

`Run` blocks until the context ends. While the account is not linked it
emits `QRCode` events (codes rotate; scan one from the phone in Settings,
Linked devices). Once linked, later runs log in directly.

## Events

`OnEvent` runs on the client's handler goroutine. Return quickly, and do
not call back into the client synchronously: start a goroutine to reply.

| Event | When |
|---|---|
| `QRCode`, `QRTimeout`, `PairSuccess`, `PairError` | linking a new device |
| `Connected`, `Disconnected`, `KeepAliveTimeout` | the connection (Run reconnects by itself) |
| `LoggedOut`, `StreamReplaced`, `ClientOutdated`, `TemporaryBan` | the session ended; see the error Run returns |
| `Message` | a message: `Info` (chat, sender, id, time, push name, `IsFromMe` for our other devices) and `Message`, the unwrapped `*wae2e.Message`; `ViewOnce` marks view once media |
| `UndecryptableMessage` | a message that could not be decrypted; a resend was requested |
| `Receipt` | delivery and read receipts |
| `Presence`, `ChatPresence` | online status, typing and recording |
| `CallEvent` | incoming calls |
| `GroupUpdate`, `IdentityChanged` | group changes, a contact's new security code |
| `HistorySync`, `AppStateChange`, `AppStateSyncComplete`, `AppStateUnverified`, `AppStateSyncError`, `AccountSync` | data synced from the phone |

## Sending

Addresses are `wax.JID`s: `wax.ParseJID("905550000001@s.whatsapp.net")`
or, from a phone number, `wax.ParseRecipient("+90 555 000 00 01")`.

```go
res, err := c.SendText(ctx, to, "Hello!")               // res.ID, res.Timestamp

data, _ := os.ReadFile("photo.jpg")
c.SendMedia(ctx, to, wax.SendMediaRequest{
	Kind: wax.KindImage, Data: data, MimeType: "image/jpeg", Caption: "A photo",
})
```

`SendMedia` handles photos, videos, audio, documents and voice notes
(`KindVoiceNote`, Ogg Opus; `wax.WebMToOgg` converts browser recordings). Optional previews (`Thumbnail`, `Width`, `Height`,
`Seconds`, `Waveform`) make the message look right on phones before it is
downloaded.

Anything else is a `*wae2e.Message` sent with `SendMessage`: a reply sets
`ContextInfo.StanzaID`, `Participant` and `QuotedMessage` on an
`ExtendedTextMessage`; mentions list the users in
`ContextInfo.MentionedJID` with `@<number>` in the text; forwarding sets
`IsForwarded` and `ForwardingScore`. The echo example builds a reply.
How these fields work is described in
[05-messages.md](../protocol/05-messages.md).

## Other calls

| Call | Does |
|---|---|
| `Download(ctx, msg)` | fetches and decrypts a message's media |
| `MarkRead(ctx, chat, sender, ids)` | read receipts |
| `SendPresence`, `SendChatPresence`, `SubscribePresence` | online status, typing |
| `RejectCall` | declines a call |
| `GroupInfo`, `JoinedGroups` | groups, members and communities |
| `Profile`, `ProfilePicture` | about text, devices, business profile; pictures |
| `IsLoggedIn`, `OwnJID`, `Logout` | the session |

Calls before `Run` return `ErrNotRunning`; calls while disconnected return
`ErrNotLoggedIn`. Other errors: `ErrNotOnWhatsApp`, `ErrGroupNotFound`,
`ErrNoMedia`, `ErrNoPicture`, `ErrRateLimited` (profile lookups; WhatsApp
allows only a few in a row).

## Things to know

- **One process per account.** The library does not coordinate several
  processes; running the same account twice makes WhatsApp drop one
  connection (`StreamReplaced`). The server (`wax serve`) adds leases for
  running many accounts on several nodes.
- **No send queue.** `SendText` and friends send at once and return the
  error. Pace your sends; the server has a durable, rate-limited outbox if
  you need one.
- **Stored history.** Messages and chats are stored in the database as
  they arrive (and from the phone's history sync); the REST server lists
  them, the library does not offer queries over them yet.
- **Use it responsibly.** Read the disclaimer in the
  [README](../../README.md#disclaimer).
