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
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

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

// hkdf32 derives n bytes with HKDF-SHA256. A nil salt means 32 zero bytes,
// matching libsignal's HKDFv3.
func hkdfDerive(ikm, salt []byte, info string, n int) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, info, n)
	if err != nil {
		// Only possible for absurd lengths; n is a constant at every call site.
		panic(fmt.Errorf("signal: hkdf: %w", err))
	}
	return out
}

type rootKey [32]byte

// createChain performs one DH ratchet step: it mixes DH(ours, theirs) into
// the root key and returns the new root key and a fresh chain key.
func (rk rootKey) createChain(theirRatchet [32]byte, ours *keys.KeyPair) (rootKey, chainKey, error) {
	secret, err := keys.DH(ours.Priv, theirRatchet)
	if err != nil {
		return rootKey{}, chainKey{}, fmt.Errorf("%w: ratchet dh: %w", ErrInvalidMessage, err)
	}
	out := hkdfDerive(secret[:], rk[:], infoRatchet, 64)
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
	m := hmac.New(sha256.New, ck.key[:])
	m.Write(seed)
	return m.Sum(nil)
}

func (ck chainKey) next() chainKey {
	var n chainKey
	copy(n.key[:], ck.hmac(chainKeySeed))
	n.index = ck.index + 1
	return n
}

func (ck chainKey) messageKeys() messageKeys {
	out := hkdfDerive(ck.hmac(messageKeySeed), nil, infoMessageKeys, 80)
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
	out := hkdfDerive(masterSecret, nil, infoText, 64)
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

var errPadding = errors.New("signal: bad padding")

func cbcEncrypt(key [32]byte, iv [16]byte, plaintext []byte) []byte {
	block, _ := aes.NewCipher(key[:]) // 32-byte key cannot fail
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	buf := make([]byte, len(plaintext)+pad)
	copy(buf, plaintext)
	for i := len(plaintext); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(buf, buf)
	return buf
}

func cbcDecrypt(key [32]byte, iv [16]byte, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errPadding
	}
	block, _ := aes.NewCipher(key[:])
	out := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(out, ciphertext)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, errPadding
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, errPadding
		}
	}
	return out[:len(out)-pad], nil
}
