package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// WebMToOgg rewraps the Opus audio of a WebM file (what browsers record)
// as an Ogg Opus file (what WhatsApp expects for voice notes), without
// re-encoding. It returns the file and its duration in seconds.
//
// Only what recorders produce is supported: one Opus track, unlaced
// blocks; elements of unknown size (live recordings) are fine.
func WebMToOgg(webm []byte) (ogg []byte, seconds uint32, err error) {
	track, err := readWebM(webm)
	if err != nil {
		return nil, 0, err
	}
	head := track.head
	if !bytes.HasPrefix(head, []byte("OpusHead")) || len(head) < 19 {
		head = opusHead(track.channels)
	}
	preSkip := uint64(binary.LittleEndian.Uint16(head[10:12]))

	w := &oggWriter{serial: 0x77617821} // "wax!"
	w.page([][]byte{head}, 0, oggBOS)
	w.page([][]byte{opusTags()}, 0, 0)
	var granule uint64
	var batch [][]byte
	segments := 0
	for i, p := range track.packets {
		granule += uint64(opusSamples(p))
		batch = append(batch, p)
		segments += len(p)/255 + 1
		last := i == len(track.packets)-1
		// Pages of about a second (or 200 segments) keep the overhead low.
		if last || segments >= 200 || len(batch) >= 50 {
			flags := byte(0)
			if last {
				flags = oggEOS
			}
			w.page(batch, granule, flags)
			batch, segments = nil, 0
		}
	}
	if granule > preSkip {
		seconds = uint32((granule - preSkip + 24000) / 48000) // nearest second
	}
	return w.buf.Bytes(), seconds, nil
}

// webmTrack is the Opus track of a WebM file.
type webmTrack struct {
	head     []byte // CodecPrivate: the OpusHead
	channels int
	packets  [][]byte
}

// EBML element IDs used here.
const (
	idSegment      = 0x18538067
	idCluster      = 0x1F43B675
	idTracks       = 0x1654AE6B
	idTrackEntry   = 0xAE
	idAudio        = 0xE1
	idBlockGroup   = 0xA0
	idCodecID      = 0x86
	idCodecPrivate = 0x63A2
	idChannels     = 0x9F
	idSimpleBlock  = 0xA3
	idBlock        = 0xA1
)

// readWebM walks the file element by element. Container elements are
// entered rather than skipped, so their (possibly unknown) sizes do not
// matter.
func readWebM(b []byte) (*webmTrack, error) {
	if !bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return nil, errors.New("media: not a WebM file")
	}
	t := &webmTrack{channels: 1}
	codec := ""
	for pos := 0; pos < len(b); {
		id, n := vint(b[pos:], true)
		if n == 0 {
			break
		}
		size, m := vint(b[pos+n:], false)
		if m == 0 {
			break
		}
		pos += n + m
		switch id {
		case idSegment, idCluster, idTracks, idTrackEntry, idAudio, idBlockGroup:
			continue // enter
		}
		if size > uint64(len(b)-pos) {
			break // cut off: a recording stopped mid-block
		}
		data := b[pos : pos+int(size)]
		pos += int(size)
		switch id {
		case idCodecID:
			codec = string(data)
		case idCodecPrivate:
			t.head = data
		case idChannels:
			if c := beUint(data); c > 0 && c <= 8 {
				t.channels = int(c)
			}
		case idSimpleBlock, idBlock:
			p, err := blockFrame(data)
			if err != nil {
				return nil, err
			}
			if len(p) > 0 {
				t.packets = append(t.packets, p)
			}
		}
	}
	if codec != "A_OPUS" {
		return nil, fmt.Errorf("media: WebM audio must be Opus, not %q", codec)
	}
	if len(t.packets) == 0 {
		return nil, errors.New("media: WebM file has no audio")
	}
	return t, nil
}

// blockFrame returns the frame of an unlaced (Simple)Block: track number,
// 16-bit timecode and flags come first.
func blockFrame(data []byte) ([]byte, error) {
	_, n := vint(data, false)
	if n == 0 || len(data) < n+3 {
		return nil, errors.New("media: malformed WebM block")
	}
	if lacing := data[n+2] >> 1 & 3; lacing != 0 {
		return nil, errors.New("media: laced WebM blocks are not supported")
	}
	return data[n+3:], nil
}

// vint reads an EBML variable-length integer: IDs keep their length
// marker, sizes do not. n is 0 when b is too short or invalid.
func vint(b []byte, keepMarker bool) (v uint64, n int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if len(b) < n {
		return 0, 0
	}
	v = uint64(b[0])
	if !keepMarker {
		v &= 0xFF >> n
	}
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// opusSamples is the duration of an Opus packet in 48 kHz samples, from
// its TOC byte (RFC 6716, section 3.1).
func opusSamples(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	cfg := p[0] >> 3
	var perFrame int
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		perFrame = [4]int{480, 960, 1920, 2880}[cfg&3]
	case cfg < 16: // hybrid: 10, 20 ms
		perFrame = [2]int{480, 960}[cfg&1]
	default: // CELT: 2.5, 5, 10, 20 ms
		perFrame = [4]int{120, 240, 480, 960}[cfg&3]
	}
	switch p[0] & 3 {
	case 0:
		return perFrame
	case 1, 2:
		return 2 * perFrame
	}
	if len(p) < 2 {
		return 0
	}
	return int(p[1]&0x3F) * perFrame
}

// opusHead is an identification header for a track without one
// (RFC 7845, section 5.1).
func opusHead(channels int) []byte {
	h := append([]byte("OpusHead"), 1, byte(channels))
	h = binary.LittleEndian.AppendUint16(h, 312) // pre-skip: libopus's usual encoder delay
	h = binary.LittleEndian.AppendUint32(h, 48000)
	return append(h, 0, 0, 0) // output gain 0, mapping family 0
}

// opusTags is a comment header with only a vendor string.
func opusTags() []byte {
	t := append([]byte("OpusTags"), 3, 0, 0, 0)
	return append(append(t, "wax"...), 0, 0, 0, 0)
}

// Ogg page flags.
const (
	oggBOS = 0x02
	oggEOS = 0x04
)

type oggWriter struct {
	buf    bytes.Buffer
	serial uint32
	seq    uint32
}

// page writes one page holding whole packets (each under 64 KiB, as Opus
// packets are), with granule as the position after its last sample.
func (w *oggWriter) page(packets [][]byte, granule uint64, flags byte) {
	var lacing []byte
	for _, p := range packets {
		n := len(p)
		for ; n >= 255; n -= 255 {
			lacing = append(lacing, 255)
		}
		lacing = append(lacing, byte(n))
	}
	h := make([]byte, 27, 27+len(lacing))
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[14:], w.serial)
	binary.LittleEndian.PutUint32(h[18:], w.seq)
	h[26] = byte(len(lacing))
	h = append(h, lacing...)
	w.seq++
	start := w.buf.Len()
	w.buf.Write(h)
	for _, p := range packets {
		w.buf.Write(p)
	}
	page := w.buf.Bytes()[start:]
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
}

// oggCRC is Ogg's page checksum: CRC-32 with polynomial 0x04C11DB7,
// MSB first, no reflection, initial value and final XOR 0.
func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ oggTable[byte(crc>>24)^c]
	}
	return crc
}

var oggTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()
