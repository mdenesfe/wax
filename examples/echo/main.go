// Command echo is a minimal wax program: it links an account (showing the
// QR code in the terminal) and answers every text message with the same
// text.
//
//	export WAX_DATABASE_URL=postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable
//	export WAX_MASTER_KEY=$(openssl rand -base64 32)   # keep it: it seals the keys
//	go run ./examples/echo
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax"
	"github.com/mdenesfe/wax/proto/wae2e"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var c *wax.Client
	c, err := wax.Open(ctx, wax.Config{
		DatabaseURL: os.Getenv("WAX_DATABASE_URL"),
		MasterKey:   os.Getenv("WAX_MASTER_KEY"),
		Account:     "echo",
		OnEvent: func(e wax.Event) {
			switch e := e.(type) {
			case wax.QRCode:
				qr, _ := wax.QRTerminal(e.Content)
				fmt.Println("Scan with WhatsApp (Settings, Linked devices):")
				fmt.Println(qr)
			case wax.Connected:
				fmt.Println("connected as", e.JID)
			case wax.Message:
				text := e.Message.GetConversation()
				if text == "" {
					text = e.Message.GetExtendedTextMessage().GetText()
				}
				if e.Info.IsFromMe || text == "" {
					return
				}
				// Reply from another goroutine: event handlers must return quickly.
				go reply(ctx, c, e.Info, text)
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	for {
		err := c.Run(ctx)
		if errors.Is(err, wax.ErrLoggedOut) || errors.Is(err, wax.ErrQRTimeout) {
			fmt.Println("pairing again:", err)
			continue
		}
		if err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
		return
	}
}

// reply quotes the message and repeats its text.
func reply(ctx context.Context, c *wax.Client, info wax.MessageInfo, text string) {
	msg := &wae2e.Message{ExtendedTextMessage: &wae2e.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &wae2e.ContextInfo{
			StanzaID:      proto.String(info.ID),
			Participant:   proto.String(info.Sender.ToNonAD().String()),
			QuotedMessage: &wae2e.Message{Conversation: proto.String(text)},
		},
	}}
	if _, err := c.SendMessage(ctx, info.Chat, msg); err != nil {
		log.Println("reply failed:", err)
	}
	_ = c.MarkRead(ctx, info.Chat, info.Sender, []string{info.ID})
}
