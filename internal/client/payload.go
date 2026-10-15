// Package client manages a single WhatsApp linked-device session. Phase 1
// contains the pieces the handshake needs: the ClientPayload builders and the
// classification of server-initiated disconnects.
package client

import (
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wa6"
	"github.com/mdenesfe/wax/internal/proto/wacompanionreg"
	"github.com/mdenesfe/wax/internal/waversion"
)

// djbKeyType is Signal's type byte for Curve25519 public keys. Serialized
// Signal public keys are prefixed with it, including when signed.
const djbKeyType = 0x05

// DeviceConfig describes how this companion presents itself.
type DeviceConfig struct {
	Version waversion.Version
	// OSName is shown in the phone's "Linked devices" list.
	OSName string
	// Platform selects the icon in the phone's list (CHROME, DESKTOP, ...).
	Platform wacompanionreg.DeviceProps_PlatformType
	// Locale, e.g. "tr" / "TR".
	Language, Country string
}

// DefaultDeviceConfig returns a sensible configuration for the given version.
func DefaultDeviceConfig(v waversion.Version) DeviceConfig {
	return DeviceConfig{
		Version:  v,
		OSName:   "wax",
		Platform: wacompanionreg.DeviceProps_DESKTOP,
		Language: "en",
		Country:  "US",
	}
}

// RegistrationKeys is the public key material announced during registration.
type RegistrationKeys struct {
	RegistrationID  uint32
	IdentityKey     [32]byte
	SignedPreKeyID  uint32
	SignedPreKey    [32]byte
	SignedPreKeySig [64]byte
}

// SignPreKey signs a prekey public key the way Signal does: over the
// type-prefixed serialization 0x05 || pub.
func SignPreKey(identity *keys.SigningKey, preKeyPub [32]byte) [64]byte {
	msg := make([]byte, 0, 33)
	msg = append(msg, djbKeyType)
	msg = append(msg, preKeyPub[:]...)
	return identity.Sign(msg)
}

func basePayload(cfg DeviceConfig) *wa6.ClientPayload {
	v := cfg.Version
	return &wa6.ClientPayload{
		UserAgent: &wa6.ClientPayload_UserAgent{
			Platform:       wa6.ClientPayload_UserAgent_WEB.Enum(),
			ReleaseChannel: wa6.ClientPayload_UserAgent_RELEASE.Enum(),
			AppVersion: &wa6.ClientPayload_UserAgent_AppVersion{
				Primary: proto.Uint32(v[0]), Secondary: proto.Uint32(v[1]), Tertiary: proto.Uint32(v[2]),
			},
			// Web clients send placeholder carrier/OS values.
			Mcc:                         proto.String("000"),
			Mnc:                         proto.String("000"),
			OsVersion:                   proto.String("0.1"),
			Manufacturer:                proto.String(""),
			Device:                      proto.String("Desktop"),
			OsBuildNumber:               proto.String("0.1"),
			LocaleLanguageIso6391:       proto.String(cfg.Language),
			LocaleCountryIso31661Alpha2: proto.String(cfg.Country),
		},
		WebInfo: &wa6.ClientPayload_WebInfo{
			WebSubPlatform: wa6.ClientPayload_WebInfo_WEB_BROWSER.Enum(),
		},
		ConnectType:   wa6.ClientPayload_WIFI_UNKNOWN.Enum(),
		ConnectReason: wa6.ClientPayload_USER_ACTIVATED.Enum(),
	}
}

// RegistrationPayload builds the ClientPayload for a device that is not yet
// paired. The server answers it with a pair-device IQ carrying QR refs.
func RegistrationPayload(cfg DeviceConfig, k RegistrationKeys) ([]byte, error) {
	props, err := proto.Marshal(&wacompanionreg.DeviceProps{
		Os: proto.String(cfg.OSName),
		Version: &wacompanionreg.DeviceProps_AppVersion{
			Primary: proto.Uint32(0), Secondary: proto.Uint32(1), Tertiary: proto.Uint32(0),
		},
		PlatformType:    cfg.Platform.Enum(),
		RequireFullSync: proto.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal device props: %w", err)
	}
	var regID [4]byte
	binary.BigEndian.PutUint32(regID[:], k.RegistrationID)
	var skeyID [4]byte
	binary.BigEndian.PutUint32(skeyID[:], k.SignedPreKeyID)
	buildHash := cfg.Version.BuildHash()

	p := basePayload(cfg)
	p.Passive = proto.Bool(false)
	p.Pull = proto.Bool(false)
	p.DevicePairingData = &wa6.ClientPayload_DevicePairingRegistrationData{
		ERegid:      regID[:],
		EKeytype:    []byte{djbKeyType},
		EIdent:      k.IdentityKey[:],
		ESkeyID:     skeyID[1:], // the signed prekey id is sent as 3 bytes
		ESkeyVal:    k.SignedPreKey[:],
		ESkeySig:    k.SignedPreKeySig[:],
		BuildHash:   buildHash[:],
		DeviceProps: props,
	}
	return proto.Marshal(p)
}

// LoginPayload builds the ClientPayload for an already paired device.
func LoginPayload(cfg DeviceConfig, user uint64, device uint32) ([]byte, error) {
	p := basePayload(cfg)
	p.Username = proto.Uint64(user)
	p.Device = proto.Uint32(device)
	p.Passive = proto.Bool(true)
	p.Pull = proto.Bool(true)
	return proto.Marshal(p)
}
