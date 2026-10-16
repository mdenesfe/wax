package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ebml encodes one element; size -1 writes "unknown size" like a live
// recording.
func ebml(id uint32, size int, data ...[]byte) []byte {
	var out []byte
	for shift := 24; shift >= 0; shift -= 8 {
		if b := byte(id >> shift); b != 0 || len(out) > 0 {
			out = append(out, b)
		}
	}
	body := bytes.Join(data, nil)
	if size < 0 {
		out = append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	} else {
		out = append(out, 0x10, byte(len(body)>>16), byte(len(body)>>8), byte(len(body))) // 4-byte size
	}
	return append(out, body...)
}

// recording builds a WebM file like a browser's: unknown-size segment and
// cluster, an Opus track, and n simple blocks of 20 ms CELT packets.
func recording(n int, withHead bool) []byte {
	head := opusHead(1)
	binary.LittleEndian.PutUint16(head[10:], 3840)
	track := [][]byte{ebml(idCodecID, 0, []byte("A_OPUS")), ebml(idAudio, 0, ebml(idChannels, 0, []byte{1}))}
	if withHead {
		track = append(track, ebml(idCodecPrivate, 0, head))
	}
	var blocks [][]byte
	for i := range n {
		packet := append([]byte{31 << 3}, bytes.Repeat([]byte{byte(i)}, 300)...) // CELT FB 20 ms, one frame; > 255 bytes
		blocks = append(blocks, ebml(idSimpleBlock, 0, []byte{0x81, 0, byte(i), 0x80}, packet))
	}
	return bytes.Join([][]byte{
		ebml(0x1A45DFA3, 0, ebml(0x4282, 0, []byte("webm"))),
		ebml(idSegment, -1,
			ebml(idTracks, 0, ebml(idTrackEntry, 0, track...)),
			ebml(idCluster, -1, ebml(0xE7, 0, []byte{0}), bytes.Join(blocks, nil))),
	}, nil)
}

type oggPage struct {
	flags   byte
	granule uint64
	seq     uint32
	packets [][]byte
}

// readOgg parses pages, checking each checksum.
func readOgg(t *testing.T, b []byte) []oggPage {
	t.Helper()
	var pages []oggPage
	for len(b) > 0 {
		if len(b) < 27 || string(b[:4]) != "OggS" {
			t.Fatalf("bad page header at %d bytes left", len(b))
		}
		nseg := int(b[26])
		lacing := b[27 : 27+nseg]
		size := 27 + nseg
		for _, l := range lacing {
			size += int(l)
		}
		page := append([]byte(nil), b[:size]...)
		want := binary.LittleEndian.Uint32(page[22:])
		binary.LittleEndian.PutUint32(page[22:], 0)
		if got := oggCRC(page); got != want {
			t.Fatalf("page %d: crc %08x, want %08x", len(pages), got, want)
		}
		p := oggPage{flags: b[5], granule: binary.LittleEndian.Uint64(b[6:]), seq: binary.LittleEndian.Uint32(b[18:])}
		body := b[27+nseg : size]
		var cur []byte
		for _, l := range lacing {
			cur = append(cur, body[:l]...)
			body = body[l:]
			if l < 255 {
				p.packets = append(p.packets, cur)
				cur = nil
			}
		}
		pages = append(pages, p)
		b = b[size:]
	}
	return pages
}

func TestWebMToOgg(t *testing.T) {
	for _, withHead := range []bool{true, false} {
		const n = 120 // 2.4 s
		ogg, secs, err := WebMToOgg(recording(n, withHead))
		if err != nil {
			t.Fatal(err)
		}
		pages := readOgg(t, ogg)
		if len(pages) < 4 || pages[0].flags != oggBOS || !bytes.HasPrefix(pages[0].packets[0], []byte("OpusHead")) ||
			!bytes.HasPrefix(pages[1].packets[0], []byte("OpusTags")) || pages[len(pages)-1].flags != oggEOS {
			t.Fatalf("headers or flags wrong (%d pages)", len(pages))
		}
		var packets [][]byte
		for i, p := range pages {
			if p.seq != uint32(i) {
				t.Fatalf("page %d has sequence %d", i, p.seq)
			}
			if i >= 2 {
				packets = append(packets, p.packets...)
			}
		}
		if len(packets) != n || len(packets[7]) != 301 || packets[7][1] != 7 {
			t.Fatalf("%d packets", len(packets))
		}
		if g := pages[len(pages)-1].granule; g != n*960 {
			t.Fatalf("final granule %d", g)
		}
		preSkip := 312
		if withHead {
			preSkip = 3840
		}
		if want := uint32((n*960 - preSkip + 24000) / 48000); secs != want {
			t.Fatalf("seconds %d, want %d", secs, want)
		}
	}
}

func TestWebMToOggRejects(t *testing.T) {
	if _, _, err := WebMToOgg([]byte("not webm")); err == nil {
		t.Fatal("accepted garbage")
	}
	vorbis := bytes.Replace(recording(3, true), []byte("A_OPUS"), []byte("A_VORB"), 1)
	if _, _, err := WebMToOgg(vorbis); err == nil {
		t.Fatal("accepted a non-Opus track")
	}
	// A recording cut mid-block still yields what came before.
	full := recording(10, true)
	if _, _, err := WebMToOgg(full[:len(full)-100]); err != nil {
		t.Fatalf("truncated: %v", err)
	}
}

func TestOpusSamples(t *testing.T) {
	for _, tt := range []struct {
		p    []byte
		want int
	}{
		{[]byte{0 << 3}, 480},        // SILK NB 10 ms
		{[]byte{3 << 3}, 2880},       // SILK NB 60 ms
		{[]byte{13 << 3}, 960},       // hybrid 20 ms
		{[]byte{16 << 3}, 120},       // CELT 2.5 ms
		{[]byte{31<<3 | 1}, 1920},    // two 20 ms frames
		{[]byte{31<<3 | 3, 3}, 2880}, // three frames (code 3)
		{nil, 0},
	} {
		if got := opusSamples(tt.p); got != tt.want {
			t.Errorf("%x: %d, want %d", tt.p, got, tt.want)
		}
	}
}
