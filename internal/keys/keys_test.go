package keys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Vectors from libsignal's test_signature (rust/core/src/curve/curve25519.rs).
// The signature was produced by libsignal's calculate_signature over a
// type-prefixed (0x05) public key.
const (
	libsignalIdentityPriv = "c0972484" + "12e58bf05df487968205132794178e367637f5818f81e0e6ce73e865"
	libsignalIdentityPub  = "ab7e717d4a163b7d9a1d8071dfe9dcf8cdcd1cea3339b6356be84d887e322c64"
	libsignalMessage      = "05edce9d9c415ca78cb7252e72c2c4a554d3eb29485a0e1d503118d1a82d99fb4a"
	libsignalSignature    = "5de88ca9a89b4a115da79109c67c9c7464a3e4180274f1cb8c63c2984e286dfb" +
		"ede82deb9dcd9fae0bfbb821569b3d9001bd8130cd11d486cef047bd60b86e88"
)

func TestLibsignalVectorPublicKey(t *testing.T) {
	kp, err := KeyPairFromPrivate(mustHex(t, libsignalIdentityPriv))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kp.Pub[:], mustHex(t, libsignalIdentityPub)) {
		t.Fatalf("derived public key mismatch: %x", kp.Pub)
	}
}

func TestLibsignalVectorVerify(t *testing.T) {
	pub := [32]byte(mustHex(t, libsignalIdentityPub))
	msg := mustHex(t, libsignalMessage)
	sig := [64]byte(mustHex(t, libsignalSignature))
	if !Verify(pub, msg, sig) {
		t.Fatal("libsignal test vector did not verify")
	}
	// Every single-bit flip must break the signature (mirrors libsignal's test).
	for i := range sig {
		bad := sig
		bad[i] ^= 0x01
		if Verify(pub, msg, bad) {
			t.Fatalf("signature with byte %d flipped still verified", i)
		}
	}
	badMsg := bytes.Clone(msg)
	badMsg[0] ^= 1
	if Verify(pub, badMsg, sig) {
		t.Fatal("signature verified over a modified message")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	for i := range 64 {
		sk, err := NewSigningKey()
		if err != nil {
			t.Fatal(err)
		}
		msg := []byte{byte(i), 1, 2, 3}
		sig := sk.Sign(msg)
		if !Verify(sk.Pub, msg, sig) {
			t.Fatalf("iteration %d: own signature did not verify", i)
		}
		other, _ := NewSigningKey()
		if Verify(other.Pub, msg, sig) {
			t.Fatalf("iteration %d: signature verified under a different key", i)
		}
	}
}

// TestSigningKeyConsistency checks the core assumption of SigningKey: the
// X25519 public key and the Ed25519 public key describe the same point.
func TestSigningKeyConsistency(t *testing.T) {
	for range 64 {
		sk, err := NewSigningKey()
		if err != nil {
			t.Fatal(err)
		}
		// Independent check: X25519 scalar multiplication of the private key.
		if ref, err := KeyPairFromPrivate(sk.Priv[:]); err != nil || ref.Pub != sk.Pub {
			t.Fatalf("edwards->montgomery public key differs from scalar multiplication")
		}
		edPub := sk.ed.Public().(ed25519.PublicKey)
		conv, ok := montgomeryToEdwards(sk.Pub, edPub[31]>>7)
		if !ok || !bytes.Equal(conv, edPub) {
			t.Fatalf("montgomery->edwards mismatch:\n got %x\nwant %x", conv, edPub)
		}
		seed := sk.Seed()
		again, err := SigningKeyFromSeed(seed[:])
		if err != nil || again.Pub != sk.Pub || again.Priv != sk.Priv {
			t.Fatal("restoring from seed produced a different key")
		}
	}
}

func TestDHAgreement(t *testing.T) {
	a, _ := NewKeyPair()
	b, _ := NewKeyPair()
	s1, err := a.DH(b.Pub)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := b.DH(a.Pub)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("shared secrets differ")
	}
	// Low-order point (all zero) must be rejected.
	if _, err := a.DH([32]byte{}); err == nil {
		t.Fatal("expected error for low-order public key")
	}
}

func TestVerifySliceLengths(t *testing.T) {
	if VerifySlice(make([]byte, 31), nil, make([]byte, 64)) {
		t.Fatal("short key accepted")
	}
	if VerifySlice(make([]byte, 32), nil, make([]byte, 63)) {
		t.Fatal("short signature accepted")
	}
}
