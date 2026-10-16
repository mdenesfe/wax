package store

import (
	"context"
	"testing"

	"github.com/mdenesfe/wax/internal/signal"
)

// BenchmarkSessionPutLoad is the database cost per decrypted message: a
// sealed session record written and read back in one transaction.
func BenchmarkSessionPutLoad(b *testing.B) {
	s := openTestStore(b)
	ctx := context.Background()
	id := "bench-session"
	d, _ := NewDevice(id)
	_ = s.DeleteDevice(ctx, id)
	if err := s.CreateDevice(ctx, d); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.DeleteDevice(ctx, id) })
	rec := &signal.Record{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		err := s.InTx(ctx, func(tx *Store) error {
			if _, err := tx.LoadSession(ctx, id, "123.0:1"); err != nil {
				return err
			}
			return tx.PutSession(ctx, id, "123.0:1", rec)
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
