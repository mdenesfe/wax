package pairing

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/waadv"
	"github.com/mdenesfe/wax/internal/wabin"
)

// fakePhone plays the primary device: it signs our identity with its account
// key and authenticates the result with the adv secret from the QR code.
type fakePhone struct {
	account *keys.SigningKey
}

func (p *fakePhone) pairSuccess(t *testing.T, companionIdentity [32]byte, advSecret [32]byte, mutate func(*waadv.ADVSignedDeviceIdentity, *[]byte)) *wabin.Node {
	t.Helper()
	details, _ := proto.Marshal(&waadv.ADVDeviceIdentity{RawID: proto.Uint32(77), Timestamp: proto.Uint64(1700000000), KeyIndex: proto.Uint32(3)})
	sig := p.account.Sign(slices.Concat([]byte{6, 0}, details, companionIdentity[:]))
	signed := &waadv.ADVSignedDeviceIdentity{Details: details, AccountSignatureKey: p.account.Pub[:], AccountSignature: sig[:]}
	key := advSecret[:]
	if mutate != nil {
		mutate(signed, &key)
	}
	signedRaw, _ := proto.Marshal(signed)
	mac := hmac.New(sha256.New, key)
	mac.Write(signedRaw)
	container, _ := proto.Marshal(&waadv.ADVSignedDeviceIdentityHMAC{Details: signedRaw, HMAC: mac.Sum(nil)})
	return &wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"id": "pair-1", "type": "set", "from": wabin.ServerJID, "t": "1700000000"},
		Content: []wabin.Node{{Tag: "pair-success", Content: []wabin.Node{
			{Tag: "device-identity", Content: container},
			{Tag: "platform", Attrs: wabin.Attrs{"name": "android"}},
			{Tag: "device", Attrs: wabin.Attrs{
				"jid": wabin.JID{User: "905550000000", Device: 12, Server: wabin.ServerUser},
				"lid": wabin.JID{User: "11111111", Device: 12, Server: wabin.ServerLID},
			}},
		}}},
	}
}

func setup(t *testing.T) (*fakePhone, *keys.SigningKey, [32]byte) {
	acc, _ := keys.NewSigningKey()
	ident, _ := keys.NewSigningKey()
	var adv [32]byte
	rand.Read(adv[:])
	return &fakePhone{account: acc}, ident, adv
}

func TestPairSuccessHappyPath(t *testing.T) {
	phone, ident, adv := setup(t)
	node := phone.pairSuccess(t, ident.Pub, adv, nil)
	s, err := ParsePairSuccess(node)
	if err != nil {
		t.Fatal(err)
	}
	if s.JID.Device != 12 || s.LID.Server != wabin.ServerLID || s.Platform != "android" || s.IQID != "pair-1" || s.ServerTime.Unix() != 1700000000 {
		t.Fatalf("parsed %+v", s)
	}
	res, err := Verify(s, ident, adv)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrimaryIdentity != phone.account.Pub || res.KeyIndex != 3 {
		t.Fatal("wrong primary identity / key index")
	}
	// Our device signature must verify like the server/peers check it.
	var acc waadv.ADVSignedDeviceIdentity
	if err := proto.Unmarshal(res.Account, &acc); err != nil {
		t.Fatal(err)
	}
	if !keys.VerifySlice(ident.Pub[:], slices.Concat([]byte{6, 1}, acc.Details, ident.Pub[:], acc.AccountSignatureKey), acc.DeviceSignature) {
		t.Fatal("device signature does not verify")
	}
	// The confirmation carries the identity without the account key.
	confirm := res.ConfirmNode(s.IQID)
	pds, _ := confirm.Child("pair-device-sign")
	di, _ := pds.Child("device-identity")
	var sent waadv.ADVSignedDeviceIdentity
	raw, _ := di.Bytes()
	if err := proto.Unmarshal(raw, &sent); err != nil || sent.AccountSignatureKey != nil || sent.DeviceSignature == nil {
		t.Fatalf("bad confirmation payload: %v", err)
	}
	if ki, _ := di.AttrString("key-index"); ki != "3" {
		t.Fatalf("key-index %q", ki)
	}
	if _, err := wabin.Marshal(confirm); err != nil {
		t.Fatal(err)
	}
}

func TestPairSuccessRejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(*waadv.ADVSignedDeviceIdentity, *[]byte)
		want   error
	}{
		"wrong adv secret": {func(_ *waadv.ADVSignedDeviceIdentity, k *[]byte) { *k = bytes.Repeat([]byte{1}, 32) }, ErrHMACMismatch},
		"bad account signature": {func(s *waadv.ADVSignedDeviceIdentity, _ *[]byte) {
			s.AccountSignature = bytes.Clone(s.AccountSignature)
			s.AccountSignature[5] ^= 1
		}, ErrSignatureMismatch},
		"signature by other key": {func(s *waadv.ADVSignedDeviceIdentity, _ *[]byte) {
			other, _ := keys.NewSigningKey()
			s.AccountSignatureKey = other.Pub[:]
		}, ErrSignatureMismatch},
		"truncated key": {func(s *waadv.ADVSignedDeviceIdentity, _ *[]byte) { s.AccountSignatureKey = s.AccountSignatureKey[:31] }, ErrMalformed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			phone, ident, adv := setup(t)
			s, err := ParsePairSuccess(phone.pairSuccess(t, ident.Pub, adv, c.mutate))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Verify(s, ident, adv)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
			en := ErrorNode("x", err)
			e, _ := en.Child("error")
			if code, _ := e.AttrString("code"); (c.want == ErrMalformed) != (code == "500") {
				t.Fatalf("unexpected error code %s", code)
			}
		})
	}
	t.Run("signed for another companion", func(t *testing.T) {
		phone, ident, adv := setup(t)
		other, _ := keys.NewSigningKey()
		s, _ := ParsePairSuccess(phone.pairSuccess(t, other.Pub, adv, nil))
		if _, err := Verify(s, ident, adv); !errors.Is(err, ErrSignatureMismatch) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestQRContentAndRefs(t *testing.T) {
	iq := &wabin.Node{Tag: "iq", Content: []wabin.Node{{Tag: "pair-device", Content: []wabin.Node{
		{Tag: "ref", Content: []byte("2@abc")}, {Tag: "ref", Content: []byte("2@def")},
	}}}}
	refs, err := ParsePairDevice(iq)
	if err != nil || len(refs) != 2 || string(refs[1]) != "2@def" {
		t.Fatalf("refs %q %v", refs, err)
	}
	var n, i, a [32]byte
	n[0], i[0], a[0] = 1, 2, 3
	s := QRContent(refs[0], n, i, a, ClientOtherWeb)
	parts := strings.Split(strings.TrimPrefix(s, QRPrefix), ",")
	if len(parts) != 5 || parts[0] != "2@abc" || parts[4] != "9" {
		t.Fatalf("qr content %q", s)
	}
	if b, _ := base64.StdEncoding.DecodeString(parts[2]); !bytes.Equal(b, i[:]) {
		t.Fatal("identity field wrong")
	}
	if _, err := RenderTerminal(s); err != nil {
		t.Fatal(err)
	}
	if png, err := RenderPNG(s); err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatal("png render failed")
	}
}
