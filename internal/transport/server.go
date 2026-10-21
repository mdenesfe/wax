package transport

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/proto/wa6"
)

// ServerHandshakeConfig configures the responder side of the handshake.
type ServerHandshakeConfig struct {
	Prologue  []byte
	StaticKey *keys.KeyPair
	CertChain []byte // serialized wacert.CertChain certifying StaticKey
}

// ServerHandshakeResult is what the responder learns from the client.
type ServerHandshakeResult struct {
	Conn         *NoiseConn
	ClientStatic [32]byte
	Payload      []byte // decrypted, serialized ClientPayload
}

// ServerHandshake runs the responder side of WhatsApp's Noise XX handshake.
// It exists for tests and local simulations of the WhatsApp server; real
// deployments only ever use ClientHandshake.
func ServerHandshake(ctx context.Context, fc FrameConn, cfg ServerHandshakeConfig) (*ServerHandshakeResult, error) {
	ss := newSymmetricState(NoiseProtocolName, cfg.Prologue)
	raw, err := fc.ReadFrame(ctx)
	if err != nil {
		return nil, hsErr("read client hello", err)
	}
	var hello wa6.HandshakeMessage
	if err := proto.Unmarshal(raw, &hello); err != nil {
		return nil, hsErr("decode client hello", err)
	}
	ceRaw := hello.GetClientHello().GetEphemeral()
	if len(ceRaw) != keys.KeySize {
		return nil, hsErr("client hello", fmt.Errorf("ephemeral is %d bytes", len(ceRaw)))
	}
	ce := [32]byte(ceRaw)
	ss.mixHash(ce[:])

	se, err := keys.NewKeyPair()
	if err != nil {
		return nil, hsErr("ephemeral key", err)
	}
	ss.mixHash(se.Pub[:])
	if err := ss.mixDH(se.Priv, ce); err != nil { // ee
		return nil, hsErr("dh ee", err)
	}
	encStatic := ss.encryptAndHash(cfg.StaticKey.Pub[:])
	if err := ss.mixDH(cfg.StaticKey.Priv, ce); err != nil { // es
		return nil, hsErr("dh es", err)
	}
	encCert := ss.encryptAndHash(cfg.CertChain)
	out, err := proto.Marshal(&wa6.HandshakeMessage{ServerHello: &wa6.HandshakeMessage_ServerHello{
		Ephemeral: se.Pub[:], Static: encStatic, Payload: encCert,
	}})
	if err != nil {
		return nil, hsErr("marshal server hello", err)
	}
	if err := fc.SendFrame(ctx, out); err != nil {
		return nil, hsErr("send server hello", err)
	}

	raw, err = fc.ReadFrame(ctx)
	if err != nil {
		return nil, hsErr("read client finish", err)
	}
	var fin wa6.HandshakeMessage
	if err := proto.Unmarshal(raw, &fin); err != nil {
		return nil, hsErr("decode client finish", err)
	}
	cs, err := ss.decryptAndHash(fin.GetClientFinish().GetStatic())
	if err != nil {
		return nil, hsErr("decrypt client static", err)
	}
	if len(cs) != keys.KeySize {
		return nil, hsErr("client static", fmt.Errorf("%d bytes", len(cs)))
	}
	if err := ss.mixDH(se.Priv, [32]byte(cs)); err != nil { // se
		return nil, hsErr("dh se", err)
	}
	payload, err := ss.decryptAndHash(fin.GetClientFinish().GetPayload())
	if err != nil {
		return nil, hsErr("decrypt payload", err)
	}
	c2s, s2c, err := ss.split()
	if err != nil {
		return nil, hsErr("split", err)
	}
	// The responder writes with the second key and reads with the first.
	return &ServerHandshakeResult{Conn: newNoiseConn(fc, s2c, c2s), ClientStatic: [32]byte(cs), Payload: payload}, nil
}
