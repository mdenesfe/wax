package appstate

import (
	"bytes"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/waserversync"
)

// benchSnapshot builds a snapshot of n mute records (real accounts have a
// few thousand records per collection).
func benchSnapshot(b *testing.B, n int) (*waserversync.SyncdSnapshot, *memLookup) {
	keyID, keyData := []byte{0, 0, 0, 1}, bytes.Repeat([]byte{7}, 32)
	var recs []*waserversync.SyncdRecord
	var macs [][]byte
	for i := range n {
		m, err := EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", fmt.Sprintf("%d@s.whatsapp.net", i)}, mute("", true), 2, keyID, keyData)
		if err != nil {
			b.Fatal(err)
		}
		recs = append(recs, m.Record)
		vm, _ := valueMACOf(m.Record)
		macs = append(macs, vm)
	}
	st := HashState{Version: 1}
	st.SubtractThenAdd(nil, macs)
	ss := &waserversync.SyncdSnapshot{Version: &waserversync.SyncdVersion{Version: proto.Uint64(1)}, Records: recs,
		KeyID: &waserversync.KeyId{ID: keyID}, Mac: SnapshotMAC(st, Regular, ExpandKeys(keyData).SnapshotMAC)}
	return ss, &memLookup{keys: map[string][]byte{string(keyID): keyData}, macs: map[string][]byte{}}
}

func BenchmarkApplySnapshot1000(b *testing.B) {
	ss, l := benchSnapshot(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err := ApplySnapshot(Regular, ss, l)
		if err != nil || res.State.Unverified {
			b.Fatal(err)
		}
	}
}

func BenchmarkLTHashAdd(b *testing.B) {
	item := [][]byte{bytes.Repeat([]byte{1}, 32)}
	var st HashState
	b.ReportAllocs()
	for b.Loop() {
		st.SubtractThenAdd(nil, item)
	}
}
