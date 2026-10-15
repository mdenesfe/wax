package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// MasterKeySize is the required length of the master key.
const MasterKeySize = 32

// sealVersion prefixes every sealed value. A future key rotation introduces
// version 2 with a different derived key, and open() picks the key by prefix.
const sealVersion = 1

// ErrMasterKey reports a missing or malformed master key.
var ErrMasterKey = errors.New("store: invalid master key")

// ErrSealed reports a sealed value that cannot be opened: wrong master key,
// corruption, or a value moved to another row/column.
var ErrSealed = errors.New("store: cannot decrypt stored secret")

// ParseMasterKey decodes a base64 (standard or URL, padded or not) 32-byte key.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: empty (generate one with: openssl rand -base64 32)", ErrMasterKey)
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil {
			if len(k) != MasterKeySize {
				return nil, fmt.Errorf("%w: decoded to %d bytes, need %d", ErrMasterKey, len(k), MasterKeySize)
			}
			return k, nil
		}
	}
	return nil, fmt.Errorf("%w: not valid base64", ErrMasterKey)
}

// sealer encrypts secrets at rest with AES-256-GCM.
//
// The data key is derived from the master key with HKDF so the master key is
// never used directly. Each value is bound to its location through the GCM
// associated data (table, column, instance, row key): copying a ciphertext
// into another row or column makes it fail to open instead of silently
// handing one instance's key to another.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(masterKey []byte) (*sealer, error) {
	if len(masterKey) != MasterKeySize {
		return nil, fmt.Errorf("%w: %d bytes, need %d", ErrMasterKey, len(masterKey), MasterKeySize)
	}
	dk, err := hkdf.Key(sha256.New, masterKey, nil, "wax/store/secrets/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("store: derive data key: %w", err)
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

// seal returns version || nonce || ciphertext.
func (s *sealer) seal(plaintext []byte, ad string) []byte {
	out := make([]byte, 1+s.aead.NonceSize(), 1+s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	out[0] = sealVersion
	// A random 96-bit nonce is safe here: the number of secrets written under
	// one key stays many orders of magnitude below the 2^32 bound.
	if _, err := rand.Read(out[1:]); err != nil {
		panic(fmt.Errorf("store: crypto/rand failed: %w", err)) // unrecoverable system failure
	}
	return s.aead.Seal(out, out[1:], plaintext, []byte(ad))
}

func (s *sealer) open(blob []byte, ad string) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(blob) < 1+ns+s.aead.Overhead() {
		return nil, fmt.Errorf("%w (%s): too short", ErrSealed, ad)
	}
	if blob[0] != sealVersion {
		return nil, fmt.Errorf("%w (%s): unknown version %d", ErrSealed, ad, blob[0])
	}
	pt, err := s.aead.Open(nil, blob[1:1+ns], blob[1+ns:], []byte(ad))
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", ErrSealed, ad)
	}
	return pt, nil
}

// adFor builds the associated data for a sealed column.
func adFor(table, column, instance string, rowKey ...any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s", table, column, instance)
	for _, k := range rowKey {
		fmt.Fprintf(&b, "|%v", k)
	}
	return b.String()
}
