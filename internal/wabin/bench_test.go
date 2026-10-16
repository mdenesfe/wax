package wabin

import (
	"bytes"
	"testing"
)

// benchNode is a group message fanned out to 50 devices, the largest
// stanza we send routinely.
func benchNode() Node {
	var to []Node
	for i := range 50 {
		to = append(to, Node{Tag: "to", Attrs: Attrs{"jid": JID{User: "123456789012345", Device: uint16(i), Server: ServerLID}},
			Content: []Node{{Tag: "enc", Attrs: Attrs{"v": "2", "type": "msg"}, Content: bytes.Repeat([]byte{7}, 200)}}})
	}
	return Node{Tag: "message", Attrs: Attrs{"id": "3EB0C80AFF25C8F8F2075B", "type": "text", "to": JID{User: "120363000000000000", Server: ServerGroup}},
		Content: []Node{{Tag: "participants", Content: to}, {Tag: "enc", Attrs: Attrs{"v": "2", "type": "skmsg"}, Content: bytes.Repeat([]byte{9}, 300)}}}
}

func BenchmarkEncode(b *testing.B) {
	n := benchNode()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	raw, err := Marshal(benchNode())
	if err != nil {
		b.Fatal(err)
	}
	raw = raw[1:] // Unmarshal takes the node without the flags byte
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
	}
}
