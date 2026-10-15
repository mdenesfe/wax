package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/keys"
)

// SignedPreKey is the medium-term prekey signed by the identity key.
type SignedPreKey struct {
	ID        uint32
	Key       *keys.KeyPair
	Signature [64]byte
}

// Device is the persistent identity of one instance (one linked device).
type Device struct {
	InstanceID     string
	RegistrationID uint32
	NoiseKey       *keys.KeyPair
	IdentityKey    *keys.SigningKey
	SignedPreKey   SignedPreKey
	AdvSecret      [32]byte

	// Set by pairing. JID is the empty JID while the device is unpaired.
	JID          binary.JID
	LID          binary.JID
	Account      []byte // serialized waadv.ADVSignedDeviceIdentity
	Platform     string
	BusinessName string
	PushName     string
	PairedAt     time.Time
}

// IsPaired reports whether the device has completed QR pairing.
func (d *Device) IsPaired() bool { return !d.JID.IsEmpty() }

// NewDevice generates fresh keys for an unpaired instance. It does not write
// to the database; call CreateDevice.
func NewDevice(instanceID string) (*Device, error) {
	noise, err := keys.NewKeyPair()
	if err != nil {
		return nil, err
	}
	identity, err := keys.NewSigningKey()
	if err != nil {
		return nil, err
	}
	spk, err := keys.NewKeyPair()
	if err != nil {
		return nil, err
	}
	// Signal registration ids are 14-bit and non-zero.
	n, err := rand.Int(rand.Reader, big.NewInt(16380))
	if err != nil {
		return nil, err
	}
	d := &Device{
		InstanceID:     instanceID,
		RegistrationID: uint32(n.Int64()) + 1,
		NoiseKey:       noise,
		IdentityKey:    identity,
		SignedPreKey:   SignedPreKey{ID: 1, Key: spk, Signature: identity.SignPublicKey(spk.Pub)},
	}
	if _, err := rand.Read(d.AdvSecret[:]); err != nil {
		return nil, err
	}
	return d, nil
}

const devTable = "wax_devices"

// CreateDevice inserts a new, unpaired device. It fails if the instance
// already has one.
func (s *Store) CreateDevice(ctx context.Context, d *Device) error {
	id := d.InstanceID
	seed := d.IdentityKey.Seed()
	_, err := s.db.Exec(ctx, `INSERT INTO wax_devices
		(instance_id, registration_id, noise_key, identity_seed, signed_prekey_id, signed_prekey, signed_prekey_sig, adv_secret)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, int64(d.RegistrationID),
		s.sealer.seal(d.NoiseKey.Priv[:], adFor(devTable, "noise_key", id)),
		s.sealer.seal(seed[:], adFor(devTable, "identity_seed", id)),
		int64(d.SignedPreKey.ID),
		s.sealer.seal(d.SignedPreKey.Key.Priv[:], adFor(devTable, "signed_prekey", id)),
		d.SignedPreKey.Signature[:],
		s.sealer.seal(d.AdvSecret[:], adFor(devTable, "adv_secret", id)),
	)
	if err != nil {
		return fmt.Errorf("store: create device %q: %w", id, err)
	}
	return nil
}

// SavePairing records the result of a successful QR pairing.
func (s *Store) SavePairing(ctx context.Context, d *Device) error {
	tag, err := s.db.Exec(ctx, `UPDATE wax_devices SET
		jid = $2, lid = $3, account = $4, platform = $5, business_name = $6, paired_at = $7, updated_at = now()
		WHERE instance_id = $1`,
		d.InstanceID, d.JID.String(), nullableJID(d.LID), d.Account, d.Platform, d.BusinessName, d.PairedAt)
	if err != nil {
		return fmt.Errorf("store: save pairing: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: save pairing for %q: %w", d.InstanceID, ErrNotFound)
	}
	return nil
}

// UpdateLID stores the device's LID (learned from the success node).
func (s *Store) UpdateLID(ctx context.Context, instanceID string, lid binary.JID) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_devices SET lid = $2, updated_at = now() WHERE instance_id = $1`,
		instanceID, nullableJID(lid))
	if err != nil {
		return fmt.Errorf("store: update lid: %w", err)
	}
	return nil
}

// UpdatePushName stores our own display name.
func (s *Store) UpdatePushName(ctx context.Context, instanceID, name string) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_devices SET push_name = $2, updated_at = now() WHERE instance_id = $1`, instanceID, name)
	if err != nil {
		return fmt.Errorf("store: update push name: %w", err)
	}
	return nil
}

// DeleteDevice removes an instance's device and, via ON DELETE CASCADE, all
// of its keys and sessions. Used when the session is logged out.
func (s *Store) DeleteDevice(ctx context.Context, instanceID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_devices WHERE instance_id = $1`, instanceID); err != nil {
		return fmt.Errorf("store: delete device: %w", err)
	}
	return nil
}

// LoadDevice reads and decrypts an instance's device. It returns ErrNotFound
// if the instance has none.
func (s *Store) LoadDevice(ctx context.Context, instanceID string) (*Device, error) {
	var (
		regID, spkID                                   int64
		noiseEnc, seedEnc, spkEnc, spkSig, advEnc, acc []byte
		jid, lid                                       *string
		pairedAt                                       *time.Time
		d                                              = &Device{InstanceID: instanceID}
	)
	err := s.db.QueryRow(ctx, `SELECT registration_id, noise_key, identity_seed, signed_prekey_id, signed_prekey,
			signed_prekey_sig, adv_secret, jid, lid, account, platform, business_name, push_name, paired_at
		FROM wax_devices WHERE instance_id = $1`, instanceID).Scan(
		&regID, &noiseEnc, &seedEnc, &spkID, &spkEnc, &spkSig, &advEnc, &jid, &lid, &acc,
		&d.Platform, &d.BusinessName, &d.PushName, &pairedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: device %q: %w", instanceID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: load device %q: %w", instanceID, err)
	}

	noise, err := s.sealer.open(noiseEnc, adFor(devTable, "noise_key", instanceID))
	if err != nil {
		return nil, err
	}
	if d.NoiseKey, err = keys.KeyPairFromPrivate(noise); err != nil {
		return nil, err
	}
	seed, err := s.sealer.open(seedEnc, adFor(devTable, "identity_seed", instanceID))
	if err != nil {
		return nil, err
	}
	if d.IdentityKey, err = keys.SigningKeyFromSeed(seed); err != nil {
		return nil, err
	}
	spk, err := s.sealer.open(spkEnc, adFor(devTable, "signed_prekey", instanceID))
	if err != nil {
		return nil, err
	}
	spkPair, err := keys.KeyPairFromPrivate(spk)
	if err != nil {
		return nil, err
	}
	if len(spkSig) != 64 {
		return nil, fmt.Errorf("store: device %q: signed prekey signature is %d bytes", instanceID, len(spkSig))
	}
	d.SignedPreKey = SignedPreKey{ID: uint32(spkID), Key: spkPair, Signature: [64]byte(spkSig)}
	adv, err := s.sealer.open(advEnc, adFor(devTable, "adv_secret", instanceID))
	if err != nil {
		return nil, err
	}
	if len(adv) != 32 {
		return nil, fmt.Errorf("store: device %q: adv secret is %d bytes", instanceID, len(adv))
	}
	d.AdvSecret = [32]byte(adv)
	d.RegistrationID = uint32(regID)
	d.Account = acc
	if pairedAt != nil {
		d.PairedAt = *pairedAt
	}
	if jid != nil {
		if d.JID, err = binary.ParseJID(*jid); err != nil {
			return nil, fmt.Errorf("store: device %q: %w", instanceID, err)
		}
	}
	if lid != nil {
		if d.LID, err = binary.ParseJID(*lid); err != nil {
			return nil, fmt.Errorf("store: device %q: %w", instanceID, err)
		}
	}
	return d, nil
}

func nullableJID(j binary.JID) *string {
	if j.IsEmpty() {
		return nil
	}
	s := j.String()
	return &s
}

// PutIdentity stores (or replaces) the identity key of a remote device.
func (s *Store) PutIdentity(ctx context.Context, instanceID, address string, key [32]byte) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_identities (instance_id, address, identity_key) VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, address) DO UPDATE SET identity_key = excluded.identity_key, updated_at = now()`,
		instanceID, address, key[:])
	if err != nil {
		return fmt.Errorf("store: put identity: %w", err)
	}
	return nil
}

// GetIdentity returns the stored identity key of a remote device.
func (s *Store) GetIdentity(ctx context.Context, instanceID, address string) ([32]byte, error) {
	var b []byte
	err := s.db.QueryRow(ctx, `SELECT identity_key FROM wax_identities WHERE instance_id = $1 AND address = $2`,
		instanceID, address).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return [32]byte{}, ErrNotFound
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("store: get identity: %w", err)
	}
	if len(b) != 32 {
		return [32]byte{}, fmt.Errorf("store: identity for %s is %d bytes", address, len(b))
	}
	return [32]byte(b), nil
}
