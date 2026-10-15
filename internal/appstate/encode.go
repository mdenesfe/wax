package appstate

import (
	"crypto/rand"
	"encoding/json"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/cryptoutil"
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
	ct, err := cryptoutil.CBCEncrypt(ks.ValueEncryption, iv, pt)
	if err != nil {
		return nil, err
	}
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
	// The same computation ApplyPatch verifies on the receiving side.
	next, valueMACs, err := advanceHash(state, state.Version+1, muts, l)
	if err != nil {
		return nil, state, err
	}
	p.SnapshotMAC = SnapshotMAC(next, collection, ks.SnapshotMAC)
	p.PatchMAC = PatchMAC(p.SnapshotMAC, valueMACs, next.Version, collection, ks.PatchMAC)
	return p, next, nil
}
