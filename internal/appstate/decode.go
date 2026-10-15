package appstate

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/waserversync"
	"github.com/mdenesfe/wax/internal/proto/wasyncaction"
)

// Mutation is a decrypted, verified app state change.
type Mutation struct {
	Operation    waserversync.SyncdMutation_SyncdOperation
	Index        []string // e.g. ["mute", "<jid>"]
	Action       *wasyncaction.SyncActionValue
	Version      int32
	IndexMAC     []byte
	ValueMAC     []byte
	PatchVersion uint64
}

// MACChange is a value MAC to store (or, with nil ValueMAC, to delete) for
// an index MAC.
type MACChange struct {
	IndexMAC []byte
	ValueMAC []byte // nil = removed
}

// Lookup provides keys and the stored value MACs.
type Lookup interface {
	// Key returns the raw app state sync key for a key id, or ErrKeyNotFound.
	Key(keyID []byte) ([]byte, error)
	// ValueMAC returns the stored value MAC for an index MAC, or nil.
	ValueMAC(indexMAC []byte) ([]byte, error)
}

// Result is the outcome of applying a snapshot or patch.
type Result struct {
	State     HashState
	Mutations []Mutation
	MACs      []MACChange // in order; later entries win
}

type keyCache struct {
	l     Lookup
	cache map[string]Keys
}

func (k *keyCache) get(id []byte) (Keys, error) {
	if ks, ok := k.cache[string(id)]; ok {
		return ks, nil
	}
	raw, err := k.l.Key(id)
	if err != nil {
		return Keys{}, fmt.Errorf("key %x: %w", id, err)
	}
	ks := ExpandKeys(raw)
	if k.cache == nil {
		k.cache = map[string]Keys{}
	}
	k.cache[string(id)] = ks
	return ks, nil
}

func valueMACOf(record *waserversync.SyncdRecord) ([]byte, error) {
	blob := record.GetValue().GetBlob()
	if len(blob) < 32+16+16 {
		return nil, fmt.Errorf("%w: value blob of %d bytes", ErrMalformedMutation, len(blob))
	}
	return blob[len(blob)-32:], nil
}

// decodeMutation verifies and decrypts one mutation.
func decodeMutation(kc *keyCache, op waserversync.SyncdMutation_SyncdOperation, record *waserversync.SyncdRecord, patchVersion uint64) (*Mutation, error) {
	keyID := record.GetKeyID().GetID()
	ks, err := kc.get(keyID)
	if err != nil {
		return nil, err
	}
	blob := record.GetValue().GetBlob()
	valueMAC, err := valueMACOf(record)
	if err != nil {
		return nil, err
	}
	content := blob[:len(blob)-32]
	if !hmac.Equal(ContentMAC(int32(op), content, keyID, ks.ValueMAC), valueMAC) {
		return nil, ErrContentMAC
	}
	iv, ct := content[:16], content[16:]
	if len(ct)%aes.BlockSize != 0 || len(ct) == 0 {
		return nil, fmt.Errorf("%w: ciphertext length %d", ErrMalformedMutation, len(ct))
	}
	block, err := aes.NewCipher(ks.ValueEncryption)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.Equal(pt[len(pt)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, fmt.Errorf("%w: bad padding", ErrMalformedMutation)
	}
	var data wasyncaction.SyncActionData
	if err := proto.Unmarshal(pt[:len(pt)-pad], &data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedMutation, err)
	}
	indexMAC := record.GetIndex().GetBlob()
	if !hmac.Equal(IndexMAC(data.GetIndex(), ks.Index), indexMAC) {
		return nil, ErrIndexMAC
	}
	var index []string
	if err := json.Unmarshal(data.GetIndex(), &index); err != nil || len(index) == 0 {
		return nil, fmt.Errorf("%w: index %q", ErrMalformedMutation, data.GetIndex())
	}
	return &Mutation{
		Operation: op, Index: index, Action: data.GetValue(), Version: data.GetVersion(),
		IndexMAC: indexMAC, ValueMAC: valueMAC, PatchVersion: patchVersion,
	}, nil
}

// ApplySnapshot verifies and decodes a full snapshot (the state before is
// empty).
func ApplySnapshot(collection string, ss *waserversync.SyncdSnapshot, l Lookup) (*Result, error) {
	kc := &keyCache{l: l}
	res := &Result{State: HashState{Version: ss.GetVersion().GetVersion()}}
	var added [][]byte
	for _, r := range ss.GetRecords() {
		vm, err := valueMACOf(r)
		if err != nil {
			return nil, err
		}
		added = append(added, vm)
	}
	res.State.SubtractThenAdd(nil, added)
	ks, err := kc.get(ss.GetKeyID().GetID())
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(SnapshotMAC(res.State, collection, ks.SnapshotMAC), ss.GetMac()) {
		return nil, fmt.Errorf("%w: %s snapshot v%d", ErrSnapshotMAC, collection, res.State.Version)
	}
	for i, r := range ss.GetRecords() {
		m, err := decodeMutation(kc, waserversync.SyncdMutation_SET, r, res.State.Version)
		if err != nil {
			return nil, fmt.Errorf("snapshot record %d: %w", i, err)
		}
		res.Mutations = append(res.Mutations, *m)
		res.MACs = append(res.MACs, MACChange{IndexMAC: m.IndexMAC, ValueMAC: m.ValueMAC})
	}
	return res, nil
}

// ApplyPatch verifies and decodes one patch on top of state.
func ApplyPatch(collection string, state HashState, p *waserversync.SyncdPatch, l Lookup) (*Result, error) {
	kc := &keyCache{l: l}
	res := &Result{State: HashState{Version: p.GetVersion().GetVersion(), Hash: state.Hash}}
	muts := p.GetMutations()
	var added, removed, valueMACs [][]byte
	for i, m := range muts {
		vm, err := valueMACOf(m.GetRecord())
		if err != nil {
			return nil, fmt.Errorf("mutation %d: %w", i, err)
		}
		valueMACs = append(valueMACs, vm)
		if m.GetOperation() == waserversync.SyncdMutation_SET {
			added = append(added, vm)
		}
		// Whatever value this index had before (earlier in this patch, or
		// stored) leaves the hash.
		indexMAC := m.GetRecord().GetIndex().GetBlob()
		prev, found := []byte(nil), false
		for j := i - 1; j >= 0; j-- {
			if hmac.Equal(muts[j].GetRecord().GetIndex().GetBlob(), indexMAC) {
				found = true
				if muts[j].GetOperation() == waserversync.SyncdMutation_SET {
					prev = valueMACs[j]
				}
				break
			}
		}
		if !found {
			if prev, err = l.ValueMAC(indexMAC); err != nil {
				return nil, err
			}
		}
		if prev != nil {
			removed = append(removed, prev)
		}
	}
	res.State.SubtractThenAdd(removed, added)
	ks, err := kc.get(p.GetKeyID().GetID())
	if err != nil {
		return nil, err
	}
	snap := SnapshotMAC(res.State, collection, ks.SnapshotMAC)
	if !hmac.Equal(snap, p.GetSnapshotMAC()) {
		return nil, fmt.Errorf("%w: %s patch v%d", ErrSnapshotMAC, collection, res.State.Version)
	}
	if !hmac.Equal(PatchMAC(p.GetSnapshotMAC(), valueMACs, res.State.Version, collection, ks.PatchMAC), p.GetPatchMAC()) {
		return nil, fmt.Errorf("%w: %s patch v%d", ErrPatchMAC, collection, res.State.Version)
	}
	for i, m := range muts {
		dm, err := decodeMutation(kc, m.GetOperation(), m.GetRecord(), res.State.Version)
		if err != nil {
			return nil, fmt.Errorf("patch v%d mutation %d: %w", res.State.Version, i, err)
		}
		res.Mutations = append(res.Mutations, *dm)
		if dm.Operation == waserversync.SyncdMutation_SET {
			res.MACs = append(res.MACs, MACChange{IndexMAC: dm.IndexMAC, ValueMAC: dm.ValueMAC})
		} else {
			res.MACs = append(res.MACs, MACChange{IndexMAC: dm.IndexMAC})
		}
	}
	return res, nil
}

// ApplyRecovery installs a collection received from the primary device in
// a fatal-recovery response. The records arrive in plaintext over the
// end-to-end encrypted channel from our own phone, so there is nothing to
// verify against the server: the phone's LTHash and version are adopted.
func ApplyRecovery(rec *wasyncaction.SyncdSnapshotRecovery, l Lookup) (*Result, error) {
	if len(rec.GetCollectionLthash()) != lthashSize {
		return nil, fmt.Errorf("%w: recovery lthash of %d bytes", ErrMalformedMutation, len(rec.GetCollectionLthash()))
	}
	kc := &keyCache{l: l}
	res := &Result{State: HashState{Version: rec.GetVersion().GetVersion()}}
	copy(res.State.Hash[:], rec.GetCollectionLthash())
	for i, r := range rec.GetMutationRecords() {
		ks, err := kc.get(r.GetKeyID())
		if err != nil {
			return nil, fmt.Errorf("recovery record %d: %w", i, err)
		}
		var index []string
		if err := json.Unmarshal(r.GetValue().GetIndex(), &index); err != nil || len(index) == 0 {
			return nil, fmt.Errorf("%w: recovery record %d index", ErrMalformedMutation, i)
		}
		indexMAC := IndexMAC(r.GetValue().GetIndex(), ks.Index)
		res.MACs = append(res.MACs, MACChange{IndexMAC: indexMAC, ValueMAC: r.GetMac()})
		res.Mutations = append(res.Mutations, Mutation{
			Operation: waserversync.SyncdMutation_SET, Index: index, Action: r.GetValue().GetValue(),
			Version: r.GetValue().GetVersion(), IndexMAC: indexMAC, ValueMAC: r.GetMac(), PatchVersion: res.State.Version,
		})
	}
	return res, nil
}
