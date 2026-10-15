// Package keys provides the Curve25519 key types used throughout the
// protocol: plain X25519 key pairs for Diffie-Hellman (Noise, Signal
// ephemerals, prekeys) and signing-capable identity keys whose signatures are
// compatible with libsignal's Curve25519 signature format.
//
// No cryptographic primitive is implemented here; everything delegates to the
// Go standard library (crypto/ecdh, crypto/ed25519, crypto/sha512).
package keys

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the length of Curve25519 public keys, private keys and shared secrets.
const KeySize = 32

// ErrInvalidKey is returned when a key has the wrong length or is otherwise unusable.
var ErrInvalidKey = errors.New("keys: invalid key")

// KeyPair is an X25519 key pair.
type KeyPair struct {
	Priv [KeySize]byte
	Pub  [KeySize]byte
}

// NewKeyPair generates a random X25519 key pair.
func NewKeyPair() (*KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("keys: generate x25519 key: %w", err)
	}
	return keyPairFromECDH(priv), nil
}

// KeyPairFromPrivate reconstructs a key pair from its 32-byte private key.
func KeyPairFromPrivate(priv []byte) (*KeyPair, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	return keyPairFromECDH(k), nil
}

func keyPairFromECDH(k *ecdh.PrivateKey) *KeyPair {
	var kp KeyPair
	copy(kp.Priv[:], k.Bytes())
	copy(kp.Pub[:], k.PublicKey().Bytes())
	return &kp
}

// DH computes the X25519 shared secret between this key pair's private key and
// the given public key. It fails for low-order public keys (all-zero output),
// which protects every caller against small-subgroup attacks.
func (kp *KeyPair) DH(pub [KeySize]byte) ([KeySize]byte, error) {
	return DH(kp.Priv, pub)
}

// DH computes the X25519 shared secret of priv and pub.
func DH(priv, pub [KeySize]byte) (out [KeySize]byte, err error) {
	sk, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		return out, fmt.Errorf("%w: private: %w", ErrInvalidKey, err)
	}
	pk, err := ecdh.X25519().NewPublicKey(pub[:])
	if err != nil {
		return out, fmt.Errorf("%w: public: %w", ErrInvalidKey, err)
	}
	secret, err := sk.ECDH(pk)
	if err != nil {
		return out, fmt.Errorf("keys: x25519: %w", err)
	}
	copy(out[:], secret)
	return out, nil
}
