package manager

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/wabin"
)

func TestSign(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"event":"x"}`)
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	if got, want := Sign(secret, body), "sha256="+hex.EncodeToString(m.Sum(nil)); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}

func TestTokenBucket(t *testing.T) {
	b := newBucket(6000) // one token per 10ms, burst 3
	ctx := context.Background()
	start := time.Now()
	for range 3 {
		if err := b.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 5*time.Millisecond {
		t.Fatal("burst tokens were not immediate")
	}
	if err := b.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 8*time.Millisecond {
		t.Fatalf("fourth token after %v; rate limit not applied", d)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	b.tokens = 0
	if err := b.wait(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on canceled context: %v", err)
	}
}

func TestPermanent(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("x: %w", errInvalidItem),
		fmt.Errorf("x: %w", client.ErrNotOnWhatsApp),
		fmt.Errorf("x: %w", client.ErrServerRejected),
		wabin.ErrInvalidJID,
	} {
		if !permanent(err) {
			t.Errorf("%v should be permanent", err)
		}
	}
	if permanent(errors.New("connection reset")) || permanent(context.DeadlineExceeded) {
		t.Error("transient error classified as permanent")
	}
}

func TestDescribeMessage(t *testing.T) {
	chat := wabin.JID{User: "905550000000", Server: wabin.ServerUser}
	tests := []struct {
		name      string
		msg       *wae2e.Message
		text, typ string
		media     string
		quoted    string
	}{
		{"text", &wae2e.Message{Conversation: proto.String("hi")}, "hi", "text", "", ""},
		{"reply", &wae2e.Message{ExtendedTextMessage: &wae2e.ExtendedTextMessage{Text: proto.String("re"),
			ContextInfo: &wae2e.ContextInfo{StanzaID: proto.String("Q1")}}}, "re", "text", "", "Q1"},
		{"image", &wae2e.Message{ImageMessage: &wae2e.ImageMessage{Caption: proto.String("cap"), Mimetype: proto.String("image/jpeg"),
			ContextInfo: &wae2e.ContextInfo{StanzaID: proto.String("Q2")}}}, "cap", "image", "image", "Q2"},
		{"voice", &wae2e.Message{AudioMessage: &wae2e.AudioMessage{PTT: proto.Bool(true)}}, "", "voice", "voice", ""},
		{"document", &wae2e.Message{DocumentMessage: &wae2e.DocumentMessage{FileName: proto.String("a.pdf")}}, "", "document", "document", ""},
		{"reaction", &wae2e.Message{ReactionMessage: &wae2e.ReactionMessage{Text: proto.String("👍")}}, "", "reaction", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, typ, media, _, quoted := DescribeMessage("inst", chat, "ID1", tt.msg)
			if text != tt.text || typ != tt.typ || quoted != tt.quoted {
				t.Fatalf("got (%q, %q, quoted %q)", text, typ, quoted)
			}
			switch {
			case tt.media == "" && media != nil:
				t.Fatalf("unexpected media %+v", media)
			case tt.media != "" && (media == nil || media.Kind != tt.media):
				t.Fatalf("media = %+v, want kind %s", media, tt.media)
			case media != nil && media.Download != "/v1/instances/inst/chats/905550000000@s.whatsapp.net/messages/ID1/media":
				t.Fatalf("download path %s", media.Download)
			}
			if tt.name == "document" && media.FileName != "a.pdf" {
				t.Fatalf("file name %q", media.FileName)
			}
		})
	}
}
