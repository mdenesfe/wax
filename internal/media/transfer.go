package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdenesfe/wax/internal/backoff"
)

// Origin is sent with media requests, like WhatsApp Web does.
const Origin = "https://web.whatsapp.com"

// DefaultMaxDownload bounds downloads held in memory.
const DefaultMaxDownload = 100 << 20

// Conn is the media server access info from the "media_conn" IQ.
type Conn struct {
	Auth    string
	Hosts   []string
	Expires time.Time
}

// Transfer performs uploads and downloads.
type Transfer struct {
	HTTP        *http.Client // nil: a client with sane timeouts
	MaxDownload int64        // 0: DefaultMaxDownload
	// Scheme is "https" in production; tests may use "http".
	Scheme string
	// Retry spaces download retries on one host; zero: DefaultRetry.
	Retry backoff.Policy
}

// DefaultRetry is the download retry policy.
var DefaultRetry = backoff.Policy{Min: time.Second, Max: 4 * time.Second}

// defaultHTTP is shared so connections are reused across transfers.
var defaultHTTP = &http.Client{Timeout: 10 * time.Minute}

func (t *Transfer) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return defaultHTTP
}

func (t *Transfer) retry() backoff.Policy {
	if t.Retry == (backoff.Policy{}) {
		return DefaultRetry
	}
	return t.Retry
}

func (t *Transfer) scheme() string {
	if t.Scheme != "" {
		return t.Scheme
	}
	return "https"
}

// UploadResult is the media server's answer to an upload.
type UploadResult struct {
	URL        string `json:"url"`
	DirectPath string `json:"direct_path"`
	Handle     string `json:"handle"`
}

// HTTPError is a non-2xx answer from a media server.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("media: http status %d", e.Status) }

// Upload posts an encrypted file to the first media host.
func (t *Transfer) Upload(ctx context.Context, c *Conn, enc *Encrypted, typ Type) (*UploadResult, error) {
	if len(c.Hosts) == 0 {
		return nil, errors.New("media: no upload hosts")
	}
	mms := MMSType(typ)
	if mms == "" {
		return nil, fmt.Errorf("media: no upload path for %q", typ)
	}
	token := base64.URLEncoding.EncodeToString(enc.FileEncSHA256)
	u := url.URL{
		Scheme:   t.scheme(),
		Host:     c.Hosts[0],
		Path:     fmt.Sprintf("/mms/%s/%s", mms, token),
		RawQuery: url.Values{"auth": {c.Auth}, "token": {token}}.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(enc.Data))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(enc.Data))
	req.Header.Set("Origin", Origin)
	req.Header.Set("Referer", Origin+"/")
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("media: upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media: upload: %w", &HTTPError{Status: resp.StatusCode})
	}
	var out UploadResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("media: upload response: %w", err)
	}
	if out.DirectPath == "" {
		return nil, errors.New("media: upload response without direct_path")
	}
	return &out, nil
}

// Download fetches the encrypted blob for directPath, trying each host and
// retrying transient failures. It does not decrypt (see Decrypt).
func (t *Transfer) Download(ctx context.Context, c *Conn, directPath string, fileEncSHA256 []byte, typ Type) ([]byte, error) {
	if !strings.HasPrefix(directPath, "/") {
		return nil, fmt.Errorf("media: direct path %q does not start with /", directPath)
	}
	if len(c.Hosts) == 0 {
		return nil, errors.New("media: no download hosts")
	}
	var lastErr error
	for _, host := range c.Hosts {
		u := fmt.Sprintf("%s://%s%s&hash=%s&mms-type=%s&__wa-mms=", t.scheme(), host, directPath,
			base64.URLEncoding.EncodeToString(fileEncSHA256), MMSType(typ))
		if !strings.Contains(directPath, "?") {
			u = strings.Replace(u, "&hash=", "?hash=", 1)
		}
		for attempt := 1; attempt <= 3; attempt++ {
			data, err := t.get(ctx, u)
			if err == nil {
				return data, nil
			}
			lastErr = err
			var he *HTTPError
			if errors.As(err, &he) && (he.Status == 403 || he.Status == 404 || he.Status == 410) {
				// The file is gone or the path expired; another host won't help.
				return nil, err
			}
			if ctx.Err() != nil || !retryable(err) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(t.retry().Delay(attempt)):
			}
		}
	}
	return nil, fmt.Errorf("media: download failed on all hosts: %w", lastErr)
}

func retryable(err error) bool {
	var ne net.Error
	var he *HTTPError
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) ||
		(errors.As(err, &he) && (he.Status == 429 || he.Status >= 500))
}

func (t *Transfer) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", Origin)
	req.Header.Set("Referer", Origin+"/")
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	max := t.MaxDownload
	if max <= 0 {
		max = DefaultMaxDownload
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("media: file exceeds %d bytes", max)
	}
	return data, nil
}
