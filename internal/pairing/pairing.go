// Package pairing implements QR-code linking of a companion device: building
// QR payloads from the server's refs, and verifying and confirming the
// pair-success the server sends once the phone has scanned the code.
package pairing

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/waadv"
	"github.com/mdenesfe/wax/internal/proto/wacompanionreg"
	"github.com/mdenesfe/wax/internal/wabin"
)

// QRPrefix is prepended to the comma separated QR payload. Phones that scan
// the code with a generic camera are sent to the linked devices screen.
const QRPrefix = "https://wa.me/settings/linked_devices#"

// QR ref lifetimes as used by WhatsApp Web: the first code is shown longer
// to give the user time to open the scanner.
const (
	FirstRefTimeout = 60 * time.Second
	NextRefTimeout  = 20 * time.Second
)

// ClientType is the last QR field; it selects the icon/label on the phone.
type ClientType string

// Known client types.
const (
	ClientUnknown  ClientType = "0"
	ClientChrome   ClientType = "1"
	ClientEdge     ClientType = "2"
	ClientFirefox  ClientType = "3"
	ClientIE       ClientType = "4"
	ClientOpera    ClientType = "5"
	ClientSafari   ClientType = "6"
	ClientElectron ClientType = "7"
	ClientUWP      ClientType = "8"
	ClientOtherWeb ClientType = "9"
)

// ClientTypeFor maps the DeviceProps platform to the QR client type.
func ClientTypeFor(p wacompanionreg.DeviceProps_PlatformType) ClientType {
	switch p {
	case wacompanionreg.DeviceProps_CHROME:
		return ClientChrome
	case wacompanionreg.DeviceProps_FIREFOX:
		return ClientFirefox
	case wacompanionreg.DeviceProps_EDGE:
		return ClientEdge
	case wacompanionreg.DeviceProps_IE:
		return ClientIE
	case wacompanionreg.DeviceProps_OPERA:
		return ClientOpera
	case wacompanionreg.DeviceProps_SAFARI:
		return ClientSafari
	}
	return ClientOtherWeb
}

// QRContent builds the string encoded in the QR code:
//
//	ref,base64(noisePub),base64(identityPub),base64(advSecret),clientType
//
// The phone uses the noise key to recognise our connection, the identity key
// to sign our device identity, and the adv secret to authenticate the
// pair-success payload it sends back through the server.
func QRContent(ref []byte, noisePub, identityPub [32]byte, advSecret [32]byte, ct ClientType) string {
	enc := base64.StdEncoding
	return QRPrefix + strings.Join([]string{
		string(ref),
		enc.EncodeToString(noisePub[:]),
		enc.EncodeToString(identityPub[:]),
		enc.EncodeToString(advSecret[:]),
		string(ct),
	}, ",")
}

// ParsePairDevice extracts the QR refs from a pair-device IQ, in order.
func ParsePairDevice(iq *wabin.Node) ([][]byte, error) {
	pd, ok := iq.Child("pair-device")
	if !ok {
		return nil, fmt.Errorf("%w: no <pair-device>", ErrMalformed)
	}
	var refs [][]byte
	for _, c := range pd.ChildrenByTag("ref") {
		if b, ok := c.Bytes(); ok && len(b) > 0 {
			refs = append(refs, b)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: <pair-device> without refs", ErrMalformed)
	}
	return refs, nil
}

// Success is the content of a pair-success IQ.
type Success struct {
	IQID           string
	JID            wabin.JID // our new device JID (user:device@s.whatsapp.net)
	LID            wabin.JID
	Platform       string // the phone's platform, e.g. "android"
	BusinessName   string
	DeviceIdentity []byte // serialized ADVSignedDeviceIdentityHMAC
	Props          *wacompanionreg.ClientPairingProps
	ServerTime     time.Time
}

// ParsePairSuccess reads an <iq><pair-success>…</pair-success></iq> node.
func ParsePairSuccess(iq *wabin.Node) (*Success, error) {
	ps, ok := iq.Child("pair-success")
	if !ok {
		return nil, fmt.Errorf("%w: no <pair-success>", ErrMalformed)
	}
	s := &Success{}
	s.IQID, _ = iq.AttrString("id")
	if t, ok := iq.AttrInt("t"); ok {
		s.ServerTime = time.Unix(t, 0)
	}
	dev, ok := ps.Child("device")
	if !ok {
		return nil, fmt.Errorf("%w: no <device>", ErrMalformed)
	}
	if s.JID, ok = dev.AttrJID("jid"); !ok || s.JID.User == "" {
		return nil, fmt.Errorf("%w: missing device jid", ErrMalformed)
	}
	s.LID, _ = dev.AttrJID("lid")
	if p, ok := ps.Child("platform"); ok {
		s.Platform, _ = p.AttrString("name")
	}
	if b, ok := ps.Child("biz"); ok {
		s.BusinessName, _ = b.AttrString("name")
	}
	di, ok := ps.Child("device-identity")
	if !ok {
		return nil, fmt.Errorf("%w: no <device-identity>", ErrMalformed)
	}
	if s.DeviceIdentity, ok = di.Bytes(); !ok {
		return nil, fmt.Errorf("%w: <device-identity> has no payload", ErrMalformed)
	}
	if cp, ok := ps.Child("client-props"); ok {
		if raw, ok := cp.Bytes(); ok {
			var props wacompanionreg.ClientPairingProps
			// Optional, informational; ignore decode problems.
			if proto.Unmarshal(raw, &props) == nil {
				s.Props = &props
			}
		}
	}
	return s, nil
}

// Signature prefixes for the ADV signatures. They domain-separate account
// and device signatures (and their hosted-account variants).
var (
	prefixAccountSig       = []byte{6, 0}
	prefixDeviceSig        = []byte{6, 1}
	prefixHostedAccountSig = []byte{6, 5}
)

// Errors returned by Verify. Each maps to the error code we report back to
// the server (see Error.Code).
var (
	ErrMalformed         = errors.New("pairing: malformed pair-success")
	ErrHMACMismatch      = errors.New("pairing: device identity HMAC mismatch")
	ErrSignatureMismatch = errors.New("pairing: account signature invalid")
)

// Result is a verified pairing, ready to persist and confirm.
type Result struct {
	// Account is the signed device identity including our device signature.
	// It is stored and attached to outgoing pkmsg messages later.
	Account []byte
	// PrimaryIdentity is the phone's identity key (the account signature key).
	PrimaryIdentity [32]byte
	KeyIndex        uint32
	// selfSigned is what we send back: Account without accountSignatureKey.
	selfSigned []byte
}

// Verify authenticates a pair-success:
//
//  1. HMAC-SHA256(advSecret, [6,5 if hosted] || details) must match. Only the
//     phone that scanned our QR knows advSecret, so this proves the identity
//     came from it and was not substituted by the server.
//  2. The phone's account key must sign [6,0 or 6,5] || deviceDetails ||
//     ourIdentityPub, binding our identity key to the account.
//  3. We countersign [6,1] || deviceDetails || ourIdentityPub || accountKey
//     with our identity key.
func Verify(s *Success, identity *keys.SigningKey, advSecret [32]byte) (*Result, error) {
	var container waadv.ADVSignedDeviceIdentityHMAC
	if err := proto.Unmarshal(s.DeviceIdentity, &container); err != nil {
		return nil, fmt.Errorf("%w: identity container: %w", ErrMalformed, err)
	}
	var prefix []byte
	if container.GetAccountType() == waadv.ADVEncryptionType_HOSTED {
		prefix = prefixHostedAccountSig
	}
	if !hmac.Equal(cryptoutil.HMACSHA256(advSecret[:], prefix, container.GetDetails()), container.GetHMAC()) {
		return nil, ErrHMACMismatch
	}

	var signed waadv.ADVSignedDeviceIdentity
	if err := proto.Unmarshal(container.GetDetails(), &signed); err != nil {
		return nil, fmt.Errorf("%w: signed identity: %w", ErrMalformed, err)
	}
	var details waadv.ADVDeviceIdentity
	if err := proto.Unmarshal(signed.GetDetails(), &details); err != nil {
		return nil, fmt.Errorf("%w: identity details: %w", ErrMalformed, err)
	}
	accountKey := signed.GetAccountSignatureKey()
	if len(accountKey) != keys.KeySize || len(signed.GetAccountSignature()) != keys.SignatureSize {
		return nil, fmt.Errorf("%w: account key/signature length", ErrMalformed)
	}
	prefix = prefixAccountSig
	if details.GetDeviceType() == waadv.ADVEncryptionType_HOSTED {
		prefix = prefixHostedAccountSig
	}
	if !keys.VerifySlice(accountKey, slices.Concat(prefix, signed.GetDetails(), identity.Pub[:]), signed.GetAccountSignature()) {
		return nil, ErrSignatureMismatch
	}

	devSig := identity.Sign(slices.Concat(prefixDeviceSig, signed.GetDetails(), identity.Pub[:], accountKey))
	signed.DeviceSignature = devSig[:]
	account, err := proto.Marshal(&signed)
	if err != nil {
		return nil, fmt.Errorf("pairing: marshal account: %w", err)
	}
	// The confirmation omits the account key: the server already has it.
	selfSigned := proto.Clone(&signed).(*waadv.ADVSignedDeviceIdentity)
	selfSigned.AccountSignatureKey = nil
	selfRaw, err := proto.Marshal(selfSigned)
	if err != nil {
		return nil, fmt.Errorf("pairing: marshal self-signed identity: %w", err)
	}
	return &Result{
		Account:         account,
		PrimaryIdentity: [32]byte(accountKey),
		KeyIndex:        details.GetKeyIndex(),
		selfSigned:      selfRaw,
	}, nil
}

// ConfirmNode is the IQ result that completes pairing. The server answers it
// by ending the stream with code 515; the client then logs in.
func (r *Result) ConfirmNode(iqID string) wabin.Node {
	return wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "result", "id": iqID},
		Content: []wabin.Node{{
			Tag: "pair-device-sign",
			Content: []wabin.Node{{
				Tag:     "device-identity",
				Attrs:   wabin.Attrs{"key-index": r.KeyIndex},
				Content: r.selfSigned,
			}},
		}},
	}
}

// ErrorNode reports a pairing failure to the server, so the phone shows an
// error instead of waiting.
func ErrorNode(iqID string, err error) wabin.Node {
	code, text := 500, "internal-error"
	switch {
	case errors.Is(err, ErrHMACMismatch):
		code, text = 401, "hmac-mismatch"
	case errors.Is(err, ErrSignatureMismatch):
		code, text = 401, "signature-mismatch"
	}
	return wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "error", "id": iqID},
		Content: []wabin.Node{{
			Tag:   "error",
			Attrs: wabin.Attrs{"code": code, "text": text},
		}},
	}
}
