package client

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// Reporting tokens. WhatsApp clients give every message a random message
// secret and attach <reporting><reporting_token v="2">: an HMAC over selected
// fields of the message, keyed from that secret. When a recipient reports the
// message, WhatsApp checks the reported content against the token. A client
// that never sends one stands out from WhatsApp Web.

// reportingFieldsJSON lists the wae2e.Message fields the token covers:
// f is a field number, s the covered subfields of a nested message. An
// entry marked m without s is a nested message none of whose fields count.
// The table is WhatsApp Web's, as published by Baileys (MIT).
const reportingFieldsJSON = `[{"f":1},{"f":3,"s":[{"f":2},{"f":3},{"f":8},{"f":11},{"f":17,"s":[{"f":21},{"f":22}]},{"f":25}]},{"f":4,"s":[{"f":1},{"f":16},{"f":17,"s":[{"f":21},{"f":22}]}]},{"f":5,"s":[{"f":3},{"f":4},{"f":5},{"f":16},{"f":17,"s":[{"f":21},{"f":22}]}]},{"f":6,"s":[{"f":1},{"f":17,"s":[{"f":21},{"f":22}]},{"f":30}]},{"f":7,"s":[{"f":2},{"f":7},{"f":10},{"f":17,"s":[{"f":21},{"f":22}]},{"f":20}]},{"f":8,"s":[{"f":2},{"f":7},{"f":9},{"f":17,"s":[{"f":21},{"f":22}]},{"f":21}]},{"f":9,"s":[{"f":2},{"f":6},{"f":7},{"f":13},{"f":17,"s":[{"f":21},{"f":22}]},{"f":20}]},{"f":12,"s":[{"f":1},{"f":2},{"f":14,"m":true},{"f":15}]},{"f":18,"s":[{"f":6},{"f":16},{"f":17,"s":[{"f":21},{"f":22}]}]},{"f":26,"s":[{"f":4},{"f":5},{"f":8},{"f":13},{"f":17,"s":[{"f":21},{"f":22}]}]},{"f":28,"s":[{"f":1},{"f":2},{"f":4},{"f":5},{"f":6},{"f":7,"s":[{"f":21},{"f":22}]}]},{"f":37,"s":[{"f":1,"m":true}]},{"f":49,"s":[{"f":2},{"f":3,"s":[{"f":1},{"f":2}]},{"f":5,"s":[{"f":21},{"f":22}]},{"f":8,"s":[{"f":1},{"f":2}]}]},{"f":53,"s":[{"f":1,"m":true}]},{"f":55,"s":[{"f":1,"m":true}]},{"f":58,"s":[{"f":1,"m":true}]},{"f":59,"s":[{"f":1,"m":true}]},{"f":60,"s":[{"f":2},{"f":3,"s":[{"f":1},{"f":2}]},{"f":5,"s":[{"f":21},{"f":22}]},{"f":8,"s":[{"f":1},{"f":2}]}]},{"f":64,"s":[{"f":2},{"f":3,"s":[{"f":1},{"f":2}]},{"f":5,"s":[{"f":21},{"f":22}]},{"f":8,"s":[{"f":1},{"f":2}]}]},{"f":66,"s":[{"f":2},{"f":6},{"f":7},{"f":13},{"f":17,"s":[{"f":21},{"f":22}]},{"f":20}]},{"f":74,"s":[{"f":1,"m":true}]},{"f":87,"s":[{"f":1,"m":true}]},{"f":88,"s":[{"f":1},{"f":2,"s":[{"f":1}]},{"f":3,"s":[{"f":21},{"f":22}]}]},{"f":92,"s":[{"f":1,"m":true}]},{"f":93,"s":[{"f":1,"m":true}]},{"f":94,"s":[{"f":1,"m":true}]}]`

type reportingField struct {
	Num     int              `json:"f"`
	Message bool             `json:"m,omitempty"`
	Sub     []reportingField `json:"s,omitempty"`
}

var reportingFields = sync.OnceValue(func() []reportingField {
	var f []reportingField
	if err := json.Unmarshal([]byte(reportingFieldsJSON), &f); err != nil {
		panic("client: invalid reporting field table: " + err.Error())
	}
	return f
})

// carriesReportingToken reports whether msg gets a secret and a token.
// Reactions are exempt, as in WhatsApp Web.
func carriesReportingToken(msg *wae2e.Message) bool {
	return msg.GetReactionMessage() == nil
}

// withMessageSecret returns msg with a message secret, adding a random
// 32-byte one (on a copy) when it has none.
func withMessageSecret(msg *wae2e.Message) *wae2e.Message {
	if !carriesReportingToken(msg) || len(msg.GetMessageContextInfo().GetMessageSecret()) > 0 {
		return msg
	}
	msg = proto.Clone(msg).(*wae2e.Message)
	if msg.MessageContextInfo == nil {
		msg.MessageContextInfo = &wae2e.MessageContextInfo{}
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	msg.MessageContextInfo.MessageSecret = secret
	return msg
}

// reportingNode builds the <reporting> element of a message we send to
// chat as sender (both without device), or reports false when msg carries
// no token. plaintext is msg serialized, before padding.
func reportingNode(msg *wae2e.Message, plaintext []byte, id string, sender, chat wabin.JID) (wabin.Node, bool) {
	secret := msg.GetMessageContextInfo().GetMessageSecret()
	if !carriesReportingToken(msg) || len(secret) == 0 {
		return wabin.Node{}, false
	}
	content, ok := reportingContent(plaintext, reportingFields())
	if !ok || len(content) == 0 {
		return wabin.Node{}, false
	}
	key := cryptoutil.HKDF(secret, nil, id+sender.ToNonAD().String()+chat.ToNonAD().String()+"Report Token", 32)
	return wabin.Node{Tag: "reporting", Content: []wabin.Node{{
		Tag: "reporting_token", Attrs: wabin.Attrs{"v": "2"}, Content: cryptoutil.HMACSHA256(key, content)[:16],
	}}}, true
}

// reportingContent returns the covered fields of a serialized message as
// they appear on the wire, ordered by field number; nested messages are
// re-encoded with only their covered fields. It reports false for
// malformed input.
func reportingContent(data []byte, fields []reportingField) ([]byte, bool) {
	type part struct {
		num   int
		bytes []byte
	}
	var parts []part
	for i := 0; i < len(data); {
		tag, n := binary.Uvarint(data[i:])
		if n <= 0 {
			return nil, false
		}
		start := i
		i += n
		num := int(tag >> 3)
		var f *reportingField
		if j := slices.IndexFunc(fields, func(f reportingField) bool { return f.Num == num }); j >= 0 {
			f = &fields[j]
		}
		var end int
		switch tag & 7 {
		case 0: // varint
			_, m := binary.Uvarint(data[i:])
			if m <= 0 {
				return nil, false
			}
			end = i + m
		case 1: // 64-bit
			end = i + 8
		case 5: // 32-bit
			end = i + 4
		case 2: // length-delimited
			l, m := binary.Uvarint(data[i:])
			if m <= 0 || l > uint64(len(data)-i-m) {
				return nil, false
			}
			end = i + m + int(l)
			if f != nil && (f.Message || len(f.Sub) > 0) {
				sub, ok := reportingContent(data[i+m:end], f.Sub)
				if !ok {
					return nil, false
				}
				if len(sub) > 0 {
					b := binary.AppendUvarint(nil, tag)
					b = binary.AppendUvarint(b, uint64(len(sub)))
					parts = append(parts, part{num, append(b, sub...)})
				}
				i = end
				continue
			}
		default:
			return nil, false
		}
		if end > len(data) {
			return nil, false
		}
		if f != nil {
			parts = append(parts, part{num, data[start:end]})
		}
		i = end
	}
	slices.SortStableFunc(parts, func(a, b part) int { return a.num - b.num })
	var out []byte
	for _, p := range parts {
		out = append(out, p.bytes...)
	}
	return out, true
}
