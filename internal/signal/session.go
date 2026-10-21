package signal

import (
	"bytes"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/proto/signalstore"
)

type senderChain struct {
	ratchet keys.KeyPair
	chain   chainKey
}

type receiverChain struct {
	ratchetPub [32]byte
	chain      chainKey
	skipped    []messageKeys // oldest first
}

type pendingPreKey struct {
	preKeyID       *uint32
	signedPreKeyID uint32
	baseKey        [32]byte
}

// sessionState is one ratchet session with a remote device.
type sessionState struct {
	version         uint32
	localIdentity   [32]byte
	remoteIdentity  [32]byte
	root            rootKey
	previousCounter uint32
	sender          *senderChain
	receivers       []receiverChain // newest last
	pending         *pendingPreKey
	remoteRegID     uint32
	localRegID      uint32
	aliceBaseKey    [32]byte
}

func (s *sessionState) clone() *sessionState {
	c := *s
	if s.sender != nil {
		sc := *s.sender
		c.sender = &sc
	}
	c.receivers = make([]receiverChain, len(s.receivers))
	for i, r := range s.receivers {
		r.skipped = slices.Clone(r.skipped)
		c.receivers[i] = r
	}
	if s.pending != nil {
		p := *s.pending
		if s.pending.preKeyID != nil {
			id := *s.pending.preKeyID
			p.preKeyID = &id
		}
		c.pending = &p
	}
	return &c
}

func (s *sessionState) receiver(ratchet [32]byte) *receiverChain {
	for i := range s.receivers {
		if s.receivers[i].ratchetPub == ratchet {
			return &s.receivers[i]
		}
	}
	return nil
}

func (s *sessionState) addReceiver(ratchet [32]byte, ck chainKey) {
	s.receivers = append(s.receivers, receiverChain{ratchetPub: ratchet, chain: ck})
	if len(s.receivers) > maxReceiverChain {
		s.receivers = slices.Delete(s.receivers, 0, len(s.receivers)-maxReceiverChain)
	}
}

// Record holds the current session with one remote device plus archived
// earlier sessions (kept so late messages of a replaced session still
// decrypt). The zero value is an empty ("fresh") record.
type Record struct {
	current  *sessionState
	previous []*sessionState // newest first
}

// IsFresh reports whether the record has no session at all.
func (r *Record) IsFresh() bool { return r.current == nil }

// HasSession reports whether the record can encrypt.
func (r *Record) HasSession() bool { return r.current != nil && r.current.sender != nil }

// RemoteIdentity returns the identity key of the current session.
func (r *Record) RemoteIdentity() ([32]byte, bool) {
	if r.current == nil {
		return [32]byte{}, false
	}
	return r.current.remoteIdentity, true
}

// RemoteRegistrationID returns the peer's registration id of the current session.
func (r *Record) RemoteRegistrationID() uint32 {
	if r.current == nil {
		return 0
	}
	return r.current.remoteRegID
}

func (r *Record) archiveCurrent() {
	if r.current == nil {
		return
	}
	r.previous = slices.Insert(r.previous, 0, r.current)
	if len(r.previous) > maxArchivedState {
		r.previous = r.previous[:maxArchivedState]
	}
	r.current = nil
}

func (r *Record) promote(i int, s *sessionState) {
	r.previous = slices.Delete(r.previous, i, i+1)
	r.archiveCurrent()
	r.current = s
}

func (r *Record) hasSessionWithBaseKey(version uint32, base [32]byte) bool {
	if r.current != nil && r.current.version == version && r.current.aliceBaseKey == base {
		return true
	}
	for _, p := range r.previous {
		if p.version == version && p.aliceBaseKey == base {
			return true
		}
	}
	return false
}

// --- serialization --------------------------------------------------------

// Marshal serializes the record for storage. It contains private keys and
// must be stored encrypted.
func (r *Record) Marshal() ([]byte, error) {
	pb := &signalstore.SessionRecord{}
	if r.current != nil {
		pb.Current = stateToProto(r.current)
	}
	for _, p := range r.previous {
		pb.Previous = append(pb.Previous, stateToProto(p))
	}
	return proto.Marshal(pb)
}

// UnmarshalRecord parses a stored record.
func UnmarshalRecord(b []byte) (*Record, error) {
	var pb signalstore.SessionRecord
	if err := proto.Unmarshal(b, &pb); err != nil {
		return nil, fmt.Errorf("signal: decode record: %w", err)
	}
	r := &Record{}
	var err error
	if pb.Current != nil {
		if r.current, err = stateFromProto(pb.Current); err != nil {
			return nil, err
		}
	}
	for _, p := range pb.Previous {
		s, err := stateFromProto(p)
		if err != nil {
			return nil, err
		}
		r.previous = append(r.previous, s)
	}
	return r, nil
}

func ckToProto(c chainKey) *signalstore.ChainKey {
	return &signalstore.ChainKey{Index: c.index, Key: bytes.Clone(c.key[:])}
}

func stateToProto(s *sessionState) *signalstore.SessionState {
	pb := &signalstore.SessionState{
		Version:              s.version,
		LocalIdentity:        bytes.Clone(s.localIdentity[:]),
		RemoteIdentity:       bytes.Clone(s.remoteIdentity[:]),
		RootKey:              bytes.Clone(s.root[:]),
		PreviousCounter:      s.previousCounter,
		RemoteRegistrationId: s.remoteRegID,
		LocalRegistrationId:  s.localRegID,
		AliceBaseKey:         bytes.Clone(s.aliceBaseKey[:]),
	}
	if s.sender != nil {
		pb.SenderChain = &signalstore.SenderChain{
			RatchetPublic:  bytes.Clone(s.sender.ratchet.Pub[:]),
			RatchetPrivate: bytes.Clone(s.sender.ratchet.Priv[:]),
			ChainKey:       ckToProto(s.sender.chain),
		}
	}
	for _, rc := range s.receivers {
		prc := &signalstore.ReceiverChain{RatchetPublic: bytes.Clone(rc.ratchetPub[:]), ChainKey: ckToProto(rc.chain)}
		for _, mk := range rc.skipped {
			prc.MessageKeys = append(prc.MessageKeys, &signalstore.MessageKey{
				Index: mk.index, CipherKey: bytes.Clone(mk.cipherKey[:]), MacKey: bytes.Clone(mk.macKey[:]), Iv: bytes.Clone(mk.iv[:]),
			})
		}
		pb.ReceiverChains = append(pb.ReceiverChains, prc)
	}
	if s.pending != nil {
		pb.PendingPreKey = &signalstore.PendingPreKey{SignedPreKeyId: s.pending.signedPreKeyID, BaseKey: bytes.Clone(s.pending.baseKey[:])}
		if s.pending.preKeyID != nil {
			pb.PendingPreKey.HasPreKeyId = true
			pb.PendingPreKey.PreKeyId = *s.pending.preKeyID
		}
	}
	return pb
}

func to32(b []byte, what string) ([32]byte, error) {
	if len(b) != 32 {
		return [32]byte{}, fmt.Errorf("signal: stored %s is %d bytes", what, len(b))
	}
	return [32]byte(b), nil
}

func ckFromProto(p *signalstore.ChainKey) (chainKey, error) {
	k, err := to32(p.GetKey(), "chain key")
	return chainKey{key: k, index: p.GetIndex()}, err
}

// storedKeyPair rebuilds a sender ratchet key pair, trusting the stored
// public key (records come from sealed storage); records without one are
// re-derived.
func storedKeyPair(priv, pub []byte) (*keys.KeyPair, error) {
	p, err := to32(priv, "sender ratchet private key")
	if err != nil {
		return nil, err
	}
	if len(pub) != 32 {
		return keys.KeyPairFromPrivate(p[:])
	}
	kp := keys.KeyPairFromStored(p, [32]byte(pub))
	return &kp, nil
}

func stateFromProto(pb *signalstore.SessionState) (*sessionState, error) {
	s := &sessionState{
		version: pb.GetVersion(), previousCounter: pb.GetPreviousCounter(),
		remoteRegID: pb.GetRemoteRegistrationId(), localRegID: pb.GetLocalRegistrationId(),
	}
	var err error
	if s.localIdentity, err = to32(pb.GetLocalIdentity(), "local identity"); err != nil {
		return nil, err
	}
	if s.remoteIdentity, err = to32(pb.GetRemoteIdentity(), "remote identity"); err != nil {
		return nil, err
	}
	var rk [32]byte
	if rk, err = to32(pb.GetRootKey(), "root key"); err != nil {
		return nil, err
	}
	s.root = rk
	if len(pb.GetAliceBaseKey()) > 0 {
		if s.aliceBaseKey, err = to32(pb.GetAliceBaseKey(), "alice base key"); err != nil {
			return nil, err
		}
	}
	if sc := pb.GetSenderChain(); sc != nil {
		kp, err := storedKeyPair(sc.GetRatchetPrivate(), sc.GetRatchetPublic())
		if err != nil {
			return nil, err
		}
		ck, err := ckFromProto(sc.GetChainKey())
		if err != nil {
			return nil, err
		}
		s.sender = &senderChain{ratchet: *kp, chain: ck}
	}
	for _, prc := range pb.GetReceiverChains() {
		pub, err := to32(prc.GetRatchetPublic(), "receiver ratchet key")
		if err != nil {
			return nil, err
		}
		ck, err := ckFromProto(prc.GetChainKey())
		if err != nil {
			return nil, err
		}
		rc := receiverChain{ratchetPub: pub, chain: ck}
		for _, m := range prc.GetMessageKeys() {
			var mk messageKeys
			if len(m.GetCipherKey()) != 32 || len(m.GetMacKey()) != 32 || len(m.GetIv()) != 16 {
				return nil, fmt.Errorf("signal: stored message key has bad lengths")
			}
			copy(mk.cipherKey[:], m.GetCipherKey())
			copy(mk.macKey[:], m.GetMacKey())
			copy(mk.iv[:], m.GetIv())
			mk.index = m.GetIndex()
			rc.skipped = append(rc.skipped, mk)
		}
		s.receivers = append(s.receivers, rc)
	}
	if pp := pb.GetPendingPreKey(); pp != nil {
		base, err := to32(pp.GetBaseKey(), "pending base key")
		if err != nil {
			return nil, err
		}
		s.pending = &pendingPreKey{signedPreKeyID: pp.GetSignedPreKeyId(), baseKey: base}
		if pp.GetHasPreKeyId() {
			id := pp.GetPreKeyId()
			s.pending.preKeyID = &id
		}
	}
	return s, nil
}
