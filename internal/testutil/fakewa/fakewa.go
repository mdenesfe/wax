// Package fakewa is an in-process stand-in for the WhatsApp server used by
// tests: a real WebSocket endpoint that performs the server side of the
// Noise handshake with a test certificate chain and exposes the decrypted
// node stream to the test. It also plays the phone during QR pairing.
package fakewa

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wa6"
	"github.com/mdenesfe/wax/internal/proto/waadv"
	"github.com/mdenesfe/wax/internal/proto/wacert"
	"github.com/mdenesfe/wax/internal/transport"
)

// Prologue is the connection header the client must send.
var Prologue = []byte{'W', 'A', 6, binary.DictVersion}

// Server is a fake WhatsApp chat endpoint.
type Server struct {
	URL      string
	Verifier *transport.CertVerifier

	hs     *httptest.Server
	static *keys.KeyPair
	chain  []byte
	conns  chan *Conn

	mu   sync.Mutex
	open []*Conn
}

// New starts a server on a random local port.
func New() (*Server, error) {
	root, err := keys.NewSigningKey()
	if err != nil {
		return nil, err
	}
	inter, err := keys.NewSigningKey()
	if err != nil {
		return nil, err
	}
	static, err := keys.NewKeyPair()
	if err != nil {
		return nil, err
	}
	chain, err := certChain(root, inter, static.Pub)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Verifier: &transport.CertVerifier{RootKey: root.Pub, RootSerial: 0},
		static:   static,
		chain:    chain,
		conns:    make(chan *Conn, 16),
	}
	s.hs = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = "ws" + strings.TrimPrefix(s.hs.URL, "http") + "/ws/chat"
	return s, nil
}

func certChain(root, inter *keys.SigningKey, leafKey [32]byte) ([]byte, error) {
	nb, na := uint64(time.Now().Add(-time.Hour).Unix()), uint64(time.Now().Add(24*time.Hour).Unix())
	mk := func(d *wacert.CertChain_NoiseCertificate_Details, signer *keys.SigningKey) (*wacert.CertChain_NoiseCertificate, error) {
		raw, err := proto.Marshal(d)
		if err != nil {
			return nil, err
		}
		sig := signer.Sign(raw)
		return &wacert.CertChain_NoiseCertificate{Details: raw, Signature: sig[:]}, nil
	}
	ic, err := mk(&wacert.CertChain_NoiseCertificate_Details{Serial: proto.Uint32(1), IssuerSerial: proto.Uint32(0), Key: inter.Pub[:], NotBefore: &nb, NotAfter: &na}, root)
	if err != nil {
		return nil, err
	}
	lc, err := mk(&wacert.CertChain_NoiseCertificate_Details{Serial: proto.Uint32(2), IssuerSerial: proto.Uint32(1), Key: leafKey[:], NotBefore: &nb, NotAfter: &na}, inter)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&wacert.CertChain{Intermediate: ic, Leaf: lc})
}

// Close shuts the server and all connections down.
func (s *Server) Close() {
	s.mu.Lock()
	for _, c := range s.open {
		c.Close()
	}
	s.mu.Unlock()
	s.hs.CloseClientConnections()
	s.hs.Close()
}

// Accept waits for the next client to complete the handshake.
func (s *Server) Accept(ctx context.Context) (*Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("fakewa: no connection: %w", ctx.Err())
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ws.SetReadLimit(transport.MaxFrameSize + 64)
	ctx := context.Background()
	fc := &serverFrameConn{ws: ws, expectPrologue: Prologue}
	res, err := transport.ServerHandshake(ctx, fc, transport.ServerHandshakeConfig{
		Prologue: Prologue, StaticKey: s.static, CertChain: s.chain,
	})
	if err != nil {
		ws.CloseNow()
		return
	}
	var payload wa6.ClientPayload
	if err := proto.Unmarshal(res.Payload, &payload); err != nil {
		ws.CloseNow()
		return
	}
	c := &Conn{nc: res.Conn, Payload: &payload, ClientStatic: res.ClientStatic, done: make(chan struct{})}
	s.mu.Lock()
	s.open = append(s.open, c)
	s.mu.Unlock()
	s.conns <- c
	<-c.done // keep the handler (and thus the hijacked connection) alive
}

// Conn is one accepted client connection.
type Conn struct {
	nc           *transport.NoiseConn
	Payload      *wa6.ClientPayload
	ClientStatic [32]byte
	done         chan struct{}
	once         sync.Once
}

// Send encodes and sends a node to the client.
func (c *Conn) Send(ctx context.Context, n binary.Node) error {
	data, err := binary.Marshal(n)
	if err != nil {
		return err
	}
	return c.nc.Send(ctx, data)
}

// Read returns the next node from the client.
func (c *Conn) Read(ctx context.Context) (*binary.Node, error) {
	frame, err := c.nc.Receive(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := binary.Unpack(frame)
	if err != nil {
		return nil, err
	}
	return binary.Unmarshal(payload)
}

// ReadIQ reads until an iq with the given xmlns arrives, answering keepalive
// pings on the way.
func (c *Conn) ReadIQ(ctx context.Context, xmlns string) (*binary.Node, error) {
	return c.ReadUntil(ctx, func(n *binary.Node) bool {
		ns, _ := n.AttrString("xmlns")
		return n.Tag == "iq" && ns == xmlns
	})
}

// ReadUntil reads until match returns true. Keepalive pings are answered
// automatically; other non-matching nodes are discarded.
func (c *Conn) ReadUntil(ctx context.Context, match func(*binary.Node) bool) (*binary.Node, error) {
	for {
		n, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		if match(n) {
			return n, nil
		}
		if ns, _ := n.AttrString("xmlns"); n.Tag == "iq" && ns == "w:p" {
			id, _ := n.AttrString("id")
			if err := c.Send(ctx, Result(id)); err != nil {
				return nil, err
			}
		}
	}
}

// Close drops the connection without a goodbye, like a network failure.
func (c *Conn) Close() {
	c.once.Do(func() {
		c.nc.Close()
		close(c.done)
	})
}

// Result builds an empty iq result for id.
func Result(id string, content ...binary.Node) binary.Node {
	n := binary.Node{Tag: "iq", Attrs: binary.Attrs{"from": binary.ServerJID, "type": "result", "id": id}}
	if len(content) > 0 {
		n.Content = content
	}
	return n
}

// serverFrameConn is the server side of the framed WebSocket: it strips the
// connection header from the client's first message.
type serverFrameConn struct {
	ws             *websocket.Conn
	expectPrologue []byte
	buf            []byte
	writeMu        sync.Mutex
}

func (f *serverFrameConn) SendFrame(ctx context.Context, payload []byte) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	n := len(payload)
	msg := append([]byte{byte(n >> 16), byte(n >> 8), byte(n)}, payload...)
	return f.ws.Write(ctx, websocket.MessageBinary, msg)
}

func (f *serverFrameConn) ReadFrame(ctx context.Context) ([]byte, error) {
	for {
		if f.expectPrologue == nil && len(f.buf) >= 3 {
			n := int(f.buf[0])<<16 | int(f.buf[1])<<8 | int(f.buf[2])
			if len(f.buf) >= 3+n {
				frame := bytes.Clone(f.buf[3 : 3+n])
				f.buf = f.buf[3+n:]
				return frame, nil
			}
		}
		_, data, err := f.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		f.buf = append(f.buf, data...)
		if f.expectPrologue != nil && len(f.buf) >= len(f.expectPrologue) {
			if !bytes.Equal(f.buf[:len(f.expectPrologue)], f.expectPrologue) {
				return nil, errors.New("fakewa: bad connection header")
			}
			f.buf = f.buf[len(f.expectPrologue):]
			f.expectPrologue = nil
		}
	}
}

func (f *serverFrameConn) Close() error { return f.ws.CloseNow() }

// --- phone simulation ---------------------------------------------------------

// QR is a parsed QR code.
type QR struct {
	Ref         string
	NoisePub    [32]byte
	IdentityPub [32]byte
	AdvSecret   [32]byte
}

// ParseQR decodes the content of a QR code shown by the client.
func ParseQR(content string) (*QR, error) {
	_, data, ok := strings.Cut(content, "#")
	if !ok {
		return nil, errors.New("fakewa: qr without #")
	}
	parts := strings.Split(data, ",")
	if len(parts) != 5 {
		return nil, fmt.Errorf("fakewa: qr has %d fields", len(parts))
	}
	q := &QR{Ref: parts[0]}
	for i, dst := range []*[32]byte{&q.NoisePub, &q.IdentityPub, &q.AdvSecret} {
		b, err := base64.StdEncoding.DecodeString(parts[i+1])
		if err != nil || len(b) != 32 {
			return nil, fmt.Errorf("fakewa: qr field %d invalid", i+1)
		}
		*dst = [32]byte(b)
	}
	return q, nil
}

// Phone simulates the primary device that scans the QR code.
type Phone struct {
	Account *keys.SigningKey
	JID     binary.JID // the companion JID the phone assigns (with device)
	LID     binary.JID
}

// NewPhone creates a phone with a fresh account key. Use the test number
// placeholder; no real number is involved.
func NewPhone(user string, device uint16) (*Phone, error) {
	acc, err := keys.NewSigningKey()
	if err != nil {
		return nil, err
	}
	return &Phone{
		Account: acc,
		JID:     binary.JID{User: user, Device: device, Server: binary.ServerUser},
		LID:     binary.JID{User: "1" + user, Device: device, Server: binary.ServerLID},
	}, nil
}

// PairSuccess builds the pair-success IQ the server relays after the phone
// scanned qr.
func (p *Phone) PairSuccess(qr *QR, iqID string) (binary.Node, error) {
	details, err := proto.Marshal(&waadv.ADVDeviceIdentity{
		RawID: proto.Uint32(42), Timestamp: proto.Uint64(uint64(time.Now().Unix())), KeyIndex: proto.Uint32(1),
	})
	if err != nil {
		return binary.Node{}, err
	}
	msg := append(append([]byte{6, 0}, details...), qr.IdentityPub[:]...)
	sig := p.Account.Sign(msg)
	signed, err := proto.Marshal(&waadv.ADVSignedDeviceIdentity{
		Details: details, AccountSignatureKey: p.Account.Pub[:], AccountSignature: sig[:],
	})
	if err != nil {
		return binary.Node{}, err
	}
	mac := hmac.New(sha256.New, qr.AdvSecret[:])
	mac.Write(signed)
	container, err := proto.Marshal(&waadv.ADVSignedDeviceIdentityHMAC{Details: signed, HMAC: mac.Sum(nil)})
	if err != nil {
		return binary.Node{}, err
	}
	return binary.Node{
		Tag:   "iq",
		Attrs: binary.Attrs{"from": binary.ServerJID, "type": "set", "id": iqID, "xmlns": "md", "t": fmt.Sprint(time.Now().Unix())},
		Content: []binary.Node{{Tag: "pair-success", Content: []binary.Node{
			{Tag: "device-identity", Content: container},
			{Tag: "platform", Attrs: binary.Attrs{"name": "android"}},
			{Tag: "device", Attrs: binary.Attrs{"jid": p.JID, "lid": p.LID}},
		}}},
	}, nil
}
