package transport

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/proto/wacert"
)

// WhatsAppRootKey is the Curve25519 public key of WhatsApp's Noise root
// certificate. Intermediate certificates must be signed by it. Source:
// observed in WhatsApp Web, also used by open-source clients (whatsmeow).
var WhatsAppRootKey = [32]byte{
	0x14, 0x23, 0x75, 0x57, 0x4d, 0x0a, 0x58, 0x71, 0x66, 0xaa, 0xe7, 0x1e, 0xbe, 0x51, 0x64, 0x37,
	0xc4, 0xa2, 0x8b, 0x73, 0xe3, 0x69, 0x5c, 0x6c, 0xe1, 0xf7, 0xf9, 0x54, 0x5d, 0xa8, 0xee, 0x6b,
}

// WhatsAppRootSerial is the serial of the root certificate, i.e. the issuer
// serial every intermediate certificate must name.
const WhatsAppRootSerial = 0

// ErrCertificate is wrapped by every certificate validation failure.
var ErrCertificate = errors.New("transport: invalid server certificate")

// CertVerifier validates the certificate chain presented in ServerHello.
type CertVerifier struct {
	RootKey    [32]byte
	RootSerial uint32
	Now        func() time.Time // defaults to time.Now
}

// DefaultCertVerifier validates against WhatsApp's real root key.
func DefaultCertVerifier() *CertVerifier {
	return &CertVerifier{RootKey: WhatsAppRootKey, RootSerial: WhatsAppRootSerial}
}

// Verify checks root → intermediate → leaf and that the leaf certifies the
// server's static Noise key. Without the last check a man in the middle with
// any valid chain could complete the handshake with its own static key.
func (v *CertVerifier) Verify(rawChain []byte, serverStatic [32]byte) error {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	var chain wacert.CertChain
	if err := proto.Unmarshal(rawChain, &chain); err != nil {
		return fmt.Errorf("%w: decode chain: %w", ErrCertificate, err)
	}
	inter, err := verifyCert(chain.GetIntermediate(), v.RootKey[:], "intermediate")
	if err != nil {
		return err
	}
	if inter.GetIssuerSerial() != v.RootSerial {
		return fmt.Errorf("%w: intermediate issuer serial %d, expected %d", ErrCertificate, inter.GetIssuerSerial(), v.RootSerial)
	}
	if err := checkValidity(inter, now, "intermediate"); err != nil {
		return err
	}
	leaf, err := verifyCert(chain.GetLeaf(), inter.GetKey(), "leaf")
	if err != nil {
		return err
	}
	if leaf.GetIssuerSerial() != inter.GetSerial() {
		return fmt.Errorf("%w: leaf issuer serial %d, intermediate serial %d", ErrCertificate, leaf.GetIssuerSerial(), inter.GetSerial())
	}
	if err := checkValidity(leaf, now, "leaf"); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(leaf.GetKey(), serverStatic[:]) != 1 {
		return fmt.Errorf("%w: leaf key does not match server static key", ErrCertificate)
	}
	return nil
}

// verifyCert checks the signature over the raw details bytes and only then
// decodes them.
func verifyCert(c *wacert.CertChain_NoiseCertificate, issuerKey []byte, name string) (*wacert.CertChain_NoiseCertificate_Details, error) {
	if c == nil || c.Details == nil || c.Signature == nil {
		return nil, fmt.Errorf("%w: %s certificate missing", ErrCertificate, name)
	}
	if !keys.VerifySlice(issuerKey, c.GetDetails(), c.GetSignature()) {
		return nil, fmt.Errorf("%w: %s signature invalid", ErrCertificate, name)
	}
	var d wacert.CertChain_NoiseCertificate_Details
	if err := proto.Unmarshal(c.GetDetails(), &d); err != nil {
		return nil, fmt.Errorf("%w: decode %s details: %w", ErrCertificate, name, err)
	}
	if len(d.GetKey()) != keys.KeySize {
		return nil, fmt.Errorf("%w: %s key is %d bytes", ErrCertificate, name, len(d.GetKey()))
	}
	return &d, nil
}

func checkValidity(d *wacert.CertChain_NoiseCertificate_Details, now time.Time, name string) error {
	// A zero/absent bound is treated as unbounded.
	if nb := d.GetNotBefore(); nb != 0 && now.Before(time.Unix(int64(nb), 0)) {
		return fmt.Errorf("%w: %s not valid before %s", ErrCertificate, name, time.Unix(int64(nb), 0).UTC())
	}
	if na := d.GetNotAfter(); na != 0 && now.After(time.Unix(int64(na), 0)) {
		return fmt.Errorf("%w: %s expired at %s", ErrCertificate, name, time.Unix(int64(na), 0).UTC())
	}
	return nil
}
