package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
)

// ErrNoPicture means the user or group has no profile picture, or hides it
// from us (privacy settings).
var ErrNoPicture = errors.New("client: no profile picture")

// Picture is a downloaded profile picture.
type Picture struct {
	Data        []byte
	ContentType string
}

const (
	// pictureTTL is how long a picture (or its absence) is cached. Pictures
	// change rarely, and every lookup is an IQ to WhatsApp.
	pictureTTL = 6 * time.Hour
	// maxPictureLookups bounds concurrent lookups, so opening a long chat
	// list does not burst requests at the server.
	maxPictureLookups = 3
	maxPictureSize    = 2 << 20
)

// pictureEntry caches a picture or its absence (nil).
type pictureEntry struct{ pic *Picture }

// ProfilePicture returns the small (preview) profile picture of a user or
// group, cached. ErrNoPicture is returned (and cached) when there is none
// or it is not visible to us.
func (c *Client) ProfilePicture(ctx context.Context, jid wabin.JID) (*Picture, error) {
	jid = jid.ToNonAD()
	if e, ok := c.pictures.get(jid); ok {
		if e.pic == nil {
			return nil, ErrNoPicture
		}
		return e.pic, nil
	}
	select {
	case c.pictureSlots <- struct{}{}:
		defer func() { <-c.pictureSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pic, err := c.fetchPicture(ctx, jid)
	switch {
	case errors.Is(err, ErrNoPicture):
		c.pictures.put(jid, pictureEntry{})
	case err == nil:
		c.pictures.put(jid, pictureEntry{pic: pic})
	}
	return pic, err
}

// fetchPicture asks the server for the picture's URL, then downloads it.
//
//	<iq to="s.whatsapp.net" target="<jid>" type="get" xmlns="w:profile:picture">
//	  <picture type="preview" query="url"/>
//	</iq>
//
// The answer is <picture url="…" direct_path="…" id="…"/>; a user without a
// picture, or who shows it only to contacts, yields error 404 or 401.
func (c *Client) fetchPicture(ctx context.Context, jid wabin.JID) (*Picture, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": wabin.ServerJID, "target": jid, "type": "get", "xmlns": "w:profile:picture"},
		Content: []wabin.Node{{Tag: "picture", Attrs: wabin.Attrs{"type": "preview", "query": "url"}, Content: c.tokenContent(ctx, jid)}},
	}, 0)
	var iqErr *IQError
	if errors.As(err, &iqErr) && (iqErr.Code == "404" || iqErr.Code == "401") {
		return nil, ErrNoPicture
	}
	if err != nil {
		return nil, fmt.Errorf("profile picture: %w", err)
	}
	p, _ := resp.Child("picture")
	raw, _ := p.AttrString("url")
	if raw == "" {
		return nil, ErrNoPicture
	}
	u, err := url.Parse(raw)
	if err != nil || !c.trustedPictureURL(u) {
		return nil, fmt.Errorf("profile picture: unexpected url host %q", u.Host)
	}
	return c.downloadPicture(ctx, u.String())
}

// trustedPictureURL accepts only HTTPS URLs on WhatsApp's CDN (tests use a
// local HTTP server instead).
func (c *Client) trustedPictureURL(u *url.URL) bool {
	if c.mediaScheme == "http" {
		return u.Scheme == "http"
	}
	return u.Scheme == "https" && (u.Hostname() == "whatsapp.net" || strings.HasSuffix(u.Hostname(), ".whatsapp.net"))
}

func (c *Client) downloadPicture(ctx context.Context, u string) (*Picture, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	hc := c.cfg.MediaHTTP
	if hc == nil {
		hc = pictureHTTP
	}
	res, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is signed; keep it out of errors and logs
		}
		return nil, fmt.Errorf("profile picture download: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNoPicture
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("profile picture download: http %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxPictureSize+1))
	if err != nil {
		return nil, fmt.Errorf("profile picture download: %w", err)
	}
	if len(data) > maxPictureSize {
		return nil, errors.New("profile picture download: too large")
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		return nil, fmt.Errorf("profile picture download: not an image (%s)", ct)
	}
	return &Picture{Data: data, ContentType: ct}, nil
}

var pictureHTTP = &http.Client{Timeout: 20 * time.Second}
