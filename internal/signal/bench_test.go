package signal

import (
	"bytes"
	"testing"
)

var benchMsg = bytes.Repeat([]byte("x"), 200) // a typical padded text message

// BenchmarkSessionChain measures one encrypt + decrypt on an established
// session in the same direction (symmetric chain step, no DH ratchet).
func BenchmarkSessionChain(b *testing.B) {
	alice, bob := establish(b, true)
	mustReceive(b, bob, alice.send(b, "hi"), "hi")
	mustReceive(b, alice, bob.send(b, "hello"), "hello")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, ct, err := alice.record.Encrypt(benchMsg)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := bob.record.DecryptMessage(ct); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSessionRatchet alternates directions, so every message also
// performs a DH ratchet step.
func BenchmarkSessionRatchet(b *testing.B) {
	alice, bob := establish(b, true)
	mustReceive(b, bob, alice.send(b, "hi"), "hi")
	from, to := bob, alice
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, ct, err := from.record.Encrypt(benchMsg)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := to.record.DecryptMessage(ct); err != nil {
			b.Fatal(err)
		}
		from, to = to, from
	}
}

// BenchmarkSessionSetup measures X3DH (ProcessBundle) plus the receiving
// side's PreKeySignalMessage processing.
func BenchmarkSessionSetup(b *testing.B) {
	bob := newParty(b, 200)
	alice := newParty(b, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ar, br := &Record{}, &Record{}
		if err := ar.ProcessBundle(alice.local(), bob.bundle(false)); err != nil {
			b.Fatal(err)
		}
		_, ct, err := ar.Encrypt(benchMsg)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := br.DecryptPreKeyMessage(bob.local(), bob, ct); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecordSerialize is the per-message persistence cost of a
// session record (marshal + unmarshal).
func BenchmarkRecordSerialize(b *testing.B) {
	alice, bob := establish(b, true)
	mustReceive(b, bob, alice.send(b, "hi"), "hi")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		raw, err := bob.record.Marshal()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := UnmarshalRecord(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSenderKey(b *testing.B) {
	alice, bob := &SenderKeyRecord{}, &SenderKeyRecord{}
	skdm, err := alice.CreateDistribution()
	if err != nil {
		b.Fatal(err)
	}
	if err := bob.ProcessDistribution(skdm); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ct, err := alice.EncryptGroup(benchMsg)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := bob.DecryptGroup(ct); err != nil {
			b.Fatal(err)
		}
	}
}
