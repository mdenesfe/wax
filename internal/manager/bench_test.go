package manager

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/wabin"
)

func BenchmarkToWebhook(b *testing.B) {
	ev := client.Message{Info: client.MessageInfo{ID: "3EB0C80AFF25C8F8F2075B",
		Chat: wabin.JID{User: "905550000000", Server: wabin.ServerUser}, Sender: wabin.JID{User: "905550000000", Server: wabin.ServerUser}},
		Message: &wae2e.Message{ExtendedTextMessage: &wae2e.ExtendedTextMessage{Text: proto.String("merhaba dünya")}}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, ok := toWebhook("inst", ev); !ok {
			b.Fatal("not delivered")
		}
	}
}

func BenchmarkSign(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 2048)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		Sign([]byte("secret"), body)
	}
}
