package client

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
)

// mediaState caches the media server credentials.
type mediaState struct {
	mu   sync.Mutex
	conn *media.Conn
}

// ErrNoMedia is returned when a message has nothing downloadable.
var ErrNoMedia = errors.New("client: message has no downloadable media")

func (c *Client) mediaConn(ctx context.Context, s *session, force bool) (*media.Conn, error) {
	c.media.mu.Lock()
	defer c.media.mu.Unlock()
	if !force && c.media.conn != nil && time.Now().Before(c.media.conn.Expires) {
		return c.media.conn, nil
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": wabin.ServerJID, "type": "set", "xmlns": "w:m"},
		Content: []wabin.Node{{Tag: "media_conn"}},
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("media_conn: %w", err)
	}
	mc, ok := resp.Child("media_conn")
	if !ok {
		return nil, errors.New("media_conn: missing in response")
	}
	conn := &media.Conn{}
	conn.Auth, _ = mc.AttrString("auth")
	ttl, _ := mc.AttrInt("ttl")
	if ttl <= 0 {
		ttl = 300
	}
	// Refresh a little early so an upload never starts on expired auth.
	conn.Expires = time.Now().Add(time.Duration(ttl)*time.Second - 30*time.Second)
	for _, h := range mc.ChildrenByTag("host") {
		if name, ok := h.AttrString("hostname"); ok && name != "" {
			conn.Hosts = append(conn.Hosts, name)
		}
	}
	if conn.Auth == "" || len(conn.Hosts) == 0 {
		return nil, errors.New("media_conn: no auth or hosts")
	}
	c.media.conn = conn
	return conn, nil
}

func (c *Client) transfer() *media.Transfer {
	return &media.Transfer{HTTP: c.cfg.MediaHTTP, Scheme: c.mediaScheme}
}

// UploadedMedia describes an uploaded file; it carries everything a media
// message needs.
type UploadedMedia struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// Upload encrypts and uploads a file.
func (c *Client) Upload(ctx context.Context, data []byte, typ media.Type) (*UploadedMedia, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	enc, err := media.Encrypt(data, typ)
	if err != nil {
		return nil, err
	}
	conn, err := c.mediaConn(ctx, s, false)
	if err != nil {
		return nil, err
	}
	res, err := c.transfer().Upload(ctx, conn, enc, typ)
	var he *media.HTTPError
	if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
		// Auth token rotated early: refresh once and retry.
		if conn, err = c.mediaConn(ctx, s, true); err == nil {
			res, err = c.transfer().Upload(ctx, conn, enc, typ)
		}
	}
	if err != nil {
		return nil, err
	}
	return &UploadedMedia{URL: res.URL, DirectPath: res.DirectPath, MediaKey: enc.MediaKey,
		FileSHA256: enc.FileSHA256, FileEncSHA256: enc.FileEncSHA256, FileLength: enc.FileLength}, nil
}

// downloadable is implemented by the media message types.
type downloadable interface {
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
}

// Download fetches and decrypts the media of a message (image, video,
// audio, document or sticker).
func (c *Client) Download(ctx context.Context, msg *wae2e.Message) ([]byte, error) {
	msg = unwrapMessage(msg)
	var d downloadable
	var typ media.Type
	switch {
	case msg.GetImageMessage() != nil:
		d, typ = msg.GetImageMessage(), media.Image
	case msg.GetVideoMessage() != nil:
		d, typ = msg.GetVideoMessage(), media.Video
	case msg.GetAudioMessage() != nil:
		d, typ = msg.GetAudioMessage(), media.Audio
	case msg.GetDocumentMessage() != nil:
		d, typ = msg.GetDocumentMessage(), media.Document
	case msg.GetStickerMessage() != nil:
		d, typ = msg.GetStickerMessage(), media.Sticker
	default:
		return nil, ErrNoMedia
	}
	return c.DownloadMedia(ctx, d.GetDirectPath(), d.GetMediaKey(), d.GetFileEncSHA256(), d.GetFileSHA256(), typ)
}

// DownloadMedia downloads and decrypts by explicit parameters (also used
// for history sync and app state blobs).
func (c *Client) DownloadMedia(ctx context.Context, directPath string, mediaKey, fileEncSHA256, fileSHA256 []byte, typ media.Type) ([]byte, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	if directPath == "" || len(mediaKey) == 0 {
		return nil, ErrNoMedia
	}
	conn, err := c.mediaConn(ctx, s, false)
	if err != nil {
		return nil, err
	}
	blob, err := c.transfer().Download(ctx, conn, directPath, fileEncSHA256, typ)
	if err != nil {
		return nil, err
	}
	return media.Decrypt(blob, mediaKey, typ, fileEncSHA256, fileSHA256)
}

// MediaKind selects which message type SendMedia builds.
type MediaKind int

// Media kinds.
const (
	KindImage MediaKind = iota
	KindVideo
	KindAudio
	KindVoiceNote
	KindDocument
)

// SendMediaRequest describes a media message to send.
type SendMediaRequest struct {
	Kind     MediaKind
	Data     []byte
	MimeType string
	Caption  string // image, video, document
	FileName string // document
}

// SendMedia uploads a file and sends it as a media message.
func (c *Client) SendMedia(ctx context.Context, to wabin.JID, req SendMediaRequest) (*SendResult, error) {
	typ := map[MediaKind]media.Type{KindImage: media.Image, KindVideo: media.Video, KindAudio: media.Audio,
		KindVoiceNote: media.Audio, KindDocument: media.Document}[req.Kind]
	if typ == "" {
		return nil, fmt.Errorf("client: unknown media kind %d", req.Kind)
	}
	up, err := c.Upload(ctx, req.Data, typ)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	ts := proto.Int64(time.Now().Unix())
	msg := &wae2e.Message{}
	switch req.Kind {
	case KindImage:
		msg.ImageMessage = &wae2e.ImageMessage{URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileSHA256: up.FileSHA256, FileEncSHA256: up.FileEncSHA256, FileLength: &up.FileLength,
			Mimetype: proto.String(req.MimeType), Caption: optString(req.Caption), MediaKeyTimestamp: ts}
	case KindVideo:
		msg.VideoMessage = &wae2e.VideoMessage{URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileSHA256: up.FileSHA256, FileEncSHA256: up.FileEncSHA256, FileLength: &up.FileLength,
			Mimetype: proto.String(req.MimeType), Caption: optString(req.Caption), MediaKeyTimestamp: ts}
	case KindAudio, KindVoiceNote:
		msg.AudioMessage = &wae2e.AudioMessage{URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileSHA256: up.FileSHA256, FileEncSHA256: up.FileEncSHA256, FileLength: &up.FileLength,
			Mimetype: proto.String(req.MimeType), PTT: proto.Bool(req.Kind == KindVoiceNote), MediaKeyTimestamp: ts}
	case KindDocument:
		name := req.FileName
		if name == "" {
			name = "file"
		}
		msg.DocumentMessage = &wae2e.DocumentMessage{URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileSHA256: up.FileSHA256, FileEncSHA256: up.FileEncSHA256, FileLength: &up.FileLength,
			Mimetype: proto.String(req.MimeType), FileName: proto.String(name), Title: proto.String(name),
			Caption: optString(req.Caption), MediaKeyTimestamp: ts}
	}
	return c.SendMessage(ctx, to, msg)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MarkRead sends read receipts for messages in a chat. In groups, sender is
// the author of the messages.
func (c *Client) MarkRead(ctx context.Context, chat, sender wabin.JID, ids []string, at time.Time) error {
	s, err := c.activeSession()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("client: no message ids")
	}
	attrs := wabin.Attrs{"id": ids[0], "type": "read", "to": chat, "t": strconv.FormatInt(at.Unix(), 10)}
	if chat.Server == wabin.ServerGroup || chat.Server == wabin.ServerBroadcast {
		attrs["participant"] = sender.ToNonAD()
	}
	n := wabin.Node{Tag: "receipt", Attrs: attrs}
	if len(ids) > 1 {
		items := make([]wabin.Node, 0, len(ids)-1)
		for _, id := range ids[1:] {
			items = append(items, wabin.Node{Tag: "item", Attrs: wabin.Attrs{"id": id}})
		}
		n.Content = []wabin.Node{{Tag: "list", Content: items}}
	}
	return s.conn.SendNode(ctx, n)
}
