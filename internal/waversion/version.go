// Package waversion models the WhatsApp Web client version we announce to the
// server and detects when WhatsApp ships a newer one.
//
// The server rejects clients that are too old with connect failure 405
// ("client outdated"), so the version must be kept reasonably current. It is
// configurable, and Watch reports when the live WhatsApp Web build changes.
package waversion

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Version is a WhatsApp Web version: primary.secondary.tertiary, e.g.
// 2.3000.1050469598 (the third component is the web client revision).
type Version [3]uint32

// Default is the version used when none is configured. It was the live
// WhatsApp Web revision on 2026-10-15; override it via configuration and rely
// on Watch to learn about newer builds.
var Default = Version{2, 3000, 1050469598}

// ErrParse is returned for malformed version strings.
var ErrParse = errors.New("waversion: invalid version")

// Parse parses "a.b.c".
func Parse(s string) (Version, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q must have three dot-separated parts", ErrParse, s)
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q: %w", ErrParse, s, err)
		}
		v[i] = uint32(n)
	}
	return v, nil
}

// String renders "a.b.c".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// IsZero reports whether the version is unset.
func (v Version) IsZero() bool { return v == Version{} }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// BuildHash is md5("a.b.c"), sent as buildHash during device registration.
// (MD5 here is a protocol-mandated identifier, not a security mechanism.)
func (v Version) BuildHash() [16]byte {
	return md5.Sum([]byte(v.String()))
}

// swURL serves WhatsApp Web's service worker, which embeds the current
// client revision as "client_revision":<n>.
const swURL = "https://web.whatsapp.com/sw.js"

var revisionRe = regexp.MustCompile(`client_revision\\?"\s*:\s*(\d+)`)

// FetchLatest asks WhatsApp Web for its current client revision.
//
// Assumptions, verified on 2026-10-15 and liable to change:
//   - sw.js contains client_revision (JSON-escaped inside a string literal).
//   - The major.minor prefix is 2.3000 (it has been since 2023). sw.js does
//     not state it, so a prefix change would go unnoticed here. Watch the
//     server's 405 failures as a second signal.
//   - Requests without browser-like Sec-Fetch headers get an error page.
func FetchLatest(ctx context.Context, client *http.Client) (Version, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, swURL, nil)
	if err != nil {
		return Version{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "serviceworker")
	req.Header.Set("Sec-Fetch-Mode", "same-origin")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Service-Worker", "script")
	resp, err := client.Do(req)
	if err != nil {
		return Version{}, fmt.Errorf("waversion: fetch %s: %w", swURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Version{}, fmt.Errorf("waversion: fetch %s: HTTP %d", swURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Version{}, fmt.Errorf("waversion: read %s: %w", swURL, err)
	}
	return parseServiceWorker(body)
}

func parseServiceWorker(body []byte) (Version, error) {
	m := revisionRe.FindSubmatch(body)
	if m == nil {
		return Version{}, fmt.Errorf("waversion: client_revision not found in service worker (%d bytes); the page format may have changed", len(body))
	}
	rev, err := strconv.ParseUint(string(m[1]), 10, 32)
	if err != nil {
		return Version{}, fmt.Errorf("waversion: bad client_revision %q: %w", m[1], err)
	}
	return Version{2, 3000, uint32(rev)}, nil
}

// Watch polls FetchLatest every interval until ctx ends and calls onChange
// (and logs a warning) whenever the live version is newer than the one in
// use. current returns the version currently in use, so a caller that hot
// swaps the version stops getting notified.
func Watch(ctx context.Context, log *slog.Logger, client *http.Client, interval time.Duration, current func() Version, onChange func(latest Version)) {
	check := func() {
		latest, err := FetchLatest(ctx, client)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("could not check latest WhatsApp Web version", "error", err)
			}
			return
		}
		// Only newer versions count: during a staged rollout different edge
		// servers return different revisions, so "different" alone flaps.
		if cur := current(); cur.Less(latest) {
			log.Warn("newer WhatsApp Web version available", "in_use", cur.String(), "latest", latest.String())
			if onChange != nil {
				onChange(latest)
			}
		}
	}
	check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
