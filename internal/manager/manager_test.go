package manager

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

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

func TestCheckWebhookAddr(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:80", "[::1]:443", "10.1.2.3:80", "192.168.1.5:80", "169.254.169.254:80",
		"[fe80::1]:80", "0.0.0.0:80", "100.64.0.1:80", "[::ffff:127.0.0.1]:80"} {
		if err := checkWebhookAddr(bad); !errors.Is(err, ErrWebhookAddress) {
			t.Errorf("%s: got %v", bad, err)
		}
	}
	for _, good := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"} {
		if err := checkWebhookAddr(good); err != nil {
			t.Errorf("%s rejected: %v", good, err)
		}
	}
}
