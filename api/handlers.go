package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
)

// --- instances ---------------------------------------------------------------------

type instanceJSON struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	JID           string `json:"jid,omitempty"`
	LID           string `json:"lid,omitempty"`
	WebhookURL    string `json:"webhook_url,omitempty"`
	RatePerMinute int    `json:"rate_per_minute"`
	QRPending     bool   `json:"qr_pending"`
}

func describe(v manager.View) instanceJSON {
	out := instanceJSON{ID: v.Name, Status: string(v.Status), WebhookURL: v.WebhookURL, RatePerMinute: v.RatePerMinute, QRPending: v.QRPending}
	if !v.JID.IsEmpty() {
		out.JID = v.JID.String()
	}
	if !v.LID.IsEmpty() {
		out.LID = v.LID.String()
	}
	return out
}

// view answers with the current view of one instance.
func (s *Server) view(w http.ResponseWriter, r *http.Request, status int, name string) {
	views, err := s.m.Views(r.Context(), tenantOf(r), name)
	if err == nil && len(views) == 0 {
		err = manager.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, status, describe(views[0]))
}

type instanceRequest struct {
	ID            string `json:"id"`
	WebhookURL    string `json:"webhook_url"`
	WebhookSecret string `json:"webhook_secret"`
	RatePerMinute int    `json:"rate_per_minute"`
}

func (req instanceRequest) validate() error {
	if req.WebhookURL != "" {
		u, err := url.Parse(req.WebhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return badRequest("webhook_url must be an absolute http(s) URL without credentials")
		}
	}
	if req.RatePerMinute < 0 || req.RatePerMinute > 600 {
		return badRequest("rate_per_minute must be between 1 and 600")
	}
	return nil
}

// config builds the stored configuration; for an update, id is the
// instance's internal id.
func (req instanceRequest) config(tenant, id string) store.Instance {
	return store.Instance{ID: id, TenantID: tenant, Name: req.ID, WebhookURL: req.WebhookURL,
		WebhookSecret: []byte(req.WebhookSecret), RatePerMinute: req.RatePerMinute}
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) {
	var req instanceRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := req.validate(); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.m.Create(r.Context(), req.config(tenantOf(r), "")); err != nil {
		s.fail(w, err)
		return
	}
	s.view(w, r, http.StatusCreated, req.ID)
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) {
	views, err := s.m.Views(r.Context(), tenantOf(r), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]instanceJSON, len(views))
	for i, v := range views {
		out[i] = describe(v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getInstance(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	s.view(w, r, http.StatusOK, in.Name)
}

// instancePatch is a partial update: absent fields keep their value (a
// PATCH without webhook_secret must not silently turn signing off).
type instancePatch struct {
	WebhookURL    *string `json:"webhook_url"`
	WebhookSecret *string `json:"webhook_secret"`
	RatePerMinute *int    `json:"rate_per_minute"`
}

func (s *Server) updateInstance(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	var patch instancePatch
	if err := decode(r, &patch); err != nil {
		s.fail(w, err)
		return
	}
	cur := in.Config()
	req := instanceRequest{ID: cur.Name, WebhookURL: cur.WebhookURL, WebhookSecret: string(cur.WebhookSecret), RatePerMinute: cur.RatePerMinute}
	if patch.WebhookURL != nil {
		req.WebhookURL = *patch.WebhookURL
	}
	if patch.WebhookSecret != nil {
		req.WebhookSecret = *patch.WebhookSecret
	}
	if patch.RatePerMinute != nil {
		req.RatePerMinute = *patch.RatePerMinute
	}
	if err := req.validate(); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.m.Update(r.Context(), in, req.config(in.TenantID, in.ID)); err != nil {
		s.fail(w, err)
		return
	}
	s.getInstance(w, r, in)
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	if err := s.m.Delete(r.Context(), in); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) connect(w http.ResponseWriter, _ *http.Request, in *manager.Instance) {
	s.m.Connect(in)
	w.WriteHeader(http.StatusAccepted)
}

// qr returns the pending QR code as JSON, or as PNG with ?format=png.
func (s *Server) qr(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	if !in.Running() {
		s.fail(w, manager.ErrNotRunning)
		return
	}
	code := in.QR()
	if code == "" {
		writeError(w, http.StatusNotFound, errorf(errBadRequest, "no QR code pending (status: "+string(in.Status())+")"))
		return
	}
	png, err := pairing.RenderPNG(code)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("format") == "png" {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
		return
	}
	writeJSON(w, http.StatusOK, qrJSON{Code: code, PNGBase64: base64.StdEncoding.EncodeToString(png)})
}

// --- sending -------------------------------------------------------------------------

type sendRequest struct {
	To    string        `json:"to"`
	Text  string        `json:"text"`
	Media *mediaRequest `json:"media"`
	// ReplyTo is the id of a message in the same chat to quote.
	ReplyTo string `json:"reply_to"`
	// Mentions are the users tagged in the text (or caption), which should
	// contain "@<number>" for each.
	Mentions []string `json:"mentions"`
	// Forward sends a copy of a stored message instead of text or media.
	Forward  *forwardRequest `json:"forward"`
	ClientID string          `json:"client_id"`
}

// forwardRequest names a stored message to forward.
type forwardRequest struct {
	Chat      string `json:"chat"`
	MessageID string `json:"message_id"`
}

type mediaRequest struct {
	Kind       client.MediaKind `json:"kind"` // "" = from the MIME type
	MimeType   string           `json:"mimetype"`
	Caption    string           `json:"caption"`
	FileName   string           `json:"file_name"`
	DataBase64 string           `json:"data_base64"`
	// Optional previews: a small JPEG of an image or video, its size, the
	// length of a video or audio, a voice note's waveform (64 values 0-100).
	ThumbnailBase64 string `json:"thumbnail_base64"`
	Width           uint32 `json:"width"`
	Height          uint32 `json:"height"`
	Seconds         uint32 `json:"seconds"`
	WaveformBase64  string `json:"waveform_base64"`
}

// maxThumbnail bounds a preview image; WhatsApp's are a few KiB.
const maxThumbnail = 100 << 10

// media builds the queued form of a media request. Voice notes recorded
// as WebM (browsers) are rewrapped as the Ogg Opus WhatsApp expects.
func (m *mediaRequest) media() (*manager.MediaPayload, error) {
	raw, err := base64.StdEncoding.DecodeString(m.DataBase64)
	if err != nil || len(raw) == 0 {
		return nil, badRequest("media.data_base64 must be non-empty base64")
	}
	mp := &manager.MediaPayload{Kind: m.Kind, MimeType: m.MimeType, Caption: m.Caption, FileName: m.FileName, Data: raw,
		Width: m.Width, Height: m.Height, Seconds: m.Seconds}
	if mp.MimeType == "" {
		mp.MimeType = http.DetectContentType(raw)
	}
	if mp.Kind == "" {
		mp.Kind = client.KindForMIME(mp.MimeType)
	}
	if !mp.Kind.Valid() {
		return nil, badRequest("media.kind must be image, video, audio, voice or document")
	}
	if mp.Thumbnail, err = base64.StdEncoding.DecodeString(m.ThumbnailBase64); err != nil || len(mp.Thumbnail) > maxThumbnail {
		return nil, badRequest("media.thumbnail_base64 must be base64 of at most 100 KiB")
	}
	if mp.Waveform, err = base64.StdEncoding.DecodeString(m.WaveformBase64); err != nil || len(mp.Waveform) > 256 {
		return nil, badRequest("media.waveform_base64 must be base64 of at most 256 bytes")
	}
	if mp.Kind == client.KindVoiceNote && strings.HasPrefix(mp.MimeType, "audio/webm") {
		ogg, secs, err := media.WebMToOgg(raw)
		if err != nil {
			return nil, badRequest("voice note: " + err.Error())
		}
		mp.Data, mp.MimeType = ogg, "audio/ogg; codecs=opus"
		if mp.Seconds == 0 {
			mp.Seconds = secs
		}
	}
	return mp, nil
}

// payload validates the request and builds the queued payload.
// payload builds the outbox item for a send request: a forwarded copy, or
// text or media with its reply and mention context.
func (s *Server) payload(ctx context.Context, in *manager.Instance, to wabin.JID, req *sendRequest) (kind string, data []byte, err error) {
	if req.Forward != nil {
		if req.Text != "" || req.Media != nil || req.ReplyTo != "" || len(req.Mentions) > 0 {
			return "", nil, badRequest("forward cannot be combined with text, media, reply_to or mentions")
		}
		msg, err := s.forwardCopy(ctx, in, req.Forward)
		if err != nil {
			return "", nil, err
		}
		data, err = proto.Marshal(msg)
		return "message", data, err
	}
	ci, err := s.replyContext(ctx, in, to, req)
	if err != nil {
		return "", nil, err
	}
	switch {
	case req.Media != nil:
		mp, err := req.Media.media()
		if err != nil {
			return "", nil, err
		}
		if ci != nil {
			if mp.Context, err = proto.Marshal(ci); err != nil {
				return "", nil, err
			}
		}
		data, err = jsonMarshal(mp)
		return "media", data, err
	case req.Text != "":
		msg := &wae2e.Message{Conversation: proto.String(req.Text)}
		client.SetContext(msg, ci)
		data, err = proto.Marshal(msg)
		return "message", data, err
	}
	return "", nil, badRequest("text, media or forward is required")
}

// replyContext builds the context of a reply and/or mentions (nil when
// there is neither): the quoted message comes from the stored history.
func (s *Server) replyContext(ctx context.Context, in *manager.Instance, to wabin.JID, req *sendRequest) (*wae2e.ContextInfo, error) {
	if req.ReplyTo == "" && len(req.Mentions) == 0 {
		return nil, nil
	}
	ci := &wae2e.ContextInfo{}
	for _, raw := range req.Mentions {
		j, err := recipient(raw)
		if err != nil || (j.Server != wabin.ServerUser && j.Server != wabin.ServerLID) {
			return nil, badRequest("mentions must be users (phone numbers or JIDs)")
		}
		ci.MentionedJID = append(ci.MentionedJID, j.ToNonAD().String())
	}
	if req.ReplyTo == "" {
		return ci, nil
	}
	q, err := s.store.GetMessage(ctx, in.ID, to.String(), req.ReplyTo)
	if err != nil {
		return nil, err
	}
	qm, err := storedContent(q)
	if err != nil {
		return nil, err
	}
	inner, _ := client.Unwrap(qm)
	quoted := proto.Clone(inner).(*wae2e.Message)
	client.SetContext(quoted, nil) // a quote does not carry its own quote
	ci.StanzaID, ci.QuotedMessage = proto.String(q.ID), quoted
	if p := quotedAuthor(in, q); p != "" {
		ci.Participant = proto.String(p)
	}
	return ci, nil
}

// quotedAuthor is the author of a quoted message: its sender, or our own
// account for our messages stored without one.
func quotedAuthor(in *manager.Instance, q *store.StoredMessage) string {
	if q.Sender != "" {
		return nonAD(q.Sender)
	}
	if cl := in.Client(); cl != nil && q.FromMe {
		if jid, _, ok := cl.OwnJID(); ok {
			return jid.ToNonAD().String()
		}
	}
	return ""
}

// forwardCopy is a stored message prepared for forwarding: its content,
// marked as forwarded (once more). View once messages, reactions, edits
// and system messages cannot be forwarded.
func (s *Server) forwardCopy(ctx context.Context, in *manager.Instance, f *forwardRequest) (*wae2e.Message, error) {
	if f.Chat == "" || f.MessageID == "" {
		return nil, badRequest("forward needs chat and message_id")
	}
	m, err := s.store.GetMessage(ctx, in.ID, f.Chat, f.MessageID)
	if err != nil {
		return nil, err
	}
	stored, err := storedContent(m)
	if err != nil {
		return nil, err
	}
	inner, viewOnce := client.Unwrap(stored)
	if viewOnce {
		return nil, badRequest("view once messages cannot be forwarded")
	}
	msg := proto.Clone(inner).(*wae2e.Message)
	ci := &wae2e.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(client.ContextOf(inner).GetForwardingScore() + 1)}
	if !client.SetContext(msg, ci) {
		return nil, badRequest("this message cannot be forwarded")
	}
	return msg, nil
}

// storedContent decodes a stored message's content; system messages have none.
func storedContent(m *store.StoredMessage) (*wae2e.Message, error) {
	var msg wae2e.Message
	if _, stub := manager.DescribeStub(m.Type, nil); stub || m.Content == nil || proto.Unmarshal(m.Content, &msg) != nil {
		return nil, badRequest("message has no content")
	}
	return &msg, nil
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	release, err := s.acquireLarge(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer release()
	var req sendRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	to, err := recipient(req.To)
	if err != nil {
		s.fail(w, err)
		return
	}
	kind, payload, err := s.payload(r.Context(), in, to, &req)
	if err != nil {
		s.fail(w, err)
		return
	}
	it := &store.OutboxItem{ClientID: req.ClientID, Chat: to.String(),
		MessageID: client.GenerateMessageID(), Kind: kind, Payload: payload}
	created, err := s.m.Enqueue(r.Context(), in, it)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sendResponse{OutboxID: it.ID, MessageID: it.MessageID, Status: it.Status, Duplicate: !created})
}

func (s *Server) outboxStatus(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	id, err := strconv.ParseInt(r.PathValue("oid"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid outbox id"))
		return
	}
	it, err := s.store.GetOutbox(r.Context(), in.ID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outboxJSON{OutboxID: it.ID, ClientID: it.ClientID, MessageID: it.MessageID, Chat: it.Chat,
		Status: it.Status, Attempts: it.Attempts, LastError: it.LastError, ServerTimestamp: it.ServerTS, CreatedAt: it.CreatedAt})
}

// --- reading ------------------------------------------------------------------------

func (s *Server) listChats(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	chats, err := s.store.ListChats(r.Context(), in.ID, intQuery(r, "limit", 50, 500), intQuery(r, "offset", 0, 1<<30))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]chatJSON, len(chats))
	var last []*manager.MessageJSON
	for i := range chats {
		out[i].Chat = chats[i]
		if m := chats[i].Last; m != nil {
			j := messageJSON(in.Name, m)
			out[i].LastMessage = &j
			last = append(last, &j)
		}
	}
	if err := s.resolveSenders(r.Context(), in.ID, last); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	before := time.Now().Add(time.Hour)
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			s.fail(w, badRequest("before must be RFC 3339"))
			return
		}
		before = t
	}
	var types []string
	if t := r.URL.Query().Get("types"); t != "" {
		types = strings.Split(t, ",")
	}
	rows, err := s.store.Messages(r.Context(), in.ID, r.PathValue("chat"), before, intQuery(r, "limit", 50, 500), types...)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]manager.MessageJSON, len(rows))
	ptrs := make([]*manager.MessageJSON, len(rows))
	for i := range rows {
		out[i] = messageJSON(in.Name, &rows[i])
		ptrs[i] = &out[i]
	}
	if err := s.resolveSenders(r.Context(), in.ID, ptrs); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveSenders fills in what is known (phone number, address book
// name, push name, username) about the people in messages: other senders,
// quoted authors, mentioned users and users in system messages, with one
// lookup for all of them.
func (s *Server) resolveSenders(ctx context.Context, instance string, msgs []*manager.MessageJSON) error {
	var people []*manager.PersonJSON
	var jids []string
	seen := make(map[string]bool)
	add := func(p *manager.PersonJSON) {
		p.JID = nonAD(p.JID)
		people = append(people, p)
		if !seen[p.JID] {
			seen[p.JID] = true
			jids = append(jids, p.JID)
		}
	}
	senders := make([]manager.PersonJSON, len(msgs))
	for i, m := range msgs {
		if m.Sender != "" && !m.FromMe {
			senders[i].JID = m.Sender
			add(&senders[i])
		}
		if m.Quoted != nil && m.Quoted.Sender != nil {
			add(m.Quoted.Sender)
		}
		for j := range m.Mentions {
			add(&m.Mentions[j])
		}
		if m.Stub != nil {
			for j := range m.Stub.People {
				add(&m.Stub.People[j])
			}
		}
	}
	ids, err := s.store.Identities(ctx, instance, jids)
	if err != nil {
		return err
	}
	for _, p := range people {
		id := ids[p.JID]
		p.Phone, p.Name, p.PushName, p.Username = id.Phone, id.Name, id.PushName, id.Username
	}
	for i, m := range msgs {
		if p := senders[i]; p.JID != "" {
			m.SenderPhone, m.SenderName, m.SenderUsername = p.Phone, p.Name, p.Username
			if m.PushName == "" {
				m.PushName = p.PushName
			}
		}
	}
	return nil
}

// nonAD drops the device part of a user JID ("1234:5@lid" -> "1234@lid").
func nonAD(jid string) string {
	if j, err := wabin.ParseJID(jid); err == nil {
		return j.ToNonAD().String()
	}
	return jid
}

func messageJSON(instance string, m *store.StoredMessage) manager.MessageJSON {
	chat, _ := wabin.ParseJID(m.Chat)
	var out manager.MessageJSON
	var msg wae2e.Message
	if stub, ok := manager.DescribeStub(m.Type, m.Content); ok {
		out.Stub = stub
	} else if m.Content != nil && proto.Unmarshal(m.Content, &msg) == nil {
		out = manager.DescribeMessage(instance, chat, m.ID, &msg)
	}
	// The stored type is kept (it names voice notes "ptt", like history).
	out.ID, out.Chat, out.Sender, out.FromMe, out.PushName = m.ID, m.Chat, m.Sender, m.FromMe, m.PushName
	out.Timestamp, out.Type, out.IsGroup, out.Source = m.SentAt, m.Type, chat.Server == wabin.ServerGroup, m.Source
	return out
}

// --- actions needing a live connection ---------------------------------------------

// connected returns the instance's logged-in client.
func connected(in *manager.Instance) (*client.Client, error) {
	cl := in.Client()
	if cl == nil || !cl.IsLoggedIn() {
		return nil, manager.ErrNotConnected
	}
	return cl, nil
}

func (s *Server) downloadMedia(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	cl, err := connected(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	m, err := s.store.GetMessage(r.Context(), in.ID, r.PathValue("chat"), r.PathValue("mid"))
	if err != nil {
		s.fail(w, err)
		return
	}
	var msg wae2e.Message
	if err := proto.Unmarshal(m.Content, &msg); err != nil {
		s.fail(w, err)
		return
	}
	data, err := cl.Download(r.Context(), &msg)
	if errors.Is(err, client.ErrNoMedia) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	// Peer-supplied bytes: never let a browser sniff or render them.
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	_, _ = w.Write(data)
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	var req markReadRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	chat, err := recipient(req.Chat)
	if err != nil || len(req.IDs) == 0 {
		s.fail(w, badRequest("chat and ids are required"))
		return
	}
	sender, _ := wabin.ParseJID(req.Sender)
	s.act(w, r, in, func(cl *client.Client) error { return cl.MarkRead(r.Context(), chat, sender, req.IDs, time.Now()) })
}

func (s *Server) presence(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	var req presenceRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	s.act(w, r, in, func(cl *client.Client) error { return cl.SendPresence(r.Context(), req.Available) })
}

func (s *Server) rejectCall(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	var req rejectCallRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	from, err := wabin.ParseJID(req.From)
	if err != nil || from.User == "" {
		s.fail(w, badRequest("from must be the caller's JID"))
		return
	}
	s.act(w, r, in, func(cl *client.Client) error { return cl.RejectCall(r.Context(), from, r.PathValue("call")) })
}

// picture serves a contact's or group's profile picture (looked up and
// cached by the node running the instance; 404 when there is none or it
// is hidden).
func (s *Server) picture(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	cl, err := connected(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	jid, err := recipient(r.PathValue("chat"))
	if err != nil {
		s.fail(w, err)
		return
	}
	pic, err := cl.ProfilePicture(r.Context(), jid)
	if errors.Is(err, client.ErrNoPicture) {
		writeError(w, http.StatusNotFound, errors.New("no profile picture"))
		return
	}
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	w.Header().Set("Content-Type", pic.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(pic.Data)
}

func (s *Server) groupInfo(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	cl, err := connected(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	g, err := recipient(r.PathValue("group"))
	if err != nil {
		s.fail(w, err)
		return
	}
	gi, err := cl.GetGroupInfo(r.Context(), g)
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	out, err := s.describeGroup(r.Context(), in.ID, gi)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// profile gathers everything known about a chat's contact: names and
// addresses from the store, about text, devices and business profile from
// WhatsApp (when connected), groups in common, and message counts by type.
// For a group, only the message counts apply (see GET …/groups/{group}).
func (s *Server) profile(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	chat, err := recipient(r.PathValue("chat"))
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	out := profileJSON{JID: chat.String(), CommonGroups: []groupRef{}}
	if out.Messages, err = s.store.MessageTypes(ctx, in.ID, chat.String()); err != nil {
		s.fail(w, err)
		return
	}
	if chat.Server != wabin.ServerUser && chat.Server != wabin.ServerLID {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ids, err := s.store.Identities(ctx, in.ID, []string{chat.String()})
	if err != nil {
		s.fail(w, err)
		return
	}
	id := ids[chat.String()]
	out.Phone, out.LID, out.Name, out.PushName, out.Username = id.Phone, id.LID, id.Name, id.PushName, id.Username
	addrs, user := []wabin.JID{chat}, chat // WhatsApp is asked by phone number when known
	if id.Phone != "" {
		user = wabin.JID{User: id.Phone, Server: wabin.ServerUser}
		addrs = append(addrs, user)
	}
	if id.LID != "" {
		addrs = append(addrs, wabin.JID{User: id.LID, Server: wabin.ServerLID})
	}
	if cl, err := connected(in); err == nil {
		s.liveProfile(ctx, cl, &out, user)
		if groups, err := cl.CommonGroups(ctx, addrs...); err == nil {
			for _, g := range groups {
				out.CommonGroups = append(out.CommonGroups, groupRef{JID: g.JID.String(), Name: g.Name})
			}
		} else {
			s.log.Debug("common groups unavailable", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// liveProfile adds what WhatsApp says about user (by phone number when
// known); failures leave those fields out.
func (s *Server) liveProfile(ctx context.Context, cl *client.Client, out *profileJSON, user wabin.JID) {
	p, err := cl.Profile(ctx, user)
	if err != nil {
		s.log.Debug("profile unavailable", "error", err)
		return
	}
	out.Live, out.About, out.AboutHidden = true, p.About, p.AboutHidden
	out.Devices, out.IsBusiness, out.Business = p.Devices, p.IsBusiness, p.Business
	if !p.AboutSetAt.IsZero() {
		out.AboutSetAt = &p.AboutSetAt
	}
	if out.LID == "" && !p.LID.IsEmpty() {
		out.LID = p.LID.User
	}
}

// listGroups lists the groups and communities the account is in, without
// their participants (see GET …/groups/{group} for those).
func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	cl, err := connected(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	groups, err := cl.JoinedGroups(r.Context())
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	out := make([]groupSummary, len(groups))
	for i, g := range groups {
		out[i] = groupSummary{JID: g.JID.String(), Name: g.Name, Topic: g.Topic, IsCommunity: g.IsCommunity,
			DefaultSubGroup: g.DefaultSubGroup, Announce: g.Announce, Participants: len(g.Participants)}
		if !g.Community.IsEmpty() {
			out[i].Community = g.Community.String()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// describeGroup adds what is known about each participant (number, names,
// username), with one lookup for all of them.
func (s *Server) describeGroup(ctx context.Context, instance string, gi *client.GroupInfo) (*groupJSON, error) {
	jids := make([]string, len(gi.Participants))
	for i, p := range gi.Participants {
		jids[i] = p.JID.ToNonAD().String()
	}
	ids, err := s.store.Identities(ctx, instance, jids)
	if err != nil {
		return nil, err
	}
	out := &groupJSON{GroupInfo: *gi, Participants: make([]participantJSON, len(gi.Participants))}
	for i, p := range gi.Participants {
		id := ids[jids[i]]
		if id.Phone == "" && p.PhoneNumber.Server == wabin.ServerUser {
			id.Phone = p.PhoneNumber.User
		}
		out.Participants[i] = participantJSON{GroupParticipant: p, Phone: id.Phone, Name: id.Name, PushName: id.PushName, Username: id.Username}
	}
	return out, nil
}

// act runs fn on the connected client and answers 204 or the error.
func (s *Server) act(w http.ResponseWriter, r *http.Request, in *manager.Instance, fn func(*client.Client) error) {
	cl, err := connected(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := fn(cl); err != nil {
		s.upstream(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recipient parses a chat from user input as a 400-class error.
func recipient(s string) (wabin.JID, error) {
	j, err := wabin.ParseRecipient(s)
	if err != nil {
		return wabin.JID{}, badRequest("invalid recipient: use a phone number (digits) or a JID")
	}
	return j, nil
}
