package cryptoutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func TestCBCRoundTrip(t *testing.T) {
	key, iv := make([]byte, 32), make([]byte, 16)
	rand.Read(key)
	rand.Read(iv)
	for n := range 70 {
		pt := bytes.Repeat([]byte{byte(n)}, n)
		ct, err := CBCEncrypt(key, iv, pt)
		if err != nil || len(ct)%16 != 0 || len(ct) <= n {
			t.Fatalf("n=%d: %v", n, err)
		}
		got, err := CBCDecrypt(key, iv, ct)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
	if _, err := CBCDecrypt(key, iv, make([]byte, 15)); !errors.Is(err, ErrPadding) {
		t.Fatal("short ciphertext accepted")
	}
}

// RFC 5869 test case 1.
func TestHKDFVector(t *testing.T) {
	ikm, _ := hex.DecodeString("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	want := "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"
	if got := hex.EncodeToString(HKDF(ikm, salt, string(info), 42)); got != want {
		t.Fatalf("got %s", got)
	}
}
