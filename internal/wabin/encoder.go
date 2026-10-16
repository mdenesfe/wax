package wabin

import (
	"fmt"
	"math"
)

// Marshal encodes a node into its wire form, including the leading flags byte
// (always 0: we never compress outgoing stanzas). The result is ready to be
// passed to the Noise transport.
func Marshal(n Node) ([]byte, error) {
	e := encoder{buf: make([]byte, 1, 1+sizeHint(&n))}
	if err := e.writeNode(&n, 0); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// sizeHint estimates the encoded size of n (an upper bound for typical
// stanzas), so Marshal allocates its buffer once instead of regrowing it.
// It is linear in the node count, like the encoding itself.
func sizeHint(n *Node) int {
	size := 4 + len(n.Tag)
	for k, v := range n.Attrs {
		size += 4 + len(k)
		switch v := v.(type) {
		case string:
			size += len(v)
		case JID:
			size += 8 + len(v.User) + len(v.Server)
		default:
			size += 8
		}
	}
	switch c := n.Content.(type) {
	case []byte:
		size += 5 + len(c)
	case string:
		size += 5 + len(c)
	case []Node:
		size += 3
		for i := range c {
			size += sizeHint(&c[i])
		}
	}
	return size
}

type encoder struct {
	buf []byte
}

func (e *encoder) byte1(b byte) { e.buf = append(e.buf, b) }

func (e *encoder) uint16be(v int) { e.buf = append(e.buf, byte(v>>8), byte(v)) }

func (e *encoder) writeNode(n *Node, depth int) error {
	if depth > maxDepth {
		return ErrTooDeep
	}
	if n.Tag == "" {
		return fmt.Errorf("%w: empty tag", ErrInvalidNode)
	}
	attrCount := 0
	for _, v := range n.Attrs {
		if !omitAttr(v) {
			attrCount++
		}
	}
	size := 1 + 2*attrCount
	if n.Content != nil {
		size++
	}
	if err := e.writeListStart(size); err != nil {
		return err
	}
	e.writeString(n.Tag)
	for k, v := range n.Attrs {
		if omitAttr(v) {
			continue
		}
		e.writeString(k)
		if err := e.writeValue(v, depth); err != nil {
			return fmt.Errorf("attribute %q of <%s>: %w", k, n.Tag, err)
		}
	}
	if n.Content != nil {
		if err := e.writeValue(n.Content, depth); err != nil {
			return fmt.Errorf("content of <%s>: %w", n.Tag, err)
		}
	}
	return nil
}

// omitAttr reports whether an attribute value is "absent". Callers build
// attribute maps with optional values, and the protocol has no notion of an
// empty attribute, so nil and "" are dropped.
func omitAttr(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

func (e *encoder) writeValue(v any, depth int) error {
	switch x := v.(type) {
	case nil:
		e.byte1(tagListEmpty)
	case string:
		e.writeString(x)
	case JID:
		return e.writeJID(x)
	case []byte:
		return e.writeBytes(x)
	case []Node:
		if err := e.writeListStart(len(x)); err != nil {
			return err
		}
		for i := range x {
			if err := e.writeNode(&x[i], depth+1); err != nil {
				return err
			}
		}
	default:
		s, ok := attrToString(v)
		if !ok {
			return fmt.Errorf("%w: %T", ErrInvalidType, v)
		}
		e.writeString(s)
	}
	return nil
}

func (e *encoder) writeListStart(size int) error {
	switch {
	case size == 0:
		e.byte1(tagListEmpty)
	case size < 256:
		e.byte1(tagList8)
		e.byte1(byte(size))
	case size < 65536:
		e.byte1(tagList16)
		e.uint16be(size)
	default:
		return fmt.Errorf("%w: list of %d items", ErrTooLarge, size)
	}
	return nil
}

// writeString picks the most compact representation: single-byte token,
// double-byte token, nibble/hex packing, or raw bytes.
func (e *encoder) writeString(s string) {
	if s == "" {
		e.byte1(tagBinary8)
		e.byte1(0)
		return
	}
	if idx, ok := singleByteIndex[s]; ok {
		e.byte1(idx)
		return
	}
	if idx, ok := doubleByteIndex[s]; ok {
		e.byte1(tagDictionary0 + idx[0])
		e.byte1(idx[1])
		return
	}
	if canPack(s, nibbleValue) {
		e.writePacked(s, tagNibble8, nibbleValue)
		return
	}
	if canPack(s, hexValue) {
		e.writePacked(s, tagHex8, hexValue)
		return
	}
	// Strings cannot exceed what writeBytes accepts in practice; any error
	// there is a programming error upstream and would also fail on []byte.
	_ = e.writeBytes([]byte(s))
}

func (e *encoder) writeBytes(b []byte) error {
	n := len(b)
	switch {
	case n < 256:
		e.byte1(tagBinary8)
		e.byte1(byte(n))
	case n < 1<<20:
		e.byte1(tagBinary20)
		e.buf = append(e.buf, byte(n>>16)&0x0f, byte(n>>8), byte(n))
	case n <= math.MaxInt32:
		e.byte1(tagBinary32)
		e.buf = append(e.buf, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		return fmt.Errorf("%w: %d bytes", ErrTooLarge, n)
	}
	e.buf = append(e.buf, b...)
	return nil
}

func (e *encoder) writeJID(j JID) error {
	switch {
	case (j.Server == ServerUser || j.Server == ServerLID) && j.Device > 0,
		j.Server == ServerHosted, j.Server == ServerHostedLID:
		if j.Device > 255 {
			return fmt.Errorf("%w: device %d does not fit AD JID", ErrInvalidJID, j.Device)
		}
		e.byte1(tagADJID)
		e.byte1(j.agent())
		e.byte1(byte(j.Device))
		e.writeString(j.User)
	case j.Server == ServerMessenger:
		e.byte1(tagFBJID)
		e.writeString(j.User)
		e.uint16be(int(j.Device))
		e.writeString(j.Server)
	case j.Server == ServerInterop:
		e.byte1(tagInteropJID)
		e.writeString(j.User)
		e.uint16be(int(j.Device))
		e.uint16be(int(j.Integrator))
		e.writeString(j.Server)
	default:
		e.byte1(tagJIDPair)
		if j.User == "" {
			e.byte1(tagListEmpty)
		} else {
			e.writeString(j.User)
		}
		e.writeString(j.Server)
	}
	return nil
}

// Packing: each byte holds two 4-bit codes, high nibble first. The length
// byte is ceil(len/2) with the top bit set when len is odd, in which case the
// final low nibble is padding (0xF) and is discarded by the decoder.
func (e *encoder) writePacked(s string, tag byte, value func(byte) (byte, bool)) {
	e.byte1(tag)
	n := (len(s) + 1) / 2
	lenByte := byte(n)
	if len(s)%2 == 1 {
		lenByte |= 0x80
	}
	e.byte1(lenByte)
	for i := 0; i < len(s); i += 2 {
		hi, _ := value(s[i])
		lo := byte(0x0f)
		if i+1 < len(s) {
			lo, _ = value(s[i+1])
		}
		e.byte1(hi<<4 | lo)
	}
}

func canPack(s string, value func(byte) (byte, bool)) bool {
	if len(s) > packedMax {
		return false
	}
	for i := 0; i < len(s); i++ {
		if _, ok := value(s[i]); !ok {
			return false
		}
	}
	return true
}

func nibbleValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c == '-':
		return 10, true
	case c == '.':
		return 11, true
	}
	return 0, false
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
