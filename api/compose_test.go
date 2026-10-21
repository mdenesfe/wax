package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// TestRepliesMentionsForwards: replies quote the stored message, mentions
// are carried, forwards are marked, and listings describe all of it with
// the people resolved. The account is not connected, so sends stay queued.
func TestRepliesMentionsForwards(t *testing.T) {
	e := setup(t)
	e.ok("POST", "/v1/instances", map[string]any{"id": "ctx"}, http.StatusCreated)
	internal := manager.InternalID(e.tenant, "ctx")
	const chat, ali = "905551111111@s.whatsapp.net", "905552222222@s.whatsapp.net"
	// Rows reference the instance's device row, created in the background.
	waitFor(t, "device row", 5*time.Second, func() bool {
		return e.st.PutContactName(e.ctx, internal, chat, store.ContactName{Full: "Veli"}) == nil
	})
	mustT(t, e.st.PutContactName(e.ctx, internal, ali, store.ContactName{Full: "Ali", Username: "ali"}))

	put := func(id, typ string, msg *wae2e.Message, raw []byte) {
		if msg != nil {
			raw, _ = proto.Marshal(msg)
		}
		mustT(t, e.st.PutMessage(e.ctx, internal, &store.StoredMessage{Chat: chat, ID: id, Sender: chat, SentAt: time.Unix(1700000000, 0),
			Type: typ, Content: raw, Source: "history"}))
	}
	put("M1", "text", &wae2e.Message{Conversation: proto.String("selam")}, nil)
	put("V1", "image", &wae2e.Message{ViewOnceMessageV2: &wae2e.FutureProofMessage{Message: &wae2e.Message{ImageMessage: &wae2e.ImageMessage{Mimetype: proto.String("image/jpeg")}}}}, nil)
	put("S1", "stub:27", nil, []byte(`["`+ali+`"]`))
	put("R1", "text", &wae2e.Message{ExtendedTextMessage: &wae2e.ExtendedTextMessage{Text: proto.String("@905552222222 bak"),
		ContextInfo: &wae2e.ContextInfo{StanzaID: proto.String("M1"), Participant: proto.String(chat), QuotedMessage: &wae2e.Message{Conversation: proto.String("selam")},
			MentionedJID: []string{ali}, IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(5)}}}, nil)

	queued := func(body map[string]any) *wae2e.Message {
		t.Helper()
		var r sendResponse
		mustT(t, json.Unmarshal(e.ok("POST", "/v1/instances/ctx/messages", body, http.StatusAccepted), &r))
		it, err := e.st.GetOutbox(e.ctx, internal, r.OutboxID)
		mustT(t, err)
		var m wae2e.Message
		mustT(t, proto.Unmarshal(it.Payload, &m))
		return &m
	}

	// A reply with a mention.
	m := queued(map[string]any{"to": chat, "text": "@905552222222 tamam", "reply_to": "M1", "mentions": []string{"905552222222"}})
	ci := m.GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "M1" || ci.GetParticipant() != chat || ci.GetQuotedMessage().GetConversation() != "selam" ||
		len(ci.GetMentionedJID()) != 1 || ci.GetMentionedJID()[0] != ali || m.GetExtendedTextMessage().GetText() != "@905552222222 tamam" {
		t.Fatalf("reply %v", m)
	}
	// A forward: the content, marked once more.
	m = queued(map[string]any{"to": ali, "forward": map[string]any{"chat": chat, "message_id": "R1"}})
	ci = m.GetExtendedTextMessage().GetContextInfo()
	if !ci.GetIsForwarded() || ci.GetForwardingScore() != 6 || ci.GetStanzaID() != "" || m.GetExtendedTextMessage().GetText() != "@905552222222 bak" {
		t.Fatalf("forward %v", m)
	}
	// What cannot be done.
	e.ok("POST", "/v1/instances/ctx/messages", map[string]any{"to": ali, "forward": map[string]any{"chat": chat, "message_id": "V1"}}, http.StatusBadRequest)
	e.ok("POST", "/v1/instances/ctx/messages", map[string]any{"to": chat, "text": "x", "reply_to": "S1"}, http.StatusBadRequest)
	e.ok("POST", "/v1/instances/ctx/messages", map[string]any{"to": chat, "text": "x", "reply_to": "nope"}, http.StatusNotFound)
	e.ok("POST", "/v1/instances/ctx/messages", map[string]any{"to": chat, "text": "x", "forward": map[string]any{"chat": chat, "message_id": "M1"}}, http.StatusBadRequest)

	// Listings.
	var msgs []manager.MessageJSON
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/ctx/chats/"+chat+"/messages", nil, 200), &msgs))
	byID := map[string]manager.MessageJSON{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	if r := byID["R1"]; r.Quoted == nil || r.Quoted.Text != "selam" || r.Quoted.Sender.Name != "Veli" ||
		len(r.Mentions) != 1 || r.Mentions[0].Name != "Ali" || r.Mentions[0].Username != "ali" || !r.Forwarded || r.ForwardingScore != 5 {
		t.Fatalf("reply listing %+v", r)
	}
	if v := byID["V1"]; !v.ViewOnce || v.Media == nil {
		t.Fatalf("view once listing %+v", v)
	}
	if s := byID["S1"]; s.Stub == nil || s.Stub.Name != "group_participant_add" || len(s.Stub.People) != 1 || s.Stub.People[0].Name != "Ali" {
		t.Fatalf("stub listing %+v", s.Stub)
	}
	if byID["M1"].SenderName != "Veli" {
		t.Fatalf("sender %+v", byID["M1"])
	}

	// Filtering by type, and the profile from what is stored (offline).
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/ctx/chats/"+chat+"/messages?types=image,video", nil, 200), &msgs))
	if len(msgs) != 1 || msgs[0].ID != "V1" {
		t.Fatalf("filtered %+v", msgs)
	}
	var prof profileJSON
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/ctx/chats/"+chat+"/profile", nil, 200), &prof))
	if prof.Name != "Veli" || prof.Phone != "905551111111" || prof.Messages["text"] != 2 || prof.Messages["image"] != 1 || prof.Messages["stub:27"] != 1 {
		t.Fatalf("profile %+v", prof)
	}
}

// TestMediaPreviews: previews are queued with the media, and a voice note
// recorded as WebM (a 3.2 s tone made with ffmpeg) is queued as Ogg Opus
// with its length. The account is not connected, so nothing is sent.
func TestMediaPreviews(t *testing.T) {
	e := setup(t)
	e.ok("POST", "/v1/instances", map[string]any{"id": "media"}, http.StatusCreated)
	internal := manager.InternalID(e.tenant, "media")
	queued := func(media map[string]any) manager.MediaPayload {
		t.Helper()
		var r sendResponse
		mustT(t, json.Unmarshal(e.ok("POST", "/v1/instances/media/messages", map[string]any{"to": "905551111111", "media": media}, http.StatusAccepted), &r))
		it, err := e.st.GetOutbox(e.ctx, internal, r.OutboxID)
		mustT(t, err)
		var mp manager.MediaPayload
		mustT(t, json.Unmarshal(it.Payload, &mp))
		return mp
	}
	b64 := base64.StdEncoding.EncodeToString
	img := queued(map[string]any{"kind": "image", "mimetype": "image/jpeg", "data_base64": b64([]byte("jpeg")),
		"thumbnail_base64": b64([]byte("thumb")), "width": 640, "height": 480})
	if string(img.Thumbnail) != "thumb" || img.Width != 640 || img.Height != 480 {
		t.Fatalf("image %+v", img)
	}
	webm, err := os.ReadFile("testdata/voice.webm")
	mustT(t, err)
	voice := queued(map[string]any{"kind": "voice", "mimetype": "audio/webm;codecs=opus", "data_base64": b64(webm),
		"waveform_base64": b64(bytes.Repeat([]byte{50}, 64))})
	if !bytes.HasPrefix(voice.Data, []byte("OggS")) || voice.MimeType != "audio/ogg; codecs=opus" || voice.Seconds != 3 || len(voice.Waveform) != 64 {
		t.Fatalf("voice: %s, %d s, %d waveform bytes, starts %q", voice.MimeType, voice.Seconds, len(voice.Waveform), voice.Data[:4])
	}
	e.ok("POST", "/v1/instances/media/messages", map[string]any{"to": "905551111111", "media": map[string]any{"kind": "voice",
		"mimetype": "audio/webm", "data_base64": b64([]byte("not webm"))}}, http.StatusBadRequest)
}
