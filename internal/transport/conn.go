package transport

import (
	"context"
	"crypto/cipher"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
)

// NoiseConn is the encrypted frame stream established by the handshake.
//
// Each direction has its own key and a 32-bit message counter used as the
// GCM nonce; no associated data is used after the handshake. Because the
// counters advance implicitly, a single lost, duplicated or corrupted frame
// desynchronizes the stream permanently. Any decryption failure is therefore
// fatal: the connection is closed and the caller must reconnect.
type NoiseConn struct {
	fc FrameConn

	writeMu      sync.Mutex
	writeKey     cipher.AEAD
	writeCounter uint32

	readKey     cipher.AEAD
	readCounter uint32 // only touched by the single reader

	closed atomic.Bool
}

func newNoiseConn(fc FrameConn, write, read cipher.AEAD) *NoiseConn {
	return &NoiseConn{fc: fc, writeKey: write, readKey: read}
}

// Send encrypts and writes one frame. Safe for concurrent use.
func (c *NoiseConn) Send(ctx context.Context, plaintext []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeCounter == math.MaxUint32 {
		// Never reuse a nonce. ~4 billion frames per connection is far beyond
		// realistic use; reconnecting establishes fresh keys.
		_ = c.Close()
		return fmt.Errorf("%w: write nonce space exhausted", ErrClosed)
	}
	ct := c.writeKey.Seal(nil, nonceFor(c.writeCounter), plaintext, nil)
	// The counter advances even if the write fails: a partially written
	// frame may have reached the server, and the connection is unusable
	// after a write error anyway.
	c.writeCounter++
	if err := c.fc.SendFrame(ctx, ct); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

// Receive reads and decrypts the next frame. Must be called from a single
// goroutine.
func (c *NoiseConn) Receive(ctx context.Context) ([]byte, error) {
	ct, err := c.fc.ReadFrame(ctx)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	pt, err := c.readKey.Open(ct[:0], nonceFor(c.readCounter), ct, nil)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: frame %d: %w", ErrDecrypt, c.readCounter, err)
	}
	c.readCounter++
	return pt, nil
}

// Close closes the underlying connection. It is idempotent.
func (c *NoiseConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.fc.Close()
}
