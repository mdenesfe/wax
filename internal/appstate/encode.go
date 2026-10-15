package appstate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/waserversync"
	"github.com/mdenesfe/wax/internal/proto/wasyncaction"
)

// EncodeMutation builds an encrypted, MAC'd mutation record. It is the exact
// inverse of decodeMutation and is used to send app state changes (and by
// tests to build patches).
func EncodeMutation(op waserversync.SyncdMutation_SyncdOperation, index []string, value *wasyncaction.SyncActionValue, version int32, keyID, keyData []byte) (*waserversync.SyncdMutation, error) {
	ks := ExpandKeys(keyData)
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	pt, err := proto.Marshal(&wasyncaction.SyncActionData{Index: indexJSON, Value: value, Padding: []byte{}, Version: proto.Int32(version)})
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(pt)%aes.BlockSize
	for range pad {
		pt = append(pt, byte(pad))
	}
	block, err := aes.NewCipher(ks.ValueEncryption)
	if err != nil {
		return nil, err
	}
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)
	content := append(iv, ct...)
	valueMAC := ContentMAC(int32(op), content, keyID, ks.ValueMAC)
	return &waserversync.SyncdMutation{
		Operation: op.Enum(),
		Record: &waserversync.SyncdRecord{
			Index: &waserversync.SyncdIndex{Blob: IndexMAC(indexJSON, ks.Index)},
			Value: &waserversync.SyncdValue{Blob: append(content, valueMAC...)},
			KeyID: &waserversync.KeyId{ID: keyID},
		},
	}, nil
}

// SealPatch fills in version, snapshot MAC and patch MAC for mutations
// applied on top of state, and returns the new state.
func SealPatch(collection string, state HashState, muts []*waserversync.SyncdMutation, l Lookup, keyID, keyData []byte) (*waserversync.SyncdPatch, HashState, error) {
	p := &waserversync.SyncdPatch{
		Version:   &waserversync.SyncdVersion{Version: proto.Uint64(state.Version + 1)},
		Mutations: muts,
		KeyID:     &waserversync.KeyId{ID: keyID},
	}
	ks := ExpandKeys(keyData)
	// Compute the resulting state with the same rules ApplyPatch checks.
	next := HashState{Version: state.Version + 1, Hash: state.Hash}
	var added, removed, valueMACs [][]byte
	for i, m := range muts {
		vm, err := valueMACOf(m.GetRecord())
		if err != nil {
			return nil, state, err
		}
		valueMACs = append(valueMACs, vm)
		if m.GetOperation() == waserversync.SyncdMutation_SET {
			added = append(added, vm)
		}
		var prev []byte
		found := false
		for j := i - 1; j >= 0; j-- {
			if string(muts[j].GetRecord().GetIndex().GetBlob()) == string(m.GetRecord().GetIndex().GetBlob()) {
				found = true
				if muts[j].GetOperation() == waserversync.SyncdMutation_SET {
					prev = valueMACs[j]
				}
				break
			}
		}
		if !found {
			if prev, err = l.ValueMAC(m.GetRecord().GetIndex().GetBlob()); err != nil {
				return nil, state, fmt.Errorf("previous value mac: %w", err)
			}
		}
		if prev != nil {
			removed = append(removed, prev)
		}
	}
	next.SubtractThenAdd(removed, added)
	p.SnapshotMAC = SnapshotMAC(next, collection, ks.SnapshotMAC)
	p.PatchMAC = PatchMAC(p.SnapshotMAC, valueMACs, next.Version, collection, ks.PatchMAC)
	return p, next, nil
}
