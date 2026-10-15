package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"math/big"
)

// SignatureSize is the length of a Curve25519 (XEdDSA-format) signature.
const SignatureSize = 64

// SigningKey is a Curve25519 key pair that can also produce signatures
// verifiable by libsignal ("Curve25519 signatures", the XEdDSA variant that
// carries the Edwards sign bit in the top bit of the signature).
//
// Why a seed: Go's standard library does not export Edwards25519 group
// operations, and we do not implement curve arithmetic ourselves. Instead the
// key is generated as an Ed25519 seed. Ed25519 derives its secret scalar as
// clamp(SHA-512(seed)[:32]); using those same clamped bytes as the X25519
// private key yields the Montgomery form of the very same point. Signing can
// then be delegated to crypto/ed25519, and the result is a valid signature
// for the Montgomery public key once the Edwards sign bit is embedded in
// sig[63] (S < 2^253, so that bit is otherwise always zero). This is exactly
// the layout libsignal's calculate_signature produces and verify_signature
// consumes.
//
// The consequence is that a SigningKey must be persisted as its seed; a bare
// X25519 private key generated elsewhere cannot be turned into one.
type SigningKey struct {
	KeyPair
	seed [32]byte
	ed   ed25519.PrivateKey
}

// NewSigningKey generates a random signing-capable Curve25519 key.
func NewSigningKey() (*SigningKey, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, fmt.Errorf("keys: read random seed: %w", err)
	}
	return SigningKeyFromSeed(seed[:])
}

// SigningKeyFromSeed deterministically derives a signing key from a 32-byte seed.
func SigningKeyFromSeed(seed []byte) (*SigningKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: seed must be %d bytes, got %d", ErrInvalidKey, ed25519.SeedSize, len(seed))
	}
	sk := &SigningKey{ed: ed25519.NewKeyFromSeed(seed)}
	copy(sk.seed[:], seed)

	// Same derivation as RFC 8032 §5.1.5 steps 1-2; X25519 clamps again
	// internally, which is idempotent.
	h := sha512.Sum512(seed)
	scalar := h[:32]
	scalar[0] &= 248
	scalar[31] &= 127
	scalar[31] |= 64
	kp, err := KeyPairFromPrivate(scalar)
	if err != nil {
		return nil, err
	}
	sk.KeyPair = *kp
	return sk, nil
}

// Seed returns the 32-byte seed the key was derived from. This is the value
// that must be stored (encrypted) to restore the key.
func (sk *SigningKey) Seed() [32]byte { return sk.seed }

// Sign produces a 64-byte Curve25519 signature over msg that verifies against
// sk.Pub with Verify (and with libsignal's verify_signature).
func (sk *SigningKey) Sign(msg []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte
	copy(sig[:], ed25519.Sign(sk.ed, msg))
	edPub := sk.ed.Public().(ed25519.PublicKey)
	sig[63] &= 0x7f
	sig[63] |= edPub[31] & 0x80
	return sig
}

// fieldP is the Curve25519 field prime 2^255 - 19.
var fieldP = func() *big.Int {
	p := new(big.Int).Lsh(big.NewInt(1), 255)
	return p.Sub(p, big.NewInt(19))
}()

// montgomeryToEdwards converts a Montgomery u-coordinate into a compressed
// Edwards25519 point with the given sign bit, using the birational map
// y = (u - 1) / (u + 1) (RFC 7748 §4.1). The inputs are public values, so a
// variable-time big.Int computation is acceptable here.
func montgomeryToEdwards(u [KeySize]byte, signBit byte) (ed25519.PublicKey, bool) {
	// Little-endian to big.Int; the top bit is ignored as in X25519.
	var be [KeySize]byte
	for i := range KeySize {
		be[i] = u[KeySize-1-i]
	}
	be[0] &= 0x7f
	uInt := new(big.Int).SetBytes(be[:])
	uInt.Mod(uInt, fieldP)

	num := new(big.Int).Sub(uInt, big.NewInt(1))
	num.Mod(num, fieldP)
	den := new(big.Int).Add(uInt, big.NewInt(1))
	den.Mod(den, fieldP)
	if den.Sign() == 0 {
		// u = -1 has no Edwards counterpart.
		return nil, false
	}
	inv := new(big.Int).ModInverse(den, fieldP)
	if inv == nil {
		return nil, false
	}
	y := num.Mul(num, inv)
	y.Mod(y, fieldP)

	out := make(ed25519.PublicKey, ed25519.PublicKeySize)
	y.FillBytes(be[:])
	for i := range KeySize {
		out[i] = be[KeySize-1-i]
	}
	out[31] |= signBit << 7
	return out, true
}

// Verify checks a Curve25519 (libsignal XEdDSA-format) signature made by the
// owner of the Montgomery public key pub.
func Verify(pub [KeySize]byte, msg []byte, sig [SignatureSize]byte) bool {
	edPub, ok := montgomeryToEdwards(pub, sig[63]>>7)
	if !ok {
		return false
	}
	sig[63] &= 0x7f // sig is a copy, the caller's array is untouched
	return ed25519.Verify(edPub, msg, sig[:])
}

// VerifySlice is Verify for callers holding slices (e.g. protobuf fields). It
// returns false if either slice has the wrong length.
func VerifySlice(pub, msg, sig []byte) bool {
	if len(pub) != KeySize || len(sig) != SignatureSize {
		return false
	}
	return Verify([KeySize]byte(pub), msg, [SignatureSize]byte(sig))
}

// DJBType is Signal's type byte for Curve25519 public keys. Serialized Signal
// public keys are 0x05 || key.
const DJBType = 0x05

// SignPublicKey signs another public key in Signal's serialized form
// (0x05 || pub), as done for signed prekeys.
func (sk *SigningKey) SignPublicKey(pub [KeySize]byte) [SignatureSize]byte {
	msg := make([]byte, 0, 1+KeySize)
	msg = append(msg, DJBType)
	msg = append(msg, pub[:]...)
	return sk.Sign(msg)
}
