package signal

import (
	"errors"
	"fmt"

	"github.com/mdenesfe/wax/internal/keys"
)

// LocalIdentity is our side of every session.
type LocalIdentity struct {
	Key            *keys.KeyPair // identity key pair (the X25519 part of our signing key)
	RegistrationID uint32
}

// PreKeyBundle is a remote device's published key material, fetched from
// the server before the first message to that device.
type PreKeyBundle struct {
	RegistrationID        uint32
	IdentityKey           [32]byte
	SignedPreKeyID        uint32
	SignedPreKey          [32]byte
	SignedPreKeySignature [64]byte
	PreKeyID              *uint32 // nil if the server had no one-time prekey left
	PreKey                [32]byte
}

// PreKeyStore gives the cipher access to our private prekeys when a peer
// starts a session with us.
type PreKeyStore interface {
	SignedPreKey(id uint32) (*keys.KeyPair, error)
	// PreKey returns ErrMissingPreKey (wrapped) if the key does not exist.
	PreKey(id uint32) (*keys.KeyPair, error)
}

// ProcessBundle starts a new session as initiator (X3DH "Alice"). The
// signed prekey signature is verified first. Any existing session is
// archived. Until the peer answers, Encrypt produces PreKey messages.
func (r *Record) ProcessBundle(local LocalIdentity, b *PreKeyBundle) error {
	signed := append([]byte{keys.DJBType}, b.SignedPreKey[:]...)
	if !keys.Verify(b.IdentityKey, signed, b.SignedPreKeySignature) {
		return ErrInvalidSignature
	}
	base, err := keys.NewKeyPair()
	if err != nil {
		return err
	}
	sending, err := keys.NewKeyPair()
	if err != nil {
		return err
	}
	var opk *[32]byte
	if b.PreKeyID != nil {
		opk = &b.PreKey
	}
	st, err := initAlice(local.Key, base, b.IdentityKey, b.SignedPreKey, b.SignedPreKey, opk, sending)
	if err != nil {
		return err
	}
	st.pending = &pendingPreKey{signedPreKeyID: b.SignedPreKeyID, baseKey: base.Pub}
	if b.PreKeyID != nil {
		id := *b.PreKeyID
		st.pending.preKeyID = &id
	}
	st.localRegID = local.RegistrationID
	st.remoteRegID = b.RegistrationID
	st.aliceBaseKey = base.Pub
	r.archiveCurrent()
	r.current = st
	return nil
}

// initAlice derives the initiator's session (separated from ProcessBundle so
// tests can inject the random keys).
//
//	master = 0xFF*32 || DH(IKa, SPKb) || DH(EKa, IKb) || DH(EKa, SPKb) [|| DH(EKa, OPKb)]
func initAlice(ourIdentity, ourBase *keys.KeyPair, theirIdentity, theirSignedPreKey, theirRatchet [32]byte, theirOneTime *[32]byte, sending *keys.KeyPair) (*sessionState, error) {
	secret := append([]byte{}, discontinuity...)
	pairs := []struct{ priv, pub [32]byte }{
		{ourIdentity.Priv, theirSignedPreKey},
		{ourBase.Priv, theirIdentity},
		{ourBase.Priv, theirSignedPreKey},
	}
	if theirOneTime != nil {
		pairs = append(pairs, struct{ priv, pub [32]byte }{ourBase.Priv, *theirOneTime})
	}
	for _, p := range pairs {
		s, err := keys.DH(p.priv, p.pub)
		if err != nil {
			return nil, fmt.Errorf("%w: x3dh: %w", ErrInvalidMessage, err)
		}
		secret = append(secret, s[:]...)
	}
	rk, ck := deriveInitialKeys(secret)
	// The initiator immediately performs a DH ratchet step so its first
	// message already uses a fresh ratchet key.
	nrk, sendCK, err := rk.createChain(theirRatchet, sending)
	if err != nil {
		return nil, err
	}
	st := &sessionState{
		version:        currentVersion,
		localIdentity:  ourIdentity.Pub,
		remoteIdentity: theirIdentity,
		root:           nrk,
		sender:         &senderChain{ratchet: *sending, chain: sendCK},
	}
	st.addReceiver(theirRatchet, ck)
	return st, nil
}

// initBob derives the responder's session.
//
//	master = 0xFF*32 || DH(SPKb, IKa) || DH(IKb, EKa) || DH(SPKb, EKa) [|| DH(OPKb, EKa)]
func initBob(ourIdentity, ourSignedPreKey, ourRatchet, ourOneTime *keys.KeyPair, theirIdentity, theirBase [32]byte) (*sessionState, error) {
	secret := append([]byte{}, discontinuity...)
	pairs := []struct{ priv, pub [32]byte }{
		{ourSignedPreKey.Priv, theirIdentity},
		{ourIdentity.Priv, theirBase},
		{ourSignedPreKey.Priv, theirBase},
	}
	if ourOneTime != nil {
		pairs = append(pairs, struct{ priv, pub [32]byte }{ourOneTime.Priv, theirBase})
	}
	for _, p := range pairs {
		s, err := keys.DH(p.priv, p.pub)
		if err != nil {
			return nil, fmt.Errorf("%w: x3dh: %w", ErrInvalidMessage, err)
		}
		secret = append(secret, s[:]...)
	}
	rk, ck := deriveInitialKeys(secret)
	return &sessionState{
		version:        currentVersion,
		localIdentity:  ourIdentity.Pub,
		remoteIdentity: theirIdentity,
		root:           rk,
		sender:         &senderChain{ratchet: *ourRatchet, chain: ck},
	}, nil
}

// Encrypt encrypts an (already padded) plaintext with the current session.
func (r *Record) Encrypt(plaintext []byte) (MessageType, []byte, error) {
	s := r.current
	if s == nil || s.sender == nil {
		return 0, nil, ErrNoSession
	}
	ck := s.sender.chain
	mk := ck.messageKeys()
	ct := cbcEncrypt(mk.cipherKey, mk.iv, plaintext)
	msg, err := buildSignalMessage(mk.macKey, s.sender.ratchet.Pub, ck.index, s.previousCounter, ct, s.localIdentity, s.remoteIdentity)
	if err != nil {
		return 0, nil, err
	}
	typ := TypeMessage
	if s.pending != nil {
		if msg, err = buildPreKeyMessage(s.localRegID, s.pending, s.localIdentity, msg); err != nil {
			return 0, nil, err
		}
		typ = TypePreKey
	}
	s.sender.chain = ck.next()
	return typ, msg, nil
}

// DecryptMessage decrypts a SignalMessage ("msg"). On success the record is
// updated and must be persisted; on failure it is unchanged.
func (r *Record) DecryptMessage(ciphertext []byte) ([]byte, error) {
	if r.current == nil {
		return nil, ErrNoSession
	}
	m, err := parseSignalMessage(ciphertext)
	if err != nil {
		return nil, err
	}
	return r.decrypt(m)
}

// PreKeyResult describes the outcome of DecryptPreKeyMessage.
type PreKeyResult struct {
	Plaintext []byte
	// UsedPreKeyID is the one-time prekey consumed by a newly created
	// session; the caller must delete it from storage (in the same
	// transaction as saving the record).
	UsedPreKeyID   *uint32
	RemoteIdentity [32]byte
	// NewSession is true when the message established a new session.
	NewSession bool
}

// DecryptPreKeyMessage decrypts a PreKeySignalMessage ("pkmsg"), building the
// session from it unless a session with the same base key already exists
// (i.e. the peer resent before seeing our reply).
func (r *Record) DecryptPreKeyMessage(local LocalIdentity, store PreKeyStore, ciphertext []byte) (*PreKeyResult, error) {
	pm, err := parsePreKeyMessage(ciphertext)
	if err != nil {
		return nil, err
	}
	res := &PreKeyResult{RemoteIdentity: pm.identityKey}
	// Work on a copy: nothing may change if decryption fails.
	work := r.clone()
	if !work.hasSessionWithBaseKey(uint32(pm.version), pm.baseKey) {
		spk, err := store.SignedPreKey(pm.signedPreKeyID)
		if err != nil {
			return nil, fmt.Errorf("signed prekey %d: %w", pm.signedPreKeyID, err)
		}
		var opk *keys.KeyPair
		if pm.preKeyID != nil {
			if opk, err = store.PreKey(*pm.preKeyID); err != nil {
				return nil, fmt.Errorf("prekey %d: %w", *pm.preKeyID, err)
			}
		}
		st, err := initBob(local.Key, spk, spk, opk, pm.identityKey, pm.baseKey)
		if err != nil {
			return nil, err
		}
		st.localRegID = local.RegistrationID
		st.remoteRegID = pm.registrationID
		st.aliceBaseKey = pm.baseKey
		work.archiveCurrent()
		work.current = st
		res.NewSession = true
		if pm.preKeyID != nil {
			id := *pm.preKeyID
			res.UsedPreKeyID = &id
		}
	}
	pt, err := work.decrypt(pm.message)
	if err != nil {
		return nil, err
	}
	*r = *work
	res.Plaintext = pt
	return res, nil
}

func (r *Record) clone() *Record {
	c := &Record{}
	if r.current != nil {
		c.current = r.current.clone()
	}
	for _, p := range r.previous {
		c.previous = append(c.previous, p.clone())
	}
	return c
}

// decrypt tries the current session, then archived ones; a matching
// archived session is promoted to current.
func (r *Record) decrypt(m *signalMessage) ([]byte, error) {
	var errs []error
	if r.current != nil {
		st := r.current.clone()
		pt, err := decryptWithState(st, m)
		if err == nil {
			r.current = st
			return pt, nil
		}
		if errors.Is(err, ErrDuplicateMessage) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("current: %w", err))
	}
	for i, prev := range r.previous {
		st := prev.clone()
		pt, err := decryptWithState(st, m)
		if err == nil {
			r.promote(i, st)
			return pt, nil
		}
		if errors.Is(err, ErrDuplicateMessage) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("previous %d: %w", i, err))
	}
	if len(errs) == 0 {
		return nil, ErrNoSession
	}
	return nil, fmt.Errorf("%w: no session could decrypt: %w", ErrInvalidMessage, errors.Join(errs...))
}

func decryptWithState(s *sessionState, m *signalMessage) ([]byte, error) {
	if s.sender == nil {
		return nil, fmt.Errorf("%w: uninitialized session", ErrInvalidMessage)
	}
	if uint32(m.version) != s.version {
		return nil, fmt.Errorf("%w: message version %d, session version %d", ErrInvalidMessage, m.version, s.version)
	}
	rc, err := getOrCreateReceiver(s, m.ratchetKey)
	if err != nil {
		return nil, err
	}
	mk, err := getOrCreateMessageKeys(rc, m.counter)
	if err != nil {
		return nil, err
	}
	if err := m.verifyMAC(s.remoteIdentity, s.localIdentity, mk.macKey); err != nil {
		return nil, err
	}
	pt, err := cbcDecrypt(mk.cipherKey, mk.iv, m.ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	// The peer has received our pre-key message (it replied), so plain
	// messages are enough from now on.
	s.pending = nil
	return pt, nil
}

// getOrCreateReceiver returns the receiver chain for the peer's ratchet key,
// performing a DH ratchet step if it is new.
func getOrCreateReceiver(s *sessionState, theirRatchet [32]byte) (*receiverChain, error) {
	if rc := s.receiver(theirRatchet); rc != nil {
		return rc, nil
	}
	nrk, recvCK, err := s.root.createChain(theirRatchet, &s.sender.ratchet)
	if err != nil {
		return nil, err
	}
	ourNew, err := keys.NewKeyPair()
	if err != nil {
		return nil, err
	}
	nrk2, sendCK, err := nrk.createChain(theirRatchet, ourNew)
	if err != nil {
		return nil, err
	}
	s.root = nrk2
	s.addReceiver(theirRatchet, recvCK)
	if s.sender.chain.index > 0 {
		s.previousCounter = s.sender.chain.index - 1
	} else {
		s.previousCounter = 0
	}
	s.sender = &senderChain{ratchet: *ourNew, chain: sendCK}
	return s.receiver(theirRatchet), nil
}

func getOrCreateMessageKeys(rc *receiverChain, counter uint32) (messageKeys, error) {
	ck := rc.chain
	if ck.index > counter {
		for i, mk := range rc.skipped {
			if mk.index == counter {
				rc.skipped = append(rc.skipped[:i], rc.skipped[i+1:]...)
				return mk, nil
			}
		}
		return messageKeys{}, fmt.Errorf("%w: chain at %d, message %d", ErrDuplicateMessage, ck.index, counter)
	}
	if counter-ck.index > maxForwardJump {
		return messageKeys{}, fmt.Errorf("%w: message %d is over %d messages ahead", ErrInvalidMessage, counter, maxForwardJump)
	}
	for ck.index < counter {
		rc.skipped = append(rc.skipped, ck.messageKeys())
		if len(rc.skipped) > maxMessageKeys {
			rc.skipped = rc.skipped[len(rc.skipped)-maxMessageKeys:]
		}
		ck = ck.next()
	}
	rc.chain = ck.next()
	return ck.messageKeys(), nil
}
