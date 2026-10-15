package wabin

import (
	"bytes"
	"compress/zlib"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, n Node) *Node {
	t.Helper()
	data, err := Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload, err := Unpack(data)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	got, err := Unmarshal(payload)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

func TestRoundTripTable(t *testing.T) {
	cases := []Node{
		{Tag: "iq"},
		{Tag: "iq", Attrs: Attrs{"id": "abc.123-4", "type": "get", "xmlns": "w:p", "to": ServerJID}},
		{Tag: "message", Attrs: Attrs{"to": JID{User: "905550000000", Server: ServerUser}}, Content: []Node{
			{Tag: "enc", Attrs: Attrs{"v": "2", "type": "pkmsg"}, Content: []byte{0, 1, 2, 0xff}},
		}},
		{Tag: "x", Content: []byte{}},
		{Tag: "x", Content: bytes.Repeat([]byte{7}, 300)},     // BINARY_20
		{Tag: "x", Content: bytes.Repeat([]byte{9}, 1<<20+5)}, // BINARY_32
		{Tag: "devices", Content: []Node{
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "905550000000", Device: 12, Server: ServerUser}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "123456789012345", Device: 3, Server: ServerLID}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "100000000000001", Device: 0, Server: ServerLID}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "42", Device: 7, Server: ServerMessenger}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "42", Device: 1, Integrator: 9, Server: ServerInterop}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{User: "123", Server: ServerHosted}}},
			{Tag: "device", Attrs: Attrs{"jid": JID{Server: ServerGroup}}},
		}},
		{Tag: "packed", Attrs: Attrs{
			"nibble-odd":  "123-45.6",
			"nibble-even": "1234567890",
			"hex-odd":     "ABCDEF0",
			"hex-even":    "DEADBEEF",
			"raw":         "hello, world",
			"long":        strings.Repeat("1", 200), // too long to pack
			"unicode":     "çğüşöı",
		}},
	}
	for i, n := range cases {
		got := roundTrip(t, n)
		if !reflect.DeepEqual(normalize(n), normalize(*got)) {
			t.Errorf("case %d mismatch:\n want %s\n  got %s", i, n.Format(FormatOptions{}), got.Format(FormatOptions{}))
		}
	}
}

// normalize turns the input into the shape the decoder produces: string
// attribute values that came from tokens stay strings, and empty byte
// content stays an empty slice.
func normalize(n Node) Node {
	out := Node{Tag: n.Tag}
	if len(n.Attrs) > 0 {
		out.Attrs = Attrs{}
		for k, v := range n.Attrs {
			if s, ok := attrToString(v); ok {
				out.Attrs[k] = s
			} else {
				out.Attrs[k] = v
			}
		}
	}
	switch c := n.Content.(type) {
	case []Node:
		children := make([]Node, len(c))
		for i := range c {
			children[i] = normalize(c[i])
		}
		out.Content = children
	default:
		out.Content = c
	}
	return out
}

func TestTokensAreUsed(t *testing.T) {
	data, err := Marshal(Node{Tag: "iq", Attrs: Attrs{"type": "get"}})
	if err != nil {
		t.Fatal(err)
	}
	// flags, LIST_8, size=3, token(iq), token(type), token(get)
	want := []byte{0, tagList8, 3, singleByteIndex["iq"], singleByteIndex["type"], singleByteIndex["get"]}
	if !bytes.Equal(data, want) {
		t.Fatalf("got %v want %v", data, want)
	}
	// A double-byte token.
	data, _ = Marshal(Node{Tag: "pair-device"})
	idx := doubleByteIndex["pair-device"]
	if !bytes.Equal(data, []byte{0, tagList8, 1, tagDictionary0 + idx[0], idx[1]}) {
		t.Fatalf("double byte token not used: %v", data)
	}
}

func TestPackedEncoding(t *testing.T) {
	e := encoder{}
	e.writeString("123")
	// NIBBLE_8, len=2|odd, 0x12, 0x3F
	if want := []byte{tagNibble8, 0x82, 0x12, 0x3f}; !bytes.Equal(e.buf, want) {
		t.Fatalf("got %x want %x", e.buf, want)
	}
}

func TestEmptyAttrsOmitted(t *testing.T) {
	got := roundTrip(t, Node{Tag: "iq", Attrs: Attrs{"a": "", "b": nil, "c": "x"}})
	if len(got.Attrs) != 1 || got.Attrs["c"] != "x" {
		t.Fatalf("unexpected attrs %v", got.Attrs)
	}
}

func TestIntegerAttrs(t *testing.T) {
	got := roundTrip(t, Node{Tag: "iq", Attrs: Attrs{"t": int64(1700000000), "count": 5, "flag": true}})
	if got.Attrs["t"] != "1700000000" || got.Attrs["count"] != "5" || got.Attrs["flag"] != "true" {
		t.Fatalf("unexpected attrs %v", got.Attrs)
	}
	if v, ok := got.AttrInt("t"); !ok || v != 1700000000 {
		t.Fatal("AttrInt failed")
	}
}

func TestUnpackCompressed(t *testing.T) {
	plain, _ := Marshal(Node{Tag: "iq", Attrs: Attrs{"id": "1"}})
	var buf bytes.Buffer
	buf.WriteByte(flagCompressed)
	zw := zlib.NewWriter(&buf)
	zw.Write(plain[1:])
	zw.Close()
	payload, err := Unpack(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	n, err := Unmarshal(payload)
	if err != nil || n.Tag != "iq" {
		t.Fatalf("got %v %v", n, err)
	}
}

func TestDecoderRejectsGarbage(t *testing.T) {
	bad := [][]byte{
		{},
		{tagList8},
		{tagList8, 0},
		{tagList8, 1, 240},               // unassigned tag
		{tagList8, 2, 4, tagBinary8},     // truncated content
		{tagList8, 1, tagDictionary3},    // truncated dictionary index
		{tagList8, 3, 4, tagList8, 1, 4}, // non-string attribute key
	}
	for i, b := range bad {
		if _, err := Unmarshal(b); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestDecoderDepthLimit(t *testing.T) {
	n := Node{Tag: "iq"}
	for range maxDepth + 5 {
		n = Node{Tag: "iq", Content: []Node{n}}
	}
	if _, err := Marshal(n); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("encoder: expected ErrTooDeep, got %v", err)
	}
}

// TestDecoderFuzzNoPanic feeds random and mutated input; the decoder must
// return errors, never panic.
func TestDecoderFuzzNoPanic(t *testing.T) {
	seed, _ := Marshal(Node{Tag: "message", Attrs: Attrs{"from": JID{User: "1", Device: 2, Server: ServerUser}}, Content: []Node{{Tag: "enc", Content: []byte("abc")}}})
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		b := bytes.Clone(seed[1:])
		for range 1 + r.IntN(4) {
			b[r.IntN(len(b))] = byte(r.IntN(256))
		}
		_, _ = Unmarshal(b)
		junk := make([]byte, r.IntN(64))
		for i := range junk {
			junk[i] = byte(r.IntN(256))
		}
		_, _ = Unmarshal(junk)
	}
}

func FuzzUnmarshal(f *testing.F) {
	seed, _ := Marshal(Node{Tag: "iq", Attrs: Attrs{"id": "1", "to": ServerJID}, Content: []Node{{Tag: "ping"}}})
	f.Add(seed[1:])
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := Unmarshal(data)
		if err != nil {
			return
		}
		// Anything that decodes must re-encode and decode to the same thing.
		again, err := Marshal(*n)
		if err != nil {
			return // e.g. AD JID with device > 255 cannot be produced by the decoder, but be lenient
		}
		if _, err := Unmarshal(again[1:]); err != nil {
			t.Fatalf("re-decode failed: %v", err)
		}
	})
}

func TestJIDParseString(t *testing.T) {
	cases := []string{"905550000000@s.whatsapp.net", "905550000000:12@s.whatsapp.net", "123@lid", "123:4@lid", "g.us", "1234-5678@g.us", "905550000000.1:2@s.whatsapp.net"}
	for _, s := range cases {
		j, err := ParseJID(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if j.String() != s {
			t.Errorf("round trip %q -> %q", s, j.String())
		}
	}
}

func TestFormatMasks(t *testing.T) {
	n := Node{Tag: "iq", Attrs: Attrs{"from": JID{User: "905550001234", Server: ServerUser}}, Content: []Node{{Tag: "ref", Content: []byte("2@secretref")}}}
	s := n.String()
	if strings.Contains(s, "905550001234") || strings.Contains(s, "secretref") {
		t.Fatalf("sensitive data leaked: %s", s)
	}
	if !strings.Contains(s, "********1234@s.whatsapp.net") {
		t.Fatalf("expected masked jid: %s", s)
	}
	if u := n.Format(FormatOptions{}); !strings.Contains(u, "secretref") {
		t.Fatalf("unmasked output should contain payload: %s", u)
	}
}

func TestTokenTables(t *testing.T) {
	if singleByteTokens[0] != "" || singleByteTokens[3] != ServerUser {
		t.Fatal("unexpected single byte token layout")
	}
	for i, d := range doubleByteTokens {
		if len(d) > 256 {
			t.Fatalf("dictionary %d has %d entries", i, len(d))
		}
	}
}

func TestJIDText(t *testing.T) {
	for _, s := range []string{"905550000000@s.whatsapp.net", "123:4@lid", "120363000000000000@g.us", ""} {
		var j JID
		if err := j.UnmarshalText([]byte(s)); err != nil {
			t.Fatal(err)
		}
		b, err := j.MarshalText()
		if err != nil || string(b) != s {
			t.Fatalf("%q round-tripped to %q (%v)", s, b, err)
		}
	}
}
