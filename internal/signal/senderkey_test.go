package signal

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
)

func roundTripSK(t *testing.T, r *SenderKeyRecord) *SenderKeyRecord {
	t.Helper()
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalSenderKeyRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSenderKeysBasic(t *testing.T) {
	alice := &SenderKeyRecord{}
	skdm, err := alice.CreateDistribution()
	if err != nil {
		t.Fatal(err)
	}
	alice = roundTripSK(t, alice)
	bob := &SenderKeyRecord{}
	if err := bob.ProcessDistribution(skdm); err != nil {
		t.Fatal(err)
	}
	var msgs [][]byte
	for i := range 30 {
		ct, err := alice.EncryptGroup([]byte(fmt.Sprintf("m%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		alice = roundTripSK(t, alice)
		msgs = append(msgs, ct)
	}
	order := rand.New(rand.NewPCG(3, 4)).Perm(len(msgs))
	for _, i := range order {
		pt, err := bob.DecryptGroup(msgs[i])
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if string(pt) != fmt.Sprintf("m%d", i) {
			t.Fatalf("got %q", pt)
		}
		bob = roundTripSK(t, bob)
	}
	if _, err := bob.DecryptGroup(msgs[4]); !errors.Is(err, ErrDuplicateMessage) {
		t.Fatalf("replay: %v", err)
	}
	// Replaying the original distribution must not rewind the chain.
	if err := bob.ProcessDistribution(skdm); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.DecryptGroup(msgs[4]); !errors.Is(err, ErrDuplicateMessage) {
		t.Fatalf("distribution replay rewound the chain: %v", err)
	}
	// A distribution created later starts at the current iteration and
	// cannot decrypt earlier messages.
	late, _ := alice.CreateDistribution()
	carol := &SenderKeyRecord{}
	if err := carol.ProcessDistribution(late); err != nil {
		t.Fatal(err)
	}
	if _, err := carol.DecryptGroup(msgs[0]); !errors.Is(err, ErrDuplicateMessage) {
		t.Fatalf("expected old message to be undecryptable, got %v", err)
	}
	next, _ := alice.EncryptGroup([]byte("after"))
	if pt, err := carol.DecryptGroup(next); err != nil || string(pt) != "after" {
		t.Fatalf("carol: %q %v", pt, err)
	}
}

func TestSenderKeyTamperAndUnknown(t *testing.T) {
	alice := &SenderKeyRecord{}
	skdm, _ := alice.CreateDistribution()
	bob := &SenderKeyRecord{}
	_ = bob.ProcessDistribution(skdm)
	ct, _ := alice.EncryptGroup([]byte("hello"))
	for _, pos := range []int{0, 3, len(ct) / 2, len(ct) - 1} {
		bad := append([]byte(nil), ct...)
		bad[pos] ^= 0x20
		if _, err := bob.DecryptGroup(bad); err == nil {
			t.Fatalf("tampered byte %d accepted", pos)
		}
	}
	if pt, err := bob.DecryptGroup(ct); err != nil || string(pt) != "hello" {
		t.Fatal("genuine message rejected after tampering attempts")
	}
	other := &SenderKeyRecord{}
	otherDist, _ := other.CreateDistribution()
	_ = otherDist
	ct2, _ := other.EncryptGroup([]byte("x"))
	if _, err := bob.DecryptGroup(ct2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown key id: %v", err)
	}
	if _, err := (&SenderKeyRecord{}).DecryptGroup(ct); !errors.Is(err, ErrNoSession) {
		t.Fatal("empty record must report no session")
	}
	if _, err := bob.EncryptGroup([]byte("x")); !errors.Is(err, ErrNoSession) {
		t.Fatal("received chains must not be usable for sending")
	}
}

func TestSenderKeyStateLimit(t *testing.T) {
	r := &SenderKeyRecord{}
	for range maxSenderKeyStates + 3 {
		s := &SenderKeyRecord{}
		d, _ := s.CreateDistribution()
		if err := r.ProcessDistribution(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.states) != maxSenderKeyStates {
		t.Fatalf("%d states kept", len(r.states))
	}
}
