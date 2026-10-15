// Package signal implements the Signal protocol version 3 as used by
// WhatsApp: X3DH session setup, the Double Ratchet, and the wire formats of
// SignalMessage / PreKeySignalMessage. It is a pure library: state lives in
// Record values that the caller loads and persists, so the caller controls
// transactions.
//
// Behaviour follows libsignal-protocol-java 2.x (the last v3-only release),
// which is what WhatsApp clients are compatible with.
package signal

import (
	"fmt"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/keys"
)

// Protocol constants (libsignal v3).
const (
	currentVersion   = 3
	macLength        = 8
	maxMessageKeys   = 2000 // skipped keys kept per receiver chain
	maxForwardJump   = 2000 // largest accepted counter gap
	maxReceiverChain = 5
	maxArchivedState = 40
)

var (
	infoText        = "WhisperText"
	infoRatchet     = "WhisperRatchet"
	infoMessageKeys = "WhisperMessageKeys"
	messageKeySeed  = []byte{0x01}
	chainKeySeed    = []byte{0x02}
)

type rootKey [32]byte

// createChain performs one DH ratchet step: it mixes DH(ours, theirs) into
// the root key and returns the new root key and a fresh chain key.
func (rk rootKey) createChain(theirRatchet [32]byte, ours *keys.KeyPair) (rootKey, chainKey, error) {
	secret, err := keys.DH(ours.Priv, theirRatchet)
	if err != nil {
		return rootKey{}, chainKey{}, fmt.Errorf("%w: ratchet dh: %w", ErrInvalidMessage, err)
	}
	out := cryptoutil.HKDF(secret[:], rk[:], infoRatchet, 64)
	var nrk rootKey
	var ck chainKey
	copy(nrk[:], out[:32])
	copy(ck.key[:], out[32:])
	return nrk, ck, nil
}

type chainKey struct {
	key   [32]byte
	index uint32
}

func (ck chainKey) hmac(seed []byte) []byte {
	return cryptoutil.HMACSHA256(ck.key[:], seed)
}

func (ck chainKey) next() chainKey {
	var n chainKey
	copy(n.key[:], ck.hmac(chainKeySeed))
	n.index = ck.index + 1
	return n
}

func (ck chainKey) messageKeys() messageKeys {
	out := cryptoutil.HKDF(ck.hmac(messageKeySeed), nil, infoMessageKeys, 80)
	var mk messageKeys
	copy(mk.cipherKey[:], out[:32])
	copy(mk.macKey[:], out[32:64])
	copy(mk.iv[:], out[64:])
	mk.index = ck.index
	return mk
}

type messageKeys struct {
	cipherKey [32]byte
	macKey    [32]byte
	iv        [16]byte
	index     uint32
}

// deriveInitialKeys turns the X3DH master secret into the first root and
// chain keys.
func deriveInitialKeys(masterSecret []byte) (rootKey, chainKey) {
	out := cryptoutil.HKDF(masterSecret, nil, infoText, 64)
	var rk rootKey
	var ck chainKey
	copy(rk[:], out[:32])
	copy(ck.key[:], out[32:])
	return rk, ck
}

// discontinuity prefixes the X3DH master secret (32 bytes of 0xFF), a
// domain separator inherited from the XEdDSA/X3DH specification.
var discontinuity = func() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0xff
	}
	return b
}()

// AES-256-CBC with PKCS#7 padding, as used for message bodies.

func cbcEncrypt(key [32]byte, iv [16]byte, plaintext []byte) []byte {
	out, err := cryptoutil.CBCEncrypt(key[:], iv[:], plaintext)
	if err != nil {
		panic(err) // fixed key and IV sizes; cannot fail
	}
	return out
}

func cbcDecrypt(key [32]byte, iv [16]byte, ciphertext []byte) ([]byte, error) {
	return cryptoutil.CBCDecrypt(key[:], iv[:], ciphertext)
}
