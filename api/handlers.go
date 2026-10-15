package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
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

func describe(in *manager.Instance) instanceJSON {
	cfg := in.Config()
	out := instanceJSON{ID: in.ID, Status: string(in.Status()), WebhookURL: cfg.WebhookURL, RatePerMinute: cfg.RatePerMinute, QRPending: in.QR() != ""}
	if cl := in.Client(); cl != nil {
		if jid, lid, ok := cl.OwnJID(); ok {
			out.JID, out.LID = jid.ToNonAD().String(), lid.ToNonAD().String()
		}
	}
	return out
}

type instanceRequest struct {
	ID            string `json:"id"`
	WebhookURL    string `json:"webhook_url"`
	WebhookSecret string `json:"webhook_secret"`
	RatePerMinute int    `json:"rate_per_minute"`
}

func (req instanceRequest) validate() error {
	if req.WebhookURL != "" && !strings.HasPrefix(req.WebhookURL, "https://") && !strings.HasPrefix(req.WebhookURL, "http://") {
		return badRequest("webhook_url must be an http(s) URL")
	}
	if req.RatePerMinute < 0 || req.RatePerMinute > 600 {
		return badRequest("rate_per_minute must be between 1 and 600")
	}
	return nil
}

func (req instanceRequest) config() store.Instance {
	return store.Instance{ID: req.ID, WebhookURL: req.WebhookURL, WebhookSecret: []byte(req.WebhookSecret), RatePerMinute: req.RatePerMinute}
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
	in, err := s.m.Create(r.Context(), req.config())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, describe(in))
}

func (s *Server) listInstances(w http.ResponseWriter, _ *http.Request) {
	list := s.m.List()
	out := make([]instanceJSON, len(list))
	for i, in := range list {
		out[i] = describe(in)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getInstance(w http.ResponseWriter, r *http.Request) {
	in, err := s.instance(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, describe(in))
}

func (s *Server) updateInstance(w http.ResponseWriter, r *http.Request) {
	var req instanceRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	req.ID = r.PathValue("id")
	if err := req.validate(); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.m.Update(r.Context(), req.config()); err != nil {
		s.fail(w, err)
		return
	}
	s.getInstance(w, r)
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Delete(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Connect(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// qr returns the pending QR code as JSON, or as PNG with ?format=png.
func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	in, err := s.instance(r)
	if err != nil {
		s.fail(w, err)
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

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
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
	it := &store.OutboxItem{Instance: r.PathValue("id"), ClientID: req.ClientID, Chat: to.String(),
		MessageID: client.GenerateMessageID(), Kind: kind, Payload: payload}
	created, err := s.m.Enqueue(r.Context(), it)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"outbox_id": it.ID, "message_id": it.MessageID, "status": it.Status, "duplicate": !created})
}

func (s *Server) outboxStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("oid"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid outbox id"))
		return
	}
	it, err := s.store.GetOutbox(r.Context(), r.PathValue("id"), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outbox_id": it.ID, "client_id": it.ClientID, "message_id": it.MessageID, "chat": it.Chat,
		"status": it.Status, "attempts": it.Attempts, "last_error": it.LastError, "server_timestamp": it.ServerTS, "created_at": it.CreatedAt})
}

// --- reading ------------------------------------------------------------------------

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	if _, err := s.instance(r); err != nil {
		s.fail(w, err)
		return
	}
	chats, err := s.store.ListChats(r.Context(), r.PathValue("id"), intQuery(r, "limit", 50, 500), intQuery(r, "offset", 0, 1<<30))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chats)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.instance(r); err != nil {
		s.fail(w, err)
		return
	}
	before := time.Now().Add(time.Hour)
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			s.fail(w, badRequest("before must be RFC 3339"))
			return
		}
		before = t
	}
	rows, err := s.store.Messages(r.Context(), id, r.PathValue("chat"), before, intQuery(r, "limit", 50, 500))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]manager.MessageJSON, 0, len(rows))
	for _, m := range rows {
		out = append(out, messageJSON(id, &m))
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

// connected returns the logged-in client of the request's instance.
func (s *Server) connected(r *http.Request) (*client.Client, error) {
	in, err := s.instance(r)
	if err != nil {
		return nil, err
	}
	cl := in.Client()
	if cl == nil || !cl.IsLoggedIn() {
		return nil, manager.ErrNotConnected
	}
	return cl, nil
}

func (s *Server) downloadMedia(w http.ResponseWriter, r *http.Request) {
	cl, err := s.connected(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	m, err := s.store.GetMessage(r.Context(), r.PathValue("id"), r.PathValue("chat"), r.PathValue("mid"))
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
	if err == client.ErrNoMedia {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	_, _ = w.Write(data)
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
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
	s.act(w, r, func(cl *client.Client) error { return cl.MarkRead(r.Context(), chat, sender, req.IDs, time.Now()) })
}

func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Available bool `json:"available"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	s.act(w, r, func(cl *client.Client) error { return cl.SendPresence(r.Context(), req.Available) })
}

func (s *Server) rejectCall(w http.ResponseWriter, r *http.Request) {
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
	s.act(w, r, func(cl *client.Client) error { return cl.RejectCall(r.Context(), from, r.PathValue("call")) })
}

func (s *Server) groupInfo(w http.ResponseWriter, r *http.Request) {
	cl, err := s.connected(r)
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
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, gi)
}

// act runs fn on the connected client and answers 204 or the error.
func (s *Server) act(w http.ResponseWriter, r *http.Request, fn func(*client.Client) error) {
	cl, err := s.connected(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := fn(cl); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recipient parses a chat from user input as a 400-class error.
func recipient(s string) (wabin.JID, error) {
	j, err := wabin.ParseRecipient(s)
	if err != nil {
		return wabin.JID{}, badRequest(err.Error())
	}
	return j, nil
}
