package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/manager"
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
	writeJSON(w, http.StatusOK, map[string]string{"code": code, "png_base64": base64.StdEncoding.EncodeToString(png)})
}

// --- sending -------------------------------------------------------------------------

type sendRequest struct {
	To       string        `json:"to"`
	Text     string        `json:"text"`
	Media    *mediaRequest `json:"media"`
	ClientID string        `json:"client_id"`
}

type mediaRequest struct {
	Kind       client.MediaKind `json:"kind"` // "" = from the MIME type
	MimeType   string           `json:"mimetype"`
	Caption    string           `json:"caption"`
	FileName   string           `json:"file_name"`
	DataBase64 string           `json:"data_base64"`
}

// payload validates the request and builds the queued payload.
func (req *sendRequest) payload() (kind string, data []byte, err error) {
	switch {
	case req.Media != nil:
		raw, err := base64.StdEncoding.DecodeString(req.Media.DataBase64)
		if err != nil || len(raw) == 0 {
			return "", nil, badRequest("media.data_base64 must be non-empty base64")
		}
		mp := manager.MediaPayload{Kind: req.Media.Kind, MimeType: req.Media.MimeType, Caption: req.Media.Caption, FileName: req.Media.FileName, Data: raw}
		if mp.MimeType == "" {
			mp.MimeType = http.DetectContentType(raw)
		}
		if mp.Kind == "" {
			mp.Kind = client.KindForMIME(mp.MimeType)
		}
		if !mp.Kind.Valid() {
			return "", nil, badRequest("media.kind must be image, video, audio, voice or document")
		}
		data, err = jsonMarshal(mp)
		return "media", data, err
	case req.Text != "":
		data, err = proto.Marshal(&wae2e.Message{Conversation: proto.String(req.Text)})
		return "message", data, err
	}
	return "", nil, badRequest("text or media is required")
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
	kind, payload, err := req.payload()
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
	writeJSON(w, http.StatusAccepted, map[string]any{"outbox_id": it.ID, "message_id": it.MessageID, "status": it.Status, "duplicate": !created})
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
	writeJSON(w, http.StatusOK, map[string]any{"outbox_id": it.ID, "client_id": it.ClientID, "message_id": it.MessageID, "chat": it.Chat,
		"status": it.Status, "attempts": it.Attempts, "last_error": it.LastError, "server_timestamp": it.ServerTS, "created_at": it.CreatedAt})
}

// --- reading ------------------------------------------------------------------------

func (s *Server) listChats(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	chats, err := s.store.ListChats(r.Context(), in.ID, intQuery(r, "limit", 50, 500), intQuery(r, "offset", 0, 1<<30))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chats)
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
	rows, err := s.store.Messages(r.Context(), in.ID, r.PathValue("chat"), before, intQuery(r, "limit", 50, 500))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]manager.MessageJSON, 0, len(rows))
	for _, m := range rows {
		out = append(out, messageJSON(in.Name, &m))
	}
	writeJSON(w, http.StatusOK, out)
}

func messageJSON(instance string, m *store.StoredMessage) manager.MessageJSON {
	chat, _ := wabin.ParseJID(m.Chat)
	out := manager.MessageJSON{ID: m.ID, Chat: m.Chat, Sender: m.Sender, FromMe: m.FromMe, PushName: m.PushName,
		Timestamp: m.SentAt, Type: m.Type, IsGroup: chat.Server == wabin.ServerGroup, Source: m.Source}
	var msg wae2e.Message
	if m.Content != nil && proto.Unmarshal(m.Content, &msg) == nil {
		out.Text, _, out.Media, out.Reaction, out.QuotedID = manager.DescribeMessage(instance, chat, m.ID, &msg)
	}
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
	var req struct {
		Chat   string   `json:"chat"`
		Sender string   `json:"sender"`
		IDs    []string `json:"ids"`
	}
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
	var req struct {
		Available bool `json:"available"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	s.act(w, r, in, func(cl *client.Client) error { return cl.SendPresence(r.Context(), req.Available) })
}

func (s *Server) rejectCall(w http.ResponseWriter, r *http.Request, in *manager.Instance) {
	var req struct {
		From string `json:"from"`
	}
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
	writeJSON(w, http.StatusOK, gi)
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
