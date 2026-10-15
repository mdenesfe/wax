package transport

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wa6"
)

// DefaultHandshakeTimeout bounds the whole handshake. The server answers
// ClientHello within a few hundred milliseconds in practice.
const DefaultHandshakeTimeout = 20 * time.Second

// HandshakeConfig holds the client side inputs of the Noise handshake.
type HandshakeConfig struct {
	// Prologue is mixed into the handshake hash first. It must be identical
	// to the header bytes the server receives in front of the first frame
	// (see DialOptions.Prologue), otherwise decryption of ServerHello fails.
	Prologue []byte
	// NoiseKey is our long-term static Noise key. It identifies this
	// companion device and must be persisted along with the session.
	NoiseKey *keys.KeyPair
	// Payload is the serialized wa6.ClientPayload (registration or login),
	// delivered encrypted in ClientFinish.
	Payload []byte
	// Verifier validates the server certificate chain. Nil means
	// DefaultCertVerifier.
	Verifier *CertVerifier
	// Timeout for the full exchange. Zero means DefaultHandshakeTimeout.
	Timeout time.Duration
}

// HandshakeError wraps a failure with the handshake step it happened in.
type HandshakeError struct {
	Step string
	Err  error
}

func (e *HandshakeError) Error() string { return fmt.Sprintf("noise handshake: %s: %v", e.Step, e.Err) }

// Unwrap exposes both ErrHandshake and the underlying cause to errors.Is/As.
func (e *HandshakeError) Unwrap() []error { return []error{ErrHandshake, e.Err} }

func hsErr(step string, err error) error { return &HandshakeError{Step: step, Err: err} }

// ClientHandshake runs the initiator side of WhatsApp's Noise XX handshake
// over fc and returns the encrypted connection. On error fc is left open;
// the caller closes it.
//
//	-> e
//	<- e, ee, s, es        (+ encrypted certificate chain)
//	-> s, se               (+ encrypted ClientPayload)
func ClientHandshake(ctx context.Context, fc FrameConn, cfg HandshakeConfig) (*NoiseConn, error) {
	if cfg.NoiseKey == nil {
		return nil, hsErr("config", fmt.Errorf("missing noise key"))
	}
	verifier := cfg.Verifier
	if verifier == nil {
		verifier = DefaultCertVerifier()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	// The timeout is enforced by closing the connection rather than by
	// cancelling the read context: cancelling a coder/websocket read would
	// tear the socket down anyway, and this keeps ReadFrame's contract simple.
	timer := time.AfterFunc(timeout, func() { _ = fc.Close() })
	defer timer.Stop()

	ephemeral, err := keys.NewKeyPair()
	if err != nil {
		return nil, hsErr("ephemeral key", err)
	}
	ss := newSymmetricState(NoiseProtocolName, cfg.Prologue)

	// -> e
	ss.mixHash(ephemeral.Pub[:])
	hello, err := proto.Marshal(&wa6.HandshakeMessage{
		ClientHello: &wa6.HandshakeMessage_ClientHello{Ephemeral: ephemeral.Pub[:]},
	})
	if err != nil {
		return nil, hsErr("marshal client hello", err)
	}
	if err := fc.SendFrame(ctx, hello); err != nil {
		return nil, hsErr("send client hello", err)
	}

	// <- e, ee, s, es
	resp, err := fc.ReadFrame(ctx)
	if err != nil {
		return nil, hsErr("read server hello", err)
	}
	var msg wa6.HandshakeMessage
	if err := proto.Unmarshal(resp, &msg); err != nil {
		return nil, hsErr("decode server hello", err)
	}
	sh := msg.GetServerHello()
	if len(sh.GetEphemeral()) != keys.KeySize || len(sh.GetStatic()) == 0 || len(sh.GetPayload()) == 0 {
		return nil, hsErr("server hello", fmt.Errorf("missing fields (ephemeral=%d static=%d payload=%d bytes)",
			len(sh.GetEphemeral()), len(sh.GetStatic()), len(sh.GetPayload())))
	}
	serverEphemeral := [32]byte(sh.GetEphemeral())
	ss.mixHash(serverEphemeral[:])
	if err := ss.mixDH(ephemeral.Priv, serverEphemeral); err != nil { // ee
		return nil, hsErr("dh ee", err)
	}
	staticPlain, err := ss.decryptAndHash(sh.GetStatic())
	if err != nil {
		return nil, hsErr("decrypt server static", err)
	}
	if len(staticPlain) != keys.KeySize {
		return nil, hsErr("server static", fmt.Errorf("length %d", len(staticPlain)))
	}
	serverStatic := [32]byte(staticPlain)
	if err := ss.mixDH(ephemeral.Priv, serverStatic); err != nil { // es
		return nil, hsErr("dh es", err)
	}
	certPlain, err := ss.decryptAndHash(sh.GetPayload())
	if err != nil {
		return nil, hsErr("decrypt certificate", err)
	}
	if err := verifier.Verify(certPlain, serverStatic); err != nil {
		return nil, hsErr("verify certificate", err)
	}

	// -> s, se
	encStatic := ss.encryptAndHash(cfg.NoiseKey.Pub[:])
	if err := ss.mixDH(cfg.NoiseKey.Priv, serverEphemeral); err != nil { // se
		return nil, hsErr("dh se", err)
	}
	encPayload := ss.encryptAndHash(cfg.Payload)
	finish, err := proto.Marshal(&wa6.HandshakeMessage{
		ClientFinish: &wa6.HandshakeMessage_ClientFinish{Static: encStatic, Payload: encPayload},
	})
	if err != nil {
		return nil, hsErr("marshal client finish", err)
	}
	if err := fc.SendFrame(ctx, finish); err != nil {
		return nil, hsErr("send client finish", err)
	}

	write, read, err := ss.split()
	if err != nil {
		return nil, hsErr("split", err)
	}
	if !timer.Stop() {
		// The timer fired concurrently and closed the connection.
		return nil, hsErr("timeout", ErrClosed)
	}
	return newNoiseConn(fc, write, read), nil
}
