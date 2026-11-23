package client

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/proto/wae2e"
)

// TestReportingContent: only covered fields count, nested messages keep
// only their covered fields, and fields come out ordered by number.
func TestReportingContent(t *testing.T) {
	text := protowire.AppendTag(nil, 1, protowire.BytesType)
	text = protowire.AppendString(text, "selam")
	// Field 35 (messageContextInfo) is not covered.
	ctx := protowire.AppendTag(nil, 35, protowire.BytesType)
	ctx = protowire.AppendBytes(ctx, []byte{0x1a, 0x01, 0x09})
	// Field 6 (extendedTextMessage): 1 is covered, 2 is not.
	inner := protowire.AppendTag(nil, 2, protowire.BytesType)
	inner = protowire.AppendString(inner, "x")
	innerText := protowire.AppendTag(nil, 1, protowire.BytesType)
	innerText = protowire.AppendString(innerText, "hi")
	inner = append(inner, innerText...)
	ext := protowire.AppendTag(nil, 6, protowire.BytesType)
	ext = protowire.AppendBytes(ext, inner)

	in := append(append(append([]byte{}, ext...), ctx...), text...)
	got, ok := reportingContent(in, reportingFields())
	if !ok {
		t.Fatal("well-formed input rejected")
	}
	wantExt := protowire.AppendTag(nil, 6, protowire.BytesType)
	wantExt = protowire.AppendBytes(wantExt, innerText)
	if want := append(append([]byte{}, text...), wantExt...); !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}

	if _, ok := reportingContent([]byte{0x0a, 0x05, 'a'}, reportingFields()); ok {
		t.Fatal("truncated input accepted")
	}
}

// TestMessageSecret: every message but a reaction gets a secret, on a copy;
// an existing secret is kept.
func TestMessageSecret(t *testing.T) {
	orig := &wae2e.Message{Conversation: proto.String("selam")}
	m := withMessageSecret(orig)
	if len(m.GetMessageContextInfo().GetMessageSecret()) != 32 || orig.MessageContextInfo != nil {
		t.Fatalf("secret %v, original %v", m, orig)
	}
	if again := withMessageSecret(m); !bytes.Equal(again.GetMessageContextInfo().GetMessageSecret(), m.GetMessageContextInfo().GetMessageSecret()) {
		t.Fatal("existing secret replaced")
	}
	r := &wae2e.Message{ReactionMessage: &wae2e.ReactionMessage{Text: proto.String("+")}}
	if withMessageSecret(r).GetMessageContextInfo() != nil {
		t.Fatal("reaction got a secret")
	}
}
