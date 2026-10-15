package transport

import (
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/keys"
)

// NoiseProtocolName is the Noise protocol name used by WhatsApp. It is exactly
// 32 bytes (padded with NULs), so per the Noise spec it is used as the
// initial handshake hash directly instead of being hashed.
const NoiseProtocolName = "Noise_XX_25519_AESGCM_SHA256\x00\x00\x00\x00"

// symmetricState is the Noise SymmetricState (Noise spec §5.2) as WhatsApp
// uses it. Deviations from textbook Noise worth knowing:
//   - MixKey derives 64 bytes with HKDF-SHA256(salt=ck, ikm=dh, info="")
//     and splits them into the new chaining key and cipher key. This matches
//     Noise's HKDF(ck, ikm, 2) output.
//   - The nonce is 12 bytes: 8 zero bytes followed by a 32-bit big-endian
//     counter (textbook Noise uses 4 zero bytes and a 64-bit counter).
//   - Reference clients key a cipher with h before the first MixKey. It is
//     never used, because in XX the first encryption follows a DH, so we
//     leave aead nil until mixKey.
type symmetricState struct {
	h     [32]byte
	ck    [32]byte
	aead  cipher.AEAD
	nonce uint32
}

func newSymmetricState(protocolName string, prologue []byte) *symmetricState {
	s := &symmetricState{}
	if len(protocolName) == 32 {
		copy(s.h[:], protocolName)
	} else {
		s.h = sha256.Sum256([]byte(protocolName))
	}
	s.ck = s.h
	s.mixHash(prologue)
	return s
}

func (s *symmetricState) mixHash(data []byte) {
	hh := sha256.New()
	hh.Write(s.h[:])
	hh.Write(data)
	hh.Sum(s.h[:0])
}

func (s *symmetricState) mixKey(ikm []byte) error {
	out := cryptoutil.HKDF(ikm, s.ck[:], "", 64)
	copy(s.ck[:], out[:32])
	var err error
	s.aead, err = cryptoutil.NewGCM(out[32:])
	if err != nil {
		return err
	}
	s.nonce = 0
	return nil
}

// mixDH performs X25519 and mixes the result into the key.
func (s *symmetricState) mixDH(priv, pub [32]byte) error {
	secret, err := keys.DH(priv, pub)
	if err != nil {
		return err
	}
	return s.mixKey(secret[:])
}

func (s *symmetricState) encryptAndHash(plaintext []byte) []byte {
	ct := s.aead.Seal(nil, nonceFor(s.nonce), plaintext, s.h[:])
	s.nonce++
	s.mixHash(ct)
	return ct
}

func (s *symmetricState) decryptAndHash(ciphertext []byte) ([]byte, error) {
	pt, err := s.aead.Open(nil, nonceFor(s.nonce), ciphertext, s.h[:])
	if err != nil {
		return nil, err
	}
	s.nonce++
	s.mixHash(ciphertext)
	return pt, nil
}

// split derives the two transport keys: the first 32 bytes encrypt
// initiator→responder traffic, the second responder→initiator.
func (s *symmetricState) split() (initiatorToResponder, responderToInitiator cipher.AEAD, err error) {
	out := cryptoutil.HKDF(nil, s.ck[:], "", 64)
	if initiatorToResponder, err = cryptoutil.NewGCM(out[:32]); err != nil {
		return nil, nil, err
	}
	if responderToInitiator, err = cryptoutil.NewGCM(out[32:]); err != nil {
		return nil, nil, err
	}
	return initiatorToResponder, responderToInitiator, nil
}

func nonceFor(counter uint32) []byte {
	var n [12]byte
	binary.BigEndian.PutUint32(n[8:], counter)
	return n[:]
}
