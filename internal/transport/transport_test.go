package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wa6"
	"github.com/mdenesfe/wax/internal/proto/wacert"
)

var testPrologue = []byte{'W', 'A', 6, 3}

// pipeConn is an in-memory FrameConn. Frames are passed through a real byte
// stream (length-prefixed, re-split in random chunk sizes) so framing is
// exercised too.
type pipeConn struct {
	in       chan []byte
	out      chan []byte
	closed   chan struct{}
	once     sync.Once
	splitter frameSplitter
	pending  [][]byte
	peer     *pipeConn
	tamper   func(frame []byte) // optional hook on outgoing frames
}

func newPipe() (*pipeConn, *pipeConn) {
	ab := make(chan []byte, 64)
	ba := make(chan []byte, 64)
	a := &pipeConn{in: ba, out: ab, closed: make(chan struct{})}
	b := &pipeConn{in: ab, out: ba, closed: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

func (p *pipeConn) SendFrame(ctx context.Context, payload []byte) error {
	frame := bytes.Clone(payload)
	if p.tamper != nil {
		p.tamper(frame)
	}
	wire, err := appendFrame(nil, nil, frame)
	if err != nil {
		return err
	}
	// Deliver the first bytes in 1..7 byte chunks (splitting the length
	// prefix) and the remainder in one chunk, to exercise reassembly without
	// overflowing the channel buffer.
	for i := 0; i < len(wire); {
		n := len(wire) - i
		if i < 16 {
			n = min(1+i%7, n)
		}
		select {
		case p.out <- wire[i : i+n]:
		case <-p.closed:
			return ErrClosed
		case <-p.peer.closed:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
		i += n
	}
	return nil
}

func (p *pipeConn) ReadFrame(ctx context.Context) ([]byte, error) {
	for len(p.pending) == 0 {
		select {
		case chunk := <-p.in:
			p.pending = p.splitter.push(chunk)
		case <-p.closed:
			return nil, ErrClosed
		case <-p.peer.closed:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := p.pending[0]
	p.pending = p.pending[1:]
	return f, nil
}

func (p *pipeConn) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// testPKI is a fake WhatsApp certificate hierarchy.
type testPKI struct {
	root, intermediate *keys.SigningKey
	serverStatic       *keys.KeyPair
	notBefore          time.Time
	notAfter           time.Time
	leafKeyOverride    []byte
	interIssuer        uint32
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	root, err := keys.NewSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	inter, _ := keys.NewSigningKey()
	static, _ := keys.NewKeyPair()
	return &testPKI{
		root: root, intermediate: inter, serverStatic: static,
		notBefore: time.Now().Add(-time.Hour), notAfter: time.Now().Add(time.Hour),
	}
}

func (p *testPKI) verifier() *CertVerifier {
	return &CertVerifier{RootKey: p.root.Pub, RootSerial: 0}
}

func (p *testPKI) chain(t *testing.T) []byte {
	t.Helper()
	mk := func(d *wacert.CertChain_NoiseCertificate_Details, signer *keys.SigningKey) *wacert.CertChain_NoiseCertificate {
		raw, err := proto.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sig := signer.Sign(raw)
		return &wacert.CertChain_NoiseCertificate{Details: raw, Signature: sig[:]}
	}
	leafKey := p.serverStatic.Pub[:]
	if p.leafKeyOverride != nil {
		leafKey = p.leafKeyOverride
	}
	nb, na := uint64(p.notBefore.Unix()), uint64(p.notAfter.Unix())
	chain := &wacert.CertChain{
		Intermediate: mk(&wacert.CertChain_NoiseCertificate_Details{
			Serial: proto.Uint32(42), IssuerSerial: proto.Uint32(p.interIssuer), Key: p.intermediate.Pub[:],
			NotBefore: &nb, NotAfter: &na,
		}, p.root),
		Leaf: mk(&wacert.CertChain_NoiseCertificate_Details{
			Serial: proto.Uint32(43), IssuerSerial: proto.Uint32(42), Key: leafKey,
			NotBefore: &nb, NotAfter: &na,
		}, p.intermediate),
	}
	raw, err := proto.Marshal(chain)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// serverResult is what the simulated server learned.
type serverResult struct {
	clientStatic [32]byte
	payload      []byte
	conn         *NoiseConn
	err          error
}

// runServer implements the responder side of WhatsApp's XX handshake,
// written independently of the client code path (only the shared symmetric
// state primitives are reused).
func runServer(ctx context.Context, fc FrameConn, pki *testPKI, certChain []byte) serverResult {
	fail := func(step string, err error) serverResult {
		return serverResult{err: fmt.Errorf("server %s: %w", step, err)}
	}
	ss := newSymmetricState(NoiseProtocolName, testPrologue)
	raw, err := fc.ReadFrame(ctx)
	if err != nil {
		return fail("read hello", err)
	}
	var hello wa6.HandshakeMessage
	if err := proto.Unmarshal(raw, &hello); err != nil {
		return fail("decode hello", err)
	}
	ce := [32]byte(hello.GetClientHello().GetEphemeral())
	ss.mixHash(ce[:])

	se, _ := keys.NewKeyPair()
	ss.mixHash(se.Pub[:])
	if err := ss.mixDH(se.Priv, ce); err != nil {
		return fail("ee", err)
	}
	encStatic := ss.encryptAndHash(pki.serverStatic.Pub[:])
	if err := ss.mixDH(pki.serverStatic.Priv, ce); err != nil {
		return fail("es", err)
	}
	encCert := ss.encryptAndHash(certChain)
	out, _ := proto.Marshal(&wa6.HandshakeMessage{ServerHello: &wa6.HandshakeMessage_ServerHello{
		Ephemeral: se.Pub[:], Static: encStatic, Payload: encCert,
	}})
	if err := fc.SendFrame(ctx, out); err != nil {
		return fail("send hello", err)
	}

	raw, err = fc.ReadFrame(ctx)
	if err != nil {
		return fail("read finish", err)
	}
	var fin wa6.HandshakeMessage
	if err := proto.Unmarshal(raw, &fin); err != nil {
		return fail("decode finish", err)
	}
	cs, err := ss.decryptAndHash(fin.GetClientFinish().GetStatic())
	if err != nil {
		return fail("decrypt client static", err)
	}
	if err := ss.mixDH(se.Priv, [32]byte(cs)); err != nil {
		return fail("se", err)
	}
	payload, err := ss.decryptAndHash(fin.GetClientFinish().GetPayload())
	if err != nil {
		return fail("decrypt payload", err)
	}
	c2s, s2c, err := ss.split()
	if err != nil {
		return fail("split", err)
	}
	// The responder writes with the second key and reads with the first.
	return serverResult{clientStatic: [32]byte(cs), payload: payload, conn: newNoiseConn(fc, s2c, c2s)}
}

func doHandshake(t *testing.T, pki *testPKI, chain []byte) (*NoiseConn, serverResult, error, *keys.KeyPair) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, server := newPipe()
	noiseKey, _ := keys.NewKeyPair()
	resCh := make(chan serverResult, 1)
	go func() { resCh <- runServer(ctx, server, pki, chain) }()
	conn, err := ClientHandshake(ctx, client, HandshakeConfig{
		Prologue: testPrologue,
		NoiseKey: noiseKey,
		Payload:  []byte("client-payload"),
		Verifier: pki.verifier(),
	})
	if err != nil {
		client.Close() // unblock the server
	}
	return conn, <-resCh, err, noiseKey
}

func TestHandshakeAndTraffic(t *testing.T) {
	pki := newTestPKI(t)
	conn, srv, err, noiseKey := doHandshake(t, pki, pki.chain(t))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if srv.err != nil {
		t.Fatalf("%v", srv.err)
	}
	if srv.clientStatic != noiseKey.Pub {
		t.Fatal("server saw wrong client static key")
	}
	if string(srv.payload) != "client-payload" {
		t.Fatalf("server got payload %q", srv.payload)
	}

	ctx := context.Background()
	// Several frames each way, including an empty and a large one.
	msgs := [][]byte{[]byte("hello"), {}, bytes.Repeat([]byte{0xab}, 200_000), []byte("bye")}
	for _, m := range msgs {
		if err := conn.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, err := srv.conn.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, m) {
			t.Fatalf("c->s mismatch (%d bytes)", len(m))
		}
		if err := srv.conn.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, err = conn.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, m) {
			t.Fatalf("s->c mismatch (%d bytes)", len(m))
		}
	}
}

func TestTamperedFrameIsFatal(t *testing.T) {
	pki := newTestPKI(t)
	conn, srv, err, _ := doHandshake(t, pki, pki.chain(t))
	if err != nil || srv.err != nil {
		t.Fatal(err, srv.err)
	}
	ctx := context.Background()
	srv.conn.fc.(*pipeConn).tamper = func(f []byte) { f[0] ^= 1 }
	if err := srv.conn.Send(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Receive(ctx); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("expected ErrDecrypt, got %v", err)
	}
	if err := conn.Send(ctx, []byte("y")); !errors.Is(err, ErrClosed) {
		t.Fatalf("connection should be closed after decrypt failure, got %v", err)
	}
}

func TestCertificateFailures(t *testing.T) {
	cases := map[string]func(p *testPKI){
		"wrong root": func(p *testPKI) { p.root, _ = keys.NewSigningKey() },
		"leaf key mismatch": func(p *testPKI) {
			other, _ := keys.NewKeyPair()
			p.leafKeyOverride = other.Pub[:]
		},
		"expired":       func(p *testPKI) { p.notAfter = time.Now().Add(-time.Minute) },
		"not yet valid": func(p *testPKI) { p.notBefore = time.Now().Add(time.Hour) },
		"wrong issuer":  func(p *testPKI) { p.interIssuer = 5 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pki := newTestPKI(t)
			verifier := pki.verifier() // trust the original root
			mutate(pki)
			chain := pki.chain(t)
			if err := verifier.Verify(chain, pki.serverStatic.Pub); !errors.Is(err, ErrCertificate) {
				t.Fatalf("expected ErrCertificate, got %v", err)
			}
		})
	}
	t.Run("handshake aborts", func(t *testing.T) {
		pki := newTestPKI(t)
		pki.notAfter = time.Now().Add(-time.Minute)
		_, _, err, _ := doHandshake(t, pki, pki.chain(t))
		var he *HandshakeError
		if !errors.As(err, &he) || he.Step != "verify certificate" || !errors.Is(err, ErrHandshake) {
			t.Fatalf("unexpected error %v", err)
		}
	})
}

func TestFrameSplitter(t *testing.T) {
	var wire []byte
	frames := [][]byte{[]byte("a"), {}, bytes.Repeat([]byte{1}, 70000), []byte("tail")}
	for _, f := range frames {
		wire, _ = appendFrame(wire, nil, f)
	}
	for _, chunk := range []int{1, 2, 3, 5, 1000, len(wire)} {
		var s frameSplitter
		var got [][]byte
		for i := 0; i < len(wire); i += chunk {
			got = append(got, s.push(wire[i:min(i+chunk, len(wire))])...)
		}
		if len(got) != len(frames) {
			t.Fatalf("chunk %d: got %d frames", chunk, len(got))
		}
		for i := range frames {
			if !bytes.Equal(got[i], frames[i]) {
				t.Fatalf("chunk %d: frame %d mismatch", chunk, i)
			}
		}
	}
	if _, err := appendFrame(nil, nil, make([]byte, MaxFrameSize+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatal("expected ErrFrameTooLarge")
	}
}

// TestNonceLayout pins the WhatsApp-specific nonce format.
func TestNonceLayout(t *testing.T) {
	got := nonceFor(0x01020304)
	want := []byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x", got)
	}
}
