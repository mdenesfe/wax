// Package transport implements the WhatsApp Web connection stack below the
// binary node layer: WebSocket framing (3-byte length prefix), the
// Noise_XX_25519_AESGCM_SHA256 handshake with server certificate validation,
// and the resulting encrypted, ordered frame stream.
package transport

import (
	"context"
	"errors"
	"fmt"
)

const (
	// FrameLengthSize is the size of the big-endian length prefix of each frame.
	FrameLengthSize = 3
	// MaxFrameSize is the largest payload expressible with a 3-byte length.
	MaxFrameSize = 1<<24 - 1
)

// Errors returned by the transport layer.
var (
	ErrFrameTooLarge = errors.New("transport: frame too large")
	ErrClosed        = errors.New("transport: connection closed")
	ErrHandshake     = errors.New("transport: noise handshake failed")
	ErrDecrypt       = errors.New("transport: frame decryption failed")
)

// FrameConn is a bidirectional stream of length-delimited frames. The
// WebSocket implementation is WSConn; tests use an in-memory pipe.
//
// SendFrame may be called concurrently with ReadFrame. ReadFrame must only be
// called from a single goroutine.
type FrameConn interface {
	SendFrame(ctx context.Context, payload []byte) error
	ReadFrame(ctx context.Context) ([]byte, error)
	Close() error
}

// appendFrame appends prefix (may be nil), the 3-byte length and payload to dst.
func appendFrame(dst, prefix, payload []byte) ([]byte, error) {
	if len(payload) > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFrameTooLarge, len(payload), MaxFrameSize)
	}
	n := len(payload)
	dst = append(dst, prefix...)
	dst = append(dst, byte(n>>16), byte(n>>8), byte(n))
	return append(dst, payload...), nil
}

// frameSplitter reassembles frames from an arbitrary chunking of the byte
// stream. WhatsApp usually sends one frame per WebSocket message, but a frame
// may span messages and a message may carry several frames, so we never rely
// on message boundaries.
type frameSplitter struct {
	buf []byte
}

// push adds received bytes and returns every frame completed by them.
func (s *frameSplitter) push(data []byte) [][]byte {
	s.buf = append(s.buf, data...)
	var frames [][]byte
	for len(s.buf) >= FrameLengthSize {
		n := int(s.buf[0])<<16 | int(s.buf[1])<<8 | int(s.buf[2])
		if len(s.buf) < FrameLengthSize+n {
			break
		}
		frame := make([]byte, n)
		copy(frame, s.buf[FrameLengthSize:FrameLengthSize+n])
		frames = append(frames, frame)
		s.buf = s.buf[FrameLengthSize+n:]
	}
	if len(s.buf) == 0 {
		s.buf = nil // release the backing array between frames
	}
	return frames
}
