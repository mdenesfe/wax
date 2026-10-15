package appstate

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/waserversync"
	"github.com/mdenesfe/wax/internal/proto/wasyncaction"
)

// Values below were computed independently (RFC 5869 HKDF written by hand
// with Python's hmac module), not with this package.
func TestKnownAnswers(t *testing.T) {
	ks := ExpandKeys(bytes.Repeat([]byte{0x22}, 32))
	if got := hex.EncodeToString(ks.Index[:8]) + hex.EncodeToString(ks.PatchMAC[24:]); got != wantKeys {
		t.Fatalf("ExpandKeys: %s", got)
	}
	var h HashState
	h.SubtractThenAdd(nil, [][]byte{[]byte("a"), []byte("b")})
	h.SubtractThenAdd([][]byte{[]byte("a")}, nil)
	if got := hex.EncodeToString(h.Hash[:8]) + hex.EncodeToString(h.Hash[120:]); got != wantLTHash {
		t.Fatalf("LTHash: %s", got)
	}
}

const (
	wantKeys   = "c6a9eda8e9867ca85e52cb95cc427f4f" // Index[:8] || PatchMAC[24:]
	wantLTHash = "22da66acd254ce9d89bbe6368b714513" // hash[:8] || hash[120:] after +a +b -a
)

func TestLTHashAlgebra(t *testing.T) {
	var a, b HashState
	a.SubtractThenAdd(nil, [][]byte{[]byte("x"), []byte("y"), []byte("z")})
	b.SubtractThenAdd(nil, [][]byte{[]byte("z"), []byte("x"), []byte("y")})
	if a.Hash != b.Hash {
		t.Fatal("lthash must be order independent")
	}
	a.SubtractThenAdd([][]byte{[]byte("y")}, nil)
	var c HashState
	c.SubtractThenAdd(nil, [][]byte{[]byte("x"), []byte("z")})
	if a.Hash != c.Hash {
		t.Fatal("subtract must undo add")
	}
}

// memLookup is an in-memory key/MAC store.
type memLookup struct {
	keys map[string][]byte
	macs map[string][]byte
}

func (m *memLookup) Key(id []byte) ([]byte, error) {
	k, ok := m.keys[string(id)]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return k, nil
}

func (m *memLookup) ValueMAC(indexMAC []byte) ([]byte, error) { return m.macs[string(indexMAC)], nil }

func (m *memLookup) apply(r *Result) {
	for _, c := range r.MACs {
		if c.ValueMAC == nil {
			delete(m.macs, string(c.IndexMAC))
		} else {
			m.macs[string(c.IndexMAC)] = c.ValueMAC
		}
	}
}

func mute(jid string, muted bool) *wasyncaction.SyncActionValue {
	return &wasyncaction.SyncActionValue{Timestamp: proto.Int64(1700000000000), MuteAction: &wasyncaction.MuteAction{Muted: proto.Bool(muted)}}
}

func TestSnapshotThenPatches(t *testing.T) {
	keyID, keyData := []byte{0, 0, 0, 1}, bytes.Repeat([]byte{7}, 32)
	phone := &memLookup{keys: map[string][]byte{string(keyID): keyData}, macs: map[string][]byte{}}
	client := &memLookup{keys: map[string][]byte{string(keyID): keyData}, macs: map[string][]byte{}}

	// The "phone" builds a snapshot of two records.
	var recs []*waserversync.SyncdRecord
	var added [][]byte
	for _, jid := range []string{"1@s.whatsapp.net", "2@s.whatsapp.net"} {
		m, err := EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", jid}, mute(jid, true), 2, keyID, keyData)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, m.Record)
		vm, _ := valueMACOf(m.Record)
		added = append(added, vm)
		phone.macs[string(m.Record.Index.Blob)] = vm
	}
	state := HashState{Version: 5}
	state.SubtractThenAdd(nil, added)
	ss := &waserversync.SyncdSnapshot{Version: &waserversync.SyncdVersion{Version: proto.Uint64(5)}, Records: recs,
		KeyID: &waserversync.KeyId{ID: keyID}, Mac: SnapshotMAC(state, Regular, ExpandKeys(keyData).SnapshotMAC)}

	res, err := ApplySnapshot(Regular, ss, client)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != state || len(res.Mutations) != 2 || res.Mutations[0].Index[0] != "mute" || !res.Mutations[0].Action.GetMuteAction().GetMuted() {
		t.Fatalf("snapshot result %+v", res.State.Version)
	}
	client.apply(res)

	// Patch: unmute #1 (replaces a stored value) and remove #2.
	m1, _ := EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", "1@s.whatsapp.net"}, mute("1", false), 2, keyID, keyData)
	m2, _ := EncodeMutation(waserversync.SyncdMutation_REMOVE, []string{"mute", "2@s.whatsapp.net"}, mute("2", false), 2, keyID, keyData)
	patch, next, err := SealPatch(Regular, state, []*waserversync.SyncdMutation{m1, m2}, phone, keyID, keyData)
	if err != nil {
		t.Fatal(err)
	}
	pres, err := ApplyPatch(Regular, res.State, patch, client)
	if err != nil {
		t.Fatal(err)
	}
	if pres.State != next || pres.Mutations[0].Action.GetMuteAction().GetMuted() {
		t.Fatal("patch result wrong")
	}
	client.apply(pres)
	if len(client.macs) != 1 {
		t.Fatalf("expected 1 stored mac after removal, got %d", len(client.macs))
	}

	// Tampering or a missed patch is detected.
	bad := proto.Clone(patch).(*waserversync.SyncdPatch)
	bad.PatchMAC[0] ^= 1
	if _, err := ApplyPatch(Regular, res.State, bad, &memLookup{keys: client.keys, macs: map[string][]byte{}}); !errors.Is(err, ErrPatchMAC) {
		// The patch MAC is checked before (and independently of) the lthash.
		t.Fatalf("expected patch mac error, got %v", err)
	}
	if _, err := ApplyPatch(Regular, res.State, bad, phoneWith(phone, res)); !errors.Is(err, ErrPatchMAC) {
		t.Fatalf("expected patch mac error, got %v", err)
	}
	if _, err := ApplyPatch(Regular, HashState{Version: 5}, patch, phoneWith(phone, res)); !errors.Is(err, ErrSnapshotMAC) {
		t.Fatalf("wrong base state must fail, got %v", err)
	}
	if _, err := ApplyPatch(Regular, res.State, patch, &memLookup{keys: map[string][]byte{}, macs: client.macs}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	evil := proto.Clone(m1).(*waserversync.SyncdMutation)
	evil.Record.Value.Blob[20] ^= 1
	if _, err := decodeMutation(&keyCache{l: client}, evil.GetOperation(), evil.Record, 1); !errors.Is(err, ErrContentMAC) {
		t.Fatalf("tampered value: %v", err)
	}
}

// phoneWith returns a lookup holding the MACs as they were after res.
func phoneWith(p *memLookup, res *Result) *memLookup {
	m := &memLookup{keys: p.keys, macs: map[string][]byte{}}
	m.apply(res)
	return m
}

// TestUnverifiedSnapshot models a collection whose LTHash covers a record
// that is never delivered: the snapshot is accepted as unverified, and
// later patches are accepted on their patch MAC while tampering is still
// rejected.
func TestUnverifiedSnapshot(t *testing.T) {
	keyID, keyData := []byte{0, 0, 0, 1}, bytes.Repeat([]byte{7}, 32)
	phone := &memLookup{keys: map[string][]byte{string(keyID): keyData}, macs: map[string][]byte{}}
	client := &memLookup{keys: map[string][]byte{string(keyID): keyData}, macs: map[string][]byte{}}

	var recs []*waserversync.SyncdRecord
	var all [][]byte
	for _, jid := range []string{"1@s.whatsapp.net", "2@s.whatsapp.net", "hidden@s.whatsapp.net"} {
		m, err := EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", jid}, mute(jid, true), 2, keyID, keyData)
		if err != nil {
			t.Fatal(err)
		}
		vm, _ := valueMACOf(m.Record)
		all = append(all, vm)
		phone.macs[string(m.Record.Index.Blob)] = vm
		if jid != "hidden@s.whatsapp.net" {
			recs = append(recs, m.Record)
		}
	}
	phoneState := HashState{Version: 7}
	phoneState.SubtractThenAdd(nil, all)
	ss := &waserversync.SyncdSnapshot{Version: &waserversync.SyncdVersion{Version: proto.Uint64(7)}, Records: recs,
		KeyID: &waserversync.KeyId{ID: keyID}, Mac: SnapshotMAC(phoneState, Regular, ExpandKeys(keyData).SnapshotMAC)}

	res, err := ApplySnapshot(Regular, ss, client)
	if err != nil {
		t.Fatal(err)
	}
	if !res.State.Unverified || len(res.Mutations) != 2 {
		t.Fatalf("want unverified state with 2 records, got unverified=%v records=%d", res.State.Unverified, len(res.Mutations))
	}
	client.apply(res)

	m1, _ := EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", "1@s.whatsapp.net"}, mute("1", false), 2, keyID, keyData)
	patch, _, err := SealPatch(Regular, phoneState, []*waserversync.SyncdMutation{m1}, phone, keyID, keyData)
	if err != nil {
		t.Fatal(err)
	}
	pres, err := ApplyPatch(Regular, res.State, patch, client)
	if err != nil {
		t.Fatalf("patch on unverified state: %v", err)
	}
	if !pres.State.Unverified || pres.State.Version != 8 || pres.Mutations[0].Action.GetMuteAction().GetMuted() {
		t.Fatalf("patch result %+v", pres.State)
	}

	bad := proto.Clone(patch).(*waserversync.SyncdPatch)
	bad.PatchMAC[0] ^= 1
	if _, err := ApplyPatch(Regular, res.State, bad, client); !errors.Is(err, ErrPatchMAC) {
		t.Fatalf("tampered patch on unverified state: %v", err)
	}
	verified := res.State
	verified.Unverified = false
	if _, err := ApplyPatch(Regular, verified, patch, client); !errors.Is(err, ErrSnapshotMAC) {
		t.Fatalf("lthash mismatch on a verified state must fail, got %v", err)
	}
}
