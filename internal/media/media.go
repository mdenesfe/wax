// Package media implements WhatsApp's media encryption and the HTTP side of
// media transfer (upload to and download from the media servers).
//
// A media file is encrypted with keys expanded from a random 32-byte media
// key that travels inside the (end-to-end encrypted) message:
//
//	HKDF-SHA256(mediaKey, salt=nil, info="WhatsApp <Type> Keys", 112)
//	  → iv[16] | cipherKey[32] | macKey[32] | refKey[32]
//	upload = AES-256-CBC(cipherKey, iv, file) || HMAC-SHA256(macKey, iv||ct)[:10]
//
// The message also carries SHA-256 of the plaintext and of the uploaded
// blob; both are verified on download.
package media

import (
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/mdenesfe/wax/internal/cryptoutil"
)

// Type selects the HKDF info string and the upload path.
type Type string

// Media types.
const (
	Image    Type = "WhatsApp Image Keys"
	Video    Type = "WhatsApp Video Keys"
	Audio    Type = "WhatsApp Audio Keys"
	Document Type = "WhatsApp Document Keys"
	Sticker  Type = "WhatsApp Image Keys" // stickers use the image keys
	History  Type = "WhatsApp History Keys"
	AppState Type = "WhatsApp App State Keys"
)

// mmsTypes maps a media type to its path segment on the media servers.
var mmsTypes = map[Type]string{
	Image:    "image",
	Video:    "video",
	Audio:    "audio",
	Document: "document",
	History:  "md-msg-hist",
	AppState: "md-app-state",
}

// MMSType returns the server path segment for t.
func MMSType(t Type) string { return mmsTypes[t] }

const macLength = 10

// Errors.
var (
	ErrInvalidMAC    = errors.New("media: invalid mac")
	ErrHashMismatch  = errors.New("media: sha256 mismatch")
	ErrInvalidLength = errors.New("media: invalid ciphertext length")
)

// Keys are the keys expanded from a media key.
type Keys struct {
	IV        [16]byte
	CipherKey [32]byte
	MACKey    [32]byte
	RefKey    [32]byte
}

// DeriveKeys expands a media key for the given type.
func DeriveKeys(mediaKey []byte, t Type) (Keys, error) {
	if len(mediaKey) != 32 {
		return Keys{}, fmt.Errorf("media: media key is %d bytes", len(mediaKey))
	}
	out := cryptoutil.HKDF(mediaKey, nil, string(t), 112)
	var k Keys
	copy(k.IV[:], out[:16])
	copy(k.CipherKey[:], out[16:48])
	copy(k.MACKey[:], out[48:80])
	copy(k.RefKey[:], out[80:])
	return k, nil
}

// Encrypted is an encrypted file ready for upload plus the metadata the
// message must carry.
type Encrypted struct {
	Data          []byte // ciphertext || mac
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// Encrypt encrypts a file with a fresh media key.
func Encrypt(plaintext []byte, t Type) (*Encrypted, error) {
	mk := make([]byte, 32)
	if _, err := rand.Read(mk); err != nil {
		return nil, err
	}
	k, err := DeriveKeys(mk, t)
	if err != nil {
		return nil, err
	}
	ct, err := cryptoutil.CBCEncrypt(k.CipherKey[:], k.IV[:], plaintext)
	if err != nil {
		return nil, err
	}
	data := append(ct, cryptoutil.HMACSHA256(k.MACKey[:], k.IV[:], ct)[:macLength]...)
	fileHash := sha256.Sum256(plaintext)
	encHash := sha256.Sum256(data)
	return &Encrypted{Data: data, MediaKey: mk, FileSHA256: fileHash[:], FileEncSHA256: encHash[:], FileLength: uint64(len(plaintext))}, nil
}

// Decrypt verifies and decrypts a downloaded blob. fileEncSHA256 and
// fileSHA256 come from the message and are checked when present.
func Decrypt(blob, mediaKey []byte, t Type, fileEncSHA256, fileSHA256 []byte) ([]byte, error) {
	if len(fileEncSHA256) > 0 {
		sum := sha256.Sum256(blob)
		if subtle.ConstantTimeCompare(sum[:], fileEncSHA256) != 1 {
			return nil, fmt.Errorf("%w (encrypted file)", ErrHashMismatch)
		}
	}
	if len(blob) < macLength+aes.BlockSize || (len(blob)-macLength)%aes.BlockSize != 0 {
		return nil, ErrInvalidLength
	}
	k, err := DeriveKeys(mediaKey, t)
	if err != nil {
		return nil, err
	}
	ct, mac := blob[:len(blob)-macLength], blob[len(blob)-macLength:]
	if !hmac.Equal(cryptoutil.HMACSHA256(k.MACKey[:], k.IV[:], ct)[:macLength], mac) {
		return nil, ErrInvalidMAC
	}
	pt, err := cryptoutil.CBCDecrypt(k.CipherKey[:], k.IV[:], ct)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidLength, err)
	}
	if len(fileSHA256) > 0 {
		sum := sha256.Sum256(pt)
		if subtle.ConstantTimeCompare(sum[:], fileSHA256) != 1 {
			return nil, fmt.Errorf("%w (plaintext)", ErrHashMismatch)
		}
	}
	return pt, nil
}
