package client

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/internal/waversion"
	"github.com/mdenesfe/wax/proto/wa6"
)

func TestRegistrationPayload(t *testing.T) {
	ident, _ := keys.NewSigningKey()
	spk, _ := keys.NewKeyPair()
	k := RegistrationKeys{
		RegistrationID: 0x01020304, IdentityKey: ident.Pub,
		SignedPreKeyID: 1, SignedPreKey: spk.Pub, SignedPreKeySig: ident.SignPublicKey(spk.Pub),
	}
	raw, err := RegistrationPayload(DefaultDeviceConfig(waversion.Default), k)
	if err != nil {
		t.Fatal(err)
	}
	var p wa6.ClientPayload
	if err := proto.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	d := p.GetDevicePairingData()
	if string(d.GetERegid()) != "\x01\x02\x03\x04" || string(d.GetESkeyID()) != "\x00\x00\x01" || string(d.GetEKeytype()) != "\x05" {
		t.Fatalf("bad encoding: %x %x %x", d.GetERegid(), d.GetESkeyID(), d.GetEKeytype())
	}
	// The signature must verify the way the phone checks it.
	if !keys.VerifySlice(d.GetEIdent(), append([]byte{5}, d.GetESkeyVal()...), d.GetESkeySig()) {
		t.Fatal("signed prekey signature does not verify")
	}
	if p.GetUserAgent().GetAppVersion().GetTertiary() != waversion.Default[2] || p.GetPassive() {
		t.Fatal("unexpected user agent / passive")
	}
}

func TestClassifyDisconnect(t *testing.T) {
	cases := []struct {
		node wabin.Node
		want DisconnectAction
	}{
		{wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "515"}}, ActionReconnectNow},
		{wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "503"}}, ActionReconnect},
		{wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "401"}, Content: []wabin.Node{{Tag: "conflict", Attrs: wabin.Attrs{"type": "device_removed"}}}}, ActionLoggedOut},
		{wabin.Node{Tag: "stream:error", Content: []wabin.Node{{Tag: "conflict", Attrs: wabin.Attrs{"type": "replaced"}}}}, ActionReplaced},
		{wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "401"}}, ActionLoggedOut},
		{wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "405"}}, ActionUpgradeRequired},
		{wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "402"}}, ActionTempBanned},
		{wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "503"}}, ActionReconnect},
		{wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "999"}}, ActionFatal},
		{wabin.Node{Tag: "iq"}, ActionNone},
	}
	for i, c := range cases {
		if got := ClassifyDisconnect(&c.node).Action; got != c.want {
			t.Errorf("case %d (%s): got %s want %s", i, c.node.String(), got, c.want)
		}
	}
}

func TestConnHeaderDictVersion(t *testing.T) {
	if ConnHeader[3] != wabin.DictVersion {
		t.Fatal("connection header must carry the token dictionary version")
	}
}
