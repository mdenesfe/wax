// Package cryptoutil holds the few symmetric building blocks shared by the
// protocol packages, so each is implemented (and reviewed) exactly once.
// Everything delegates to the Go standard library.
package cryptoutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
)

// ErrPadding is returned for ciphertexts with invalid length or PKCS#7 padding.
var ErrPadding = errors.New("cryptoutil: invalid ciphertext or padding")

// HKDF derives n bytes with HKDF-SHA256. A nil salt means 32 zero bytes.
// n is always a constant at call sites, so an error is a programming error.
func HKDF(secret, salt []byte, info string, n int) []byte {
	out, err := hkdf.Key(sha256.New, secret, salt, info, n)
	if err != nil {
		panic(fmt.Errorf("cryptoutil: hkdf with length %d: %w", n, err))
	}
	return out
}

// HMACSHA256 returns HMAC-SHA256(key, parts...).
func HMACSHA256(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// NewGCM returns AES-GCM with the standard 12-byte nonce.
func NewGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptoutil: aes: %w", err)
	}
	return cipher.NewGCM(block)
}

// CBCEncrypt encrypts with AES-CBC and PKCS#7 padding. key must be 16, 24
// or 32 bytes and iv 16 bytes.
func CBCEncrypt(key, iv, plaintext []byte) ([]byte, error) {
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("cryptoutil: iv is %d bytes", len(iv))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	out := make([]byte, len(plaintext)+pad)
	copy(out, plaintext)
	for i := len(plaintext); i < len(out); i++ {
		out[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, out)
	return out, nil
}

// CBCDecrypt decrypts AES-CBC and strips PKCS#7 padding.
func CBCDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 || len(iv) != aes.BlockSize {
		return nil, ErrPadding
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ciphertext)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, ErrPadding
	}
	var bad byte
	for _, b := range out[len(out)-pad:] {
		bad |= b ^ byte(pad)
	}
	if bad != 0 {
		return nil, ErrPadding
	}
	return out[:len(out)-pad], nil
}
