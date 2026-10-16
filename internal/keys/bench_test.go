package keys

import "testing"

func BenchmarkSign(b *testing.B) {
	k, _ := NewSigningKey()
	msg := make([]byte, 33)
	b.ReportAllocs()
	for b.Loop() {
		k.Sign(msg)
	}
}

func BenchmarkVerify(b *testing.B) {
	k, _ := NewSigningKey()
	msg := make([]byte, 33)
	sig := k.Sign(msg)
	b.ReportAllocs()
	for b.Loop() {
		if !Verify(k.Pub, msg, sig) {
			b.Fatal("verify failed")
		}
	}
}

func BenchmarkDH(b *testing.B) {
	x, _ := NewKeyPair()
	y, _ := NewKeyPair()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DH(x.Priv, y.Pub); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkKeyPairDH is DH with a prepared key pair (identity keys, fresh
// ratchet keys).
func BenchmarkKeyPairDH(b *testing.B) {
	x, _ := NewKeyPair()
	y, _ := NewKeyPair()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := x.DH(y.Pub); err != nil {
			b.Fatal(err)
		}
	}
}
