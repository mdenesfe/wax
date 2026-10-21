package signal

import (
	"crypto/hmac"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/proto/signalwire"
)

// Errors returned by the protocol.
var (
	ErrInvalidMessage   = errors.New("signal: invalid message")
	ErrInvalidMAC       = errors.New("signal: bad mac")
	ErrDuplicateMessage = errors.New("signal: duplicate message (old counter)")
	ErrNoSession        = errors.New("signal: no session")
	ErrInvalidSignature = errors.New("signal: invalid signed prekey signature")
	ErrMissingPreKey    = errors.New("signal: prekey not found")
	ErrLegacyVersion    = errors.New("signal: unsupported message version")
)

// MessageType distinguishes the two kinds of 1:1 ciphertext.
type MessageType int

const (
	// TypeMessage is a SignalMessage ("msg"): requires an existing session.
	TypeMessage MessageType = 2
	// TypePreKey is a PreKeySignalMessage ("pkmsg"): carries what the
	// recipient needs to establish the session.
	TypePreKey MessageType = 3
)

// versionByte packs the message version in the high nibble and the current
// version in the low nibble: 0x33 for v3.
const versionByte = currentVersion<<4 | currentVersion

// encodeKey serializes a public key the Signal way: 0x05 || key.
func encodeKey(k [32]byte) []byte {
	out := make([]byte, 33)
	out[0] = keys.DJBType
	copy(out[1:], k[:])
	return out
}

// decodeKey parses a serialized public key. The 32-byte form without type
// prefix is accepted too, as WhatsApp sends raw keys in some places.
func decodeKey(b []byte) ([32]byte, error) {
	switch {
	case len(b) == 33 && b[0] == keys.DJBType:
		return [32]byte(b[1:]), nil
	case len(b) == 32:
		return [32]byte(b), nil
	}
	return [32]byte{}, fmt.Errorf("%w: bad public key (%d bytes)", ErrInvalidMessage, len(b))
}

// signalMessage is a parsed SignalMessage.
type signalMessage struct {
	version         byte
	ratchetKey      [32]byte
	counter         uint32
	previousCounter uint32
	ciphertext      []byte
	serialized      []byte // version || proto || mac
}

func parseSignalMessage(b []byte) (*signalMessage, error) {
	if len(b) < 1+macLength {
		return nil, fmt.Errorf("%w: message too short", ErrInvalidMessage)
	}
	v := b[0] >> 4
	if v < currentVersion {
		return nil, fmt.Errorf("%w: %d", ErrLegacyVersion, v)
	}
	if v > currentVersion {
		return nil, fmt.Errorf("%w: unknown version %d", ErrInvalidMessage, v)
	}
	var pb signalwire.SignalMessage
	if err := proto.Unmarshal(b[1:len(b)-macLength], &pb); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if pb.Ciphertext == nil || pb.Counter == nil || pb.RatchetKey == nil {
		return nil, fmt.Errorf("%w: incomplete message", ErrInvalidMessage)
	}
	rk, err := decodeKey(pb.GetRatchetKey())
	if err != nil {
		return nil, err
	}
	return &signalMessage{
		version: v, ratchetKey: rk, counter: pb.GetCounter(), previousCounter: pb.GetPreviousCounter(),
		ciphertext: pb.GetCiphertext(), serialized: b,
	}, nil
}

// macFor computes the truncated message MAC. The identities bind the
// message to this pair of participants.
func macFor(macKey [32]byte, senderIdentity, receiverIdentity [32]byte, data []byte) []byte {
	return cryptoutil.HMACSHA256(macKey[:], encodeKey(senderIdentity), encodeKey(receiverIdentity), data)[:macLength]
}

func (m *signalMessage) verifyMAC(senderIdentity, receiverIdentity [32]byte, macKey [32]byte) error {
	body := m.serialized[:len(m.serialized)-macLength]
	want := macFor(macKey, senderIdentity, receiverIdentity, body)
	if !hmac.Equal(want, m.serialized[len(m.serialized)-macLength:]) {
		return ErrInvalidMAC
	}
	return nil
}

func buildSignalMessage(macKey [32]byte, ratchetKey [32]byte, counter, previousCounter uint32, ciphertext []byte, senderIdentity, receiverIdentity [32]byte) ([]byte, error) {
	body, err := proto.Marshal(&signalwire.SignalMessage{
		RatchetKey:      encodeKey(ratchetKey),
		Counter:         proto.Uint32(counter),
		PreviousCounter: proto.Uint32(previousCounter),
		Ciphertext:      ciphertext,
	})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(body)+macLength)
	out = append(out, versionByte)
	out = append(out, body...)
	return append(out, macFor(macKey, senderIdentity, receiverIdentity, out)...), nil
}

// preKeyMessage is a parsed PreKeySignalMessage.
type preKeyMessage struct {
	version        byte
	registrationID uint32
	preKeyID       *uint32
	signedPreKeyID uint32
	baseKey        [32]byte
	identityKey    [32]byte
	message        *signalMessage
}

func parsePreKeyMessage(b []byte) (*preKeyMessage, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("%w: prekey message too short", ErrInvalidMessage)
	}
	v := b[0] >> 4
	if v < currentVersion {
		return nil, fmt.Errorf("%w: %d", ErrLegacyVersion, v)
	}
	if v > currentVersion {
		return nil, fmt.Errorf("%w: unknown version %d", ErrInvalidMessage, v)
	}
	var pb signalwire.PreKeySignalMessage
	if err := proto.Unmarshal(b[1:], &pb); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if pb.SignedPreKeyId == nil || pb.BaseKey == nil || pb.IdentityKey == nil || pb.Message == nil {
		return nil, fmt.Errorf("%w: incomplete prekey message", ErrInvalidMessage)
	}
	base, err := decodeKey(pb.GetBaseKey())
	if err != nil {
		return nil, err
	}
	ident, err := decodeKey(pb.GetIdentityKey())
	if err != nil {
		return nil, err
	}
	inner, err := parseSignalMessage(pb.GetMessage())
	if err != nil {
		return nil, err
	}
	return &preKeyMessage{
		version: v, registrationID: pb.GetRegistrationId(), preKeyID: pb.PreKeyId,
		signedPreKeyID: pb.GetSignedPreKeyId(), baseKey: base, identityKey: ident, message: inner,
	}, nil
}

func buildPreKeyMessage(regID uint32, pending *pendingPreKey, identity [32]byte, inner []byte) ([]byte, error) {
	pb := &signalwire.PreKeySignalMessage{
		RegistrationId: proto.Uint32(regID),
		SignedPreKeyId: proto.Uint32(pending.signedPreKeyID),
		BaseKey:        encodeKey(pending.baseKey),
		IdentityKey:    encodeKey(identity),
		Message:        inner,
	}
	if pending.preKeyID != nil {
		pb.PreKeyId = proto.Uint32(*pending.preKeyID)
	}
	body, err := proto.Marshal(pb)
	if err != nil {
		return nil, err
	}
	return append([]byte{versionByte}, body...), nil
}
