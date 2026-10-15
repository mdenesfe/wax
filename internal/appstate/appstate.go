// Package appstate implements WhatsApp's app state sync ("syncd"): the
// encrypted, MAC-chained key-value collections that hold contacts, chat
// settings (mute/pin/archive), the push name and similar account data.
//
// Integrity model:
//   - Every record is encrypted (AES-CBC) and authenticated with a value MAC.
//   - Each collection keeps an LTHash: a 128-byte homomorphic hash of all
//     current value MACs, updated by adding new and subtracting replaced
//     MACs. Its HMAC (the "snapshot MAC") is signed off by the phone with
//     every patch, so a client that misses or misapplies one mutation
//     detects it on the next patch.
//
// The package is pure: callers supply key and previous-MAC lookups and
// persist the returned state, which lets them commit atomically.
package appstate

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"hash"
)

// Collection names.
const (
	CriticalBlock      = "critical_block"
	CriticalUnblockLow = "critical_unblock_low"
	RegularHigh        = "regular_high"
	Regular            = "regular"
	RegularLow         = "regular_low"
)

// AllCollections lists every collection, in sync order.
var AllCollections = []string{CriticalBlock, CriticalUnblockLow, RegularHigh, Regular, RegularLow}

// Errors.
var (
	ErrKeyNotFound       = errors.New("appstate: key not found")
	ErrContentMAC        = errors.New("appstate: mutation value mac mismatch")
	ErrIndexMAC          = errors.New("appstate: mutation index mac mismatch")
	ErrSnapshotMAC       = errors.New("appstate: snapshot mac (lthash) mismatch")
	ErrPatchMAC          = errors.New("appstate: patch mac mismatch")
	ErrMalformedMutation = errors.New("appstate: malformed mutation")
)

// --- LTHash ---------------------------------------------------------------------

const (
	lthashInfo = "WhatsApp Patch Integrity"
	lthashSize = 128
)

// lthashOp adds (or subtracts) the expansion of each item to state as
// little-endian uint16 words with wrap-around.
func lthashOp(state *[lthashSize]byte, items [][]byte, subtract bool) {
	for _, item := range items {
		exp, err := hkdf.Key(sha256.New, item, nil, lthashInfo, lthashSize)
		if err != nil {
			panic(err) // constant parameters; cannot fail
		}
		for i := 0; i < lthashSize; i += 2 {
			x := binary.LittleEndian.Uint16(state[i:])
			y := binary.LittleEndian.Uint16(exp[i:])
			if subtract {
				x -= y
			} else {
				x += y
			}
			binary.LittleEndian.PutUint16(state[i:], x)
		}
	}
}

// HashState is a collection's version and LTHash.
type HashState struct {
	Version uint64
	Hash    [lthashSize]byte
}

// SubtractThenAdd updates the hash: removed value MACs out, added ones in.
func (h *HashState) SubtractThenAdd(removed, added [][]byte) {
	lthashOp(&h.Hash, removed, true)
	lthashOp(&h.Hash, added, false)
}

// --- keys and MACs -----------------------------------------------------------------

// Keys are the expanded keys of one app state sync key.
type Keys struct {
	Index, ValueEncryption, ValueMAC, SnapshotMAC, PatchMAC []byte
}

// ExpandKeys derives the five keys from an app state sync key.
func ExpandKeys(keyData []byte) Keys {
	out, err := hkdf.Key(sha256.New, keyData, nil, "WhatsApp Mutation Keys", 160)
	if err != nil {
		panic(err)
	}
	return Keys{out[0:32], out[32:64], out[64:96], out[96:128], out[128:160]}
}

func u64be(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func hmacOf(alg func() hash.Hash, key []byte, parts ...[]byte) []byte {
	m := hmac.New(alg, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// SnapshotMAC authenticates a collection state.
func SnapshotMAC(h HashState, collection string, key []byte) []byte {
	return hmacOf(sha256.New, key, h.Hash[:], u64be(h.Version), []byte(collection))
}

// PatchMAC authenticates a patch: its snapshot MAC, every value MAC, the
// version and the collection name.
func PatchMAC(snapshotMAC []byte, valueMACs [][]byte, version uint64, collection string, key []byte) []byte {
	parts := make([][]byte, 0, len(valueMACs)+3)
	parts = append(parts, snapshotMAC)
	parts = append(parts, valueMACs...)
	parts = append(parts, u64be(version), []byte(collection))
	return hmacOf(sha256.New, key, parts...)
}

// ContentMAC authenticates one encrypted mutation value. operation is 0
// (SET) or 1 (REMOVE).
func ContentMAC(operation int32, ivAndCiphertext, keyID, key []byte) []byte {
	return hmacOf(sha512.New, key, []byte{byte(operation) + 1}, keyID, ivAndCiphertext, u64be(uint64(len(keyID)+1)))[:32]
}

// IndexMAC authenticates (and blinds) a mutation's JSON index.
func IndexMAC(indexJSON, key []byte) []byte {
	return hmacOf(sha256.New, key, indexJSON)
}
