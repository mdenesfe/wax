package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Default endpoint values for WhatsApp Web multi-device.
const (
	DefaultURL    = "wss://web.whatsapp.com/ws/chat"
	DefaultOrigin = "https://web.whatsapp.com"
)

// DialOptions configures the WebSocket connection.
type DialOptions struct {
	URL        string       // defaults to DefaultURL
	Origin     string       // defaults to DefaultOrigin
	HTTPClient *http.Client // optional, e.g. for proxies
	// Prologue is sent once, immediately before the first frame. For WhatsApp
	// it is the connection header ("WA", 6, dictionary version), optionally
	// preceded by an edge-routing header.
	Prologue []byte
}

// WSConn is a FrameConn over a WebSocket.
type WSConn struct {
	conn *websocket.Conn

	writeMu  sync.Mutex
	prologue []byte // pending; nil once sent

	splitter frameSplitter
	pending  [][]byte

	closeOnce sync.Once
}

var _ FrameConn = (*WSConn)(nil)

// Dial opens the WebSocket. The returned connection is not yet encrypted.
func Dial(ctx context.Context, opts DialOptions) (*WSConn, error) {
	url := opts.URL
	if url == "" {
		url = DefaultURL
	}
	origin := opts.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: opts.HTTPClient,
		HTTPHeader: http.Header{"Origin": {origin}},
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("transport: dial %s: HTTP %d: %w", url, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("transport: dial %s: %w", url, err)
	}
	// One WebSocket message never needs to exceed one maximal frame plus header.
	conn.SetReadLimit(MaxFrameSize + 64)
	return &WSConn{conn: conn, prologue: opts.Prologue}, nil
}

// SendFrame writes one length-prefixed frame (preceded by the prologue on the
// first call) as a single binary WebSocket message.
func (c *WSConn) SendFrame(ctx context.Context, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	msg, err := appendFrame(make([]byte, 0, len(c.prologue)+FrameLengthSize+len(payload)), c.prologue, payload)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageBinary, msg); err != nil {
		return c.wrapErr(err)
	}
	c.prologue = nil
	return nil
}

// ReadFrame returns the next complete frame. Note that cancelling ctx closes
// the underlying WebSocket (a property of coder/websocket), so callers should
// pass the connection's lifetime context, not a per-read timeout.
func (c *WSConn) ReadFrame(ctx context.Context) ([]byte, error) {
	for len(c.pending) == 0 {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, c.wrapErr(err)
		}
		if typ != websocket.MessageBinary {
			continue // the protocol only uses binary messages; ignore anything else
		}
		c.pending = c.splitter.push(data)
	}
	f := c.pending[0]
	c.pending[0] = nil
	c.pending = c.pending[1:]
	return f, nil
}

// Close closes the WebSocket without waiting for the peer's close frame.
func (c *WSConn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.CloseNow() })
	return err
}

// CloseStatus extracts the WebSocket close code from an error returned by
// ReadFrame/SendFrame, or -1 if the error did not carry one.
func CloseStatus(err error) int {
	return int(websocket.CloseStatus(err))
}

func (c *WSConn) wrapErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrClosed, err)
}
