package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/transport"
)

// ConnHeader is sent before the first frame and mixed into the Noise
// prologue: "WA", protocol magic 6, token dictionary version.
var ConnHeader = []byte{'W', 'A', 6, binary.DictVersion}

// ConnectOptions configures Connect.
type ConnectOptions struct {
	URL        string // empty: transport.DefaultURL
	HTTPClient *http.Client
	NoiseKey   *keys.KeyPair
	Payload    []byte // serialized ClientPayload
	Verifier   *transport.CertVerifier
}

// Conn is an established, encrypted connection that speaks binary nodes.
type Conn struct {
	nc       *transport.NoiseConn
	idPrefix string
	idSeq    atomic.Uint64
}

// Connect dials WhatsApp, performs the Noise handshake and returns a node
// connection. On failure nothing is left open.
func Connect(ctx context.Context, opts ConnectOptions) (*Conn, error) {
	ws, err := transport.Dial(ctx, transport.DialOptions{
		URL:        opts.URL,
		HTTPClient: opts.HTTPClient,
		Prologue:   ConnHeader,
	})
	if err != nil {
		return nil, err
	}
	nc, err := transport.ClientHandshake(ctx, ws, transport.HandshakeConfig{
		Prologue: ConnHeader,
		NoiseKey: opts.NoiseKey,
		Payload:  opts.Payload,
		Verifier: opts.Verifier,
	})
	if err != nil {
		_ = ws.Close()
		return nil, err
	}
	var p [2]byte
	_, _ = rand.Read(p[:])
	return &Conn{nc: nc, idPrefix: hex.EncodeToString(p[:]) + "-"}, nil
}

// NextID returns a unique stanza id for this connection.
func (c *Conn) NextID() string {
	return c.idPrefix + strconv.FormatUint(c.idSeq.Add(1), 10)
}

// SendNode encodes and sends a node. Safe for concurrent use.
func (c *Conn) SendNode(ctx context.Context, n binary.Node) error {
	data, err := binary.Marshal(n)
	if err != nil {
		return fmt.Errorf("encode <%s>: %w", n.Tag, err)
	}
	return c.nc.Send(ctx, data)
}

// ReadNode reads the next node. Transport errors are fatal for the
// connection. A decoding error is not: it is returned wrapped in
// *DecodeError with the raw payload, and the caller can log it and continue.
// Must be called from a single goroutine.
func (c *Conn) ReadNode(ctx context.Context) (*binary.Node, error) {
	frame, err := c.nc.Receive(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := binary.Unpack(frame)
	if err != nil {
		return nil, &DecodeError{Raw: frame, Err: err}
	}
	n, err := binary.Unmarshal(payload)
	if err != nil {
		return n, &DecodeError{Raw: payload, Err: err}
	}
	return n, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.nc.Close() }

// DecodeError reports an undecodable frame. The connection remains usable.
type DecodeError struct {
	Raw []byte
	Err error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("undecodable frame (%d bytes): %v", len(e.Raw), e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }
