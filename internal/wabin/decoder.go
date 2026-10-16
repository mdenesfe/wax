package wabin

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// maxDepth bounds node nesting. Real stanzas are a handful of levels deep;
// the limit only exists so a malformed frame cannot exhaust the stack.
const maxDepth = 64

// MaxDecompressedSize bounds zlib-compressed frames (zip-bomb protection).
// Large payloads such as history sync blobs travel via media download, not
// inline, so a generous cap is safe.
const MaxDecompressedSize = 64 << 20

// flagCompressed marks a zlib-compressed payload in the frame flags byte.
const flagCompressed = 0x02

// Unpack strips the flags byte of a decrypted frame and inflates the payload
// if it is compressed. The result is suitable for Unmarshal.
func Unpack(frame []byte) ([]byte, error) {
	if len(frame) == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrInvalidNode)
	}
	flags, data := frame[0], frame[1:]
	if flags&flagCompressed == 0 {
		return data, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("wabin: open zlib stream: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, MaxDecompressedSize+1))
	if err != nil {
		return nil, fmt.Errorf("wabin: inflate frame: %w", err)
	}
	if len(out) > MaxDecompressedSize {
		return nil, fmt.Errorf("%w: decompressed frame exceeds %d bytes", ErrTooLarge, MaxDecompressedSize)
	}
	return out, nil
}

// Unmarshal decodes a single node from data (without the flags byte; see
// Unpack). All byte slices in the result are copies, so data may be reused.
func Unmarshal(data []byte) (*Node, error) {
	d := decoder{data: data}
	n := &Node{}
	err := d.readNode(n, 0)
	if err != nil {
		return nil, fmt.Errorf("%w (at offset %d)", err, d.pos)
	}
	if d.pos != len(d.data) {
		return n, fmt.Errorf("%w: %d bytes", ErrTrailingBytes, len(d.data)-d.pos)
	}
	return n, nil
}

// errShort is wrapped into every out-of-bounds read.
var errShort = errors.New("wabin: unexpected end of data")

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) need(n int) error {
	if n < 0 || d.pos+n > len(d.data) {
		return fmt.Errorf("%w: need %d bytes, have %d", errShort, n, len(d.data)-d.pos)
	}
	return nil
}

func (d *decoder) byte1() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	b := d.data[d.pos]
	d.pos++
	return b, nil
}

func (d *decoder) uintN(n int) (int, error) {
	if err := d.need(n); err != nil {
		return 0, err
	}
	v := 0
	for i := range n {
		v = v<<8 | int(d.data[d.pos+i])
	}
	d.pos += n
	return v, nil
}

func (d *decoder) raw(n int) ([]byte, error) {
	if err := d.need(n); err != nil {
		return nil, err
	}
	out := bytes.Clone(d.data[d.pos : d.pos+n])
	if out == nil {
		out = []byte{}
	}
	d.pos += n
	return out, nil
}

func (d *decoder) listSize(tag byte) (int, error) {
	switch tag {
	case tagListEmpty:
		return 0, nil
	case tagList8:
		return d.uintN(1)
	case tagList16:
		return d.uintN(2)
	}
	return 0, fmt.Errorf("%w: expected list tag, got %d", ErrInvalidToken, tag)
}

// readNode decodes one node into n (in place, so lists of nodes need no
// per-node allocation).
func (d *decoder) readNode(n *Node, depth int) error {
	if depth > maxDepth {
		return ErrTooDeep
	}
	tag, err := d.byte1()
	if err != nil {
		return err
	}
	size, err := d.listSize(tag)
	if err != nil {
		return err
	}
	if size == 0 {
		return fmt.Errorf("%w: empty node list", ErrInvalidNode)
	}
	desc, err := d.readValue(true, depth)
	if err != nil {
		return fmt.Errorf("node tag: %w", err)
	}
	tagStr, ok := desc.(string)
	if !ok || tagStr == "" {
		return fmt.Errorf("%w: tag is %T", ErrInvalidNode, desc)
	}
	n.Tag = tagStr

	// List layout: tag, then key/value pairs, then optional content.
	if attrCount := (size - 1) / 2; attrCount > 0 {
		n.Attrs = make(Attrs, attrCount)
		for range attrCount {
			k, err := d.readValue(true, depth)
			if err != nil {
				return fmt.Errorf("attribute key in <%s>: %w", tagStr, err)
			}
			key, ok := k.(string)
			if !ok {
				return fmt.Errorf("%w in <%s>: %T", ErrNonStringKey, tagStr, k)
			}
			v, err := d.readValue(true, depth)
			if err != nil {
				return fmt.Errorf("attribute %q in <%s>: %w", key, tagStr, err)
			}
			n.Attrs[key] = v
		}
	}
	if size%2 == 0 {
		n.Content, err = d.readValue(false, depth)
		if err != nil {
			return fmt.Errorf("content of <%s>: %w", tagStr, err)
		}
	}
	return nil
}

// readValue reads any value. asString controls whether binary blobs are
// returned as string (attribute keys/values, JID parts) or []byte (content).
func (d *decoder) readValue(asString bool, depth int) (any, error) {
	// JIDs nest values too, so the depth limit is enforced here and not only
	// per node; otherwise a chain of JID tags recurses without bound.
	if depth > maxDepth {
		return nil, ErrTooDeep
	}
	tag, err := d.byte1()
	if err != nil {
		return nil, err
	}
	switch tag {
	case tagListEmpty:
		return nil, nil
	case tagList8, tagList16:
		return d.readList(tag, depth)
	case tagBinary8, tagBinary20, tagBinary32:
		return d.readBinary(tag, asString)
	case tagDictionary0, tagDictionary1, tagDictionary2, tagDictionary3:
		return d.readDictionary(tag)
	case tagJIDPair:
		return d.readJIDPair(depth)
	case tagADJID:
		return d.readADJID(depth)
	case tagFBJID, tagInteropJID:
		return d.readDeviceJID(tag == tagInteropJID, depth)
	case tagNibble8:
		return d.readPacked(unpackNibble)
	case tagHex8:
		return d.readPacked(unpackHex)
	}
	if int(tag) < len(singleByteTokens) && tag != 0 {
		return singleByteValues[tag], nil
	}
	return nil, fmt.Errorf("%w: tag %d", ErrInvalidToken, tag)
}

func (d *decoder) readList(tag byte, depth int) ([]Node, error) {
	size, err := d.listSize(tag)
	if err != nil {
		return nil, err
	}
	// Each node needs at least 2 bytes; reject absurd sizes before allocating.
	if size*2 > len(d.data)-d.pos {
		return nil, fmt.Errorf("%w: list of %d nodes in %d bytes", errShort, size, len(d.data)-d.pos)
	}
	nodes := make([]Node, size)
	for i := range nodes {
		if err := d.readNode(&nodes[i], depth+1); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

func (d *decoder) readBinary(tag byte, asString bool) (any, error) {
	var size int
	var err error
	switch tag {
	case tagBinary8:
		size, err = d.uintN(1)
	case tagBinary20:
		size, err = d.uintN(3)
		size &= 0x0fffff
	default:
		size, err = d.uintN(4)
	}
	if err != nil {
		return nil, err
	}
	b, err := d.raw(size)
	if err != nil {
		return nil, err
	}
	if asString {
		return string(b), nil
	}
	return b, nil
}

func (d *decoder) readDictionary(tag byte) (any, error) {
	idx, err := d.byte1()
	if err != nil {
		return nil, err
	}
	dict := doubleByteValues[tag-tagDictionary0]
	if int(idx) >= len(dict) {
		return nil, fmt.Errorf("%w: dictionary %d index %d", ErrInvalidToken, tag-tagDictionary0, idx)
	}
	return dict[idx], nil
}

// readJIDPair reads user@server; the user part may be absent (nil).
func (d *decoder) readJIDPair(depth int) (JID, error) {
	user, err := d.readValue(true, depth+1)
	if err != nil {
		return JID{}, err
	}
	server, err := d.readValue(true, depth+1)
	if err != nil {
		return JID{}, err
	}
	us, uok := user.(string)
	ss, sok := server.(string)
	if (user != nil && !uok) || !sok {
		return JID{}, fmt.Errorf("%w: jid pair parts %T@%T", ErrInvalidJID, user, server)
	}
	return JID{User: us, Server: ss}, nil
}

// readADJID reads agent, device and user of a device JID on the default
// (phone number or LID) servers.
func (d *decoder) readADJID(depth int) (JID, error) {
	agent, err := d.byte1()
	if err != nil {
		return JID{}, err
	}
	device, err := d.byte1()
	if err != nil {
		return JID{}, err
	}
	user, err := d.readString(depth + 1)
	if err != nil {
		return JID{}, err
	}
	return newADJID(user, agent, device), nil
}

// readDeviceJID reads an FB JID (user, device, server) or, with
// interop, an interop JID (user, device, integrator, server).
func (d *decoder) readDeviceJID(interop bool, depth int) (JID, error) {
	var j JID
	var err error
	if j.User, err = d.readString(depth + 1); err != nil {
		return JID{}, err
	}
	device, err := d.uintN(2)
	if err != nil {
		return JID{}, err
	}
	j.Device = uint16(device)
	if interop {
		integrator, err := d.uintN(2)
		if err != nil {
			return JID{}, err
		}
		j.Integrator = uint16(integrator)
	}
	if j.Server, err = d.readString(depth + 1); err != nil {
		return JID{}, err
	}
	return j, nil
}

func (d *decoder) readString(depth int) (string, error) {
	v, err := d.readValue(true, depth)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok && v != nil {
		return "", fmt.Errorf("%w: expected string, got %T", ErrInvalidJID, v)
	}
	return s, nil
}

func (d *decoder) readPacked(unpack func(byte) (byte, error)) (string, error) {
	lenByte, err := d.byte1()
	if err != nil {
		return "", err
	}
	n := int(lenByte & 0x7f)
	if err := d.need(n); err != nil {
		return "", err
	}
	out := make([]byte, 0, 2*n)
	for _, b := range d.data[d.pos : d.pos+n] {
		hi, err := unpack(b >> 4)
		if err != nil {
			return "", err
		}
		lo, err := unpack(b & 0x0f)
		if err != nil {
			return "", err
		}
		out = append(out, hi, lo)
	}
	d.pos += n
	if lenByte&0x80 != 0 && len(out) > 0 {
		out = out[:len(out)-1] // odd length: drop the padding nibble
	}
	return string(out), nil
}

func unpackNibble(v byte) (byte, error) {
	switch {
	case v < 10:
		return '0' + v, nil
	case v == 10:
		return '-', nil
	case v == 11:
		return '.', nil
	case v == 15:
		return 0, nil // padding; only valid as the final nibble
	}
	return 0, fmt.Errorf("%w: nibble value %d", ErrInvalidToken, v)
}

func unpackHex(v byte) (byte, error) {
	if v < 10 {
		return '0' + v, nil
	}
	return 'A' + v - 10, nil
}
