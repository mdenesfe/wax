package signal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/signalstore"
	"github.com/mdenesfe/wax/internal/proto/signalwire"
)

// Sender keys ("group sessions"): each member has one symmetric hash chain
// per group, distributed to the other members' devices over 1:1 sessions in
// a SenderKeyDistributionMessage. Messages are encrypted once with the
// chain and signed with a per-chain Curve25519 key.

const (
	maxSenderKeyStates = 5
	infoGroup          = "WhisperGroup"
)

type senderMessageKey struct {
	iteration uint32
	seed      [32]byte
}

// keys derives IV (16) and cipher key (32) from the message key seed.
func (k senderMessageKey) keys() (iv [16]byte, key [32]byte) {
	out := cryptoutil.HKDF(k.seed[:], nil, infoGroup, 48)
	copy(iv[:], out[:16])
	copy(key[:], out[16:])
	return
}

type groupChain struct {
	iteration uint32
	seed      [32]byte
}

func (c groupChain) messageKey() senderMessageKey {
	ck := chainKey{key: c.seed}
	var mk senderMessageKey
	copy(mk.seed[:], ck.hmac(messageKeySeed))
	mk.iteration = c.iteration
	return mk
}

func (c groupChain) next() groupChain {
	ck := chainKey{key: c.seed}
	var n groupChain
	copy(n.seed[:], ck.hmac(chainKeySeed))
	n.iteration = c.iteration + 1
	return n
}

type senderKeyState struct {
	keyID       uint32
	chain       groupChain
	signingPub  [32]byte
	signingSeed []byte // our own chains only
	skipped     []senderMessageKey
}

func (s *senderKeyState) clone() *senderKeyState {
	c := *s
	c.signingSeed = bytes.Clone(s.signingSeed)
	c.skipped = slices.Clone(s.skipped)
	return &c
}

// SenderKeyRecord holds the sender key chains of one (group, sender) pair.
type SenderKeyRecord struct {
	states []*senderKeyState // newest first
}

// IsEmpty reports whether no chain is known.
func (r *SenderKeyRecord) IsEmpty() bool { return len(r.states) == 0 }

func (r *SenderKeyRecord) state(keyID uint32) *senderKeyState {
	for _, s := range r.states {
		if s.keyID == keyID {
			return s
		}
	}
	return nil
}

// --- distribution ---------------------------------------------------------------

// CreateDistribution returns our SenderKeyDistributionMessage for this
// record, creating a fresh chain on first use. The record must then be
// persisted (it holds our signing key).
func (r *SenderKeyRecord) CreateDistribution() ([]byte, error) {
	if r.IsEmpty() {
		var seed, sigSeed [32]byte
		var idb [4]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return nil, err
		}
		if _, err := rand.Read(sigSeed[:]); err != nil {
			return nil, err
		}
		if _, err := rand.Read(idb[:]); err != nil {
			return nil, err
		}
		sk, err := keys.SigningKeyFromSeed(sigSeed[:])
		if err != nil {
			return nil, err
		}
		r.states = []*senderKeyState{{
			// libsignal uses a positive 31-bit id.
			keyID:       binary.BigEndian.Uint32(idb[:]) & 0x7fffffff,
			chain:       groupChain{iteration: 0, seed: seed},
			signingPub:  sk.Pub,
			signingSeed: sigSeed[:],
		}}
	}
	s := r.states[0]
	body, err := proto.Marshal(&signalwire.SenderKeyDistributionMessage{
		Id:         proto.Uint32(s.keyID),
		Iteration:  proto.Uint32(s.chain.iteration),
		ChainKey:   bytes.Clone(s.chain.seed[:]),
		SigningKey: encodeKey(s.signingPub),
	})
	if err != nil {
		return nil, err
	}
	return append([]byte{versionByte}, body...), nil
}

// ProcessDistribution installs a peer's chain from their distribution
// message. A distribution for a key id we already know is ignored: senders
// redistribute the same chain on retries, and redeliveries replay old
// distributions; replacing the state would rewind the chain and drop
// skipped message keys.
func (r *SenderKeyRecord) ProcessDistribution(msg []byte) error {
	if len(msg) < 2 {
		return fmt.Errorf("%w: distribution message too short", ErrInvalidMessage)
	}
	if v := msg[0] >> 4; v != currentVersion {
		return fmt.Errorf("%w: distribution message version %d", ErrLegacyVersion, v)
	}
	var pb signalwire.SenderKeyDistributionMessage
	if err := proto.Unmarshal(msg[1:], &pb); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if pb.Id == nil || pb.Iteration == nil || len(pb.GetChainKey()) != 32 || pb.SigningKey == nil {
		return fmt.Errorf("%w: incomplete distribution message", ErrInvalidMessage)
	}
	sig, err := decodeKey(pb.GetSigningKey())
	if err != nil {
		return err
	}
	if r.state(pb.GetId()) != nil {
		return nil
	}
	st := &senderKeyState{keyID: pb.GetId(), chain: groupChain{iteration: pb.GetIteration(), seed: [32]byte(pb.GetChainKey())}, signingPub: sig}
	r.states = slices.Insert(r.states, 0, st)
	if len(r.states) > maxSenderKeyStates {
		r.states = r.states[:maxSenderKeyStates]
	}
	return nil
}

// --- encrypt / decrypt ------------------------------------------------------------

// EncryptGroup encrypts a padded plaintext with our newest chain.
func (r *SenderKeyRecord) EncryptGroup(plaintext []byte) ([]byte, error) {
	if r.IsEmpty() || r.states[0].signingSeed == nil {
		return nil, fmt.Errorf("%w: no own sender key", ErrNoSession)
	}
	s := r.states[0]
	sk, err := keys.SigningKeyFromSeed(s.signingSeed)
	if err != nil {
		return nil, err
	}
	mk := s.chain.messageKey()
	iv, key := mk.keys()
	body, err := proto.Marshal(&signalwire.SenderKeyMessage{
		Id:         proto.Uint32(s.keyID),
		Iteration:  proto.Uint32(mk.iteration),
		Ciphertext: cbcEncrypt(key, iv, plaintext),
	})
	if err != nil {
		return nil, err
	}
	out := append([]byte{versionByte}, body...)
	sig := sk.Sign(out)
	s.chain = s.chain.next()
	return append(out, sig[:]...), nil
}

// DecryptGroup verifies and decrypts a SenderKeyMessage. On failure the
// record is unchanged.
func (r *SenderKeyRecord) DecryptGroup(msg []byte) ([]byte, error) {
	if r.IsEmpty() {
		return nil, fmt.Errorf("%w: no sender key", ErrNoSession)
	}
	if len(msg) < 1+keys.SignatureSize+1 {
		return nil, fmt.Errorf("%w: sender key message too short", ErrInvalidMessage)
	}
	if v := msg[0] >> 4; v != currentVersion {
		return nil, fmt.Errorf("%w: sender key message version %d", ErrLegacyVersion, v)
	}
	signed, sig := msg[:len(msg)-keys.SignatureSize], msg[len(msg)-keys.SignatureSize:]
	var pb signalwire.SenderKeyMessage
	if err := proto.Unmarshal(signed[1:], &pb); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if pb.Id == nil || pb.Iteration == nil || pb.Ciphertext == nil {
		return nil, fmt.Errorf("%w: incomplete sender key message", ErrInvalidMessage)
	}
	orig := r.state(pb.GetId())
	if orig == nil {
		return nil, fmt.Errorf("%w: unknown sender key id %d", ErrNoSession, pb.GetId())
	}
	if !keys.Verify(orig.signingPub, signed, [64]byte(sig)) {
		return nil, fmt.Errorf("%w: bad sender key signature", ErrInvalidMessage)
	}
	s := orig.clone()
	mk, err := s.messageKeyFor(pb.GetIteration())
	if err != nil {
		return nil, err
	}
	iv, key := mk.keys()
	pt, err := cbcDecrypt(key, iv, pb.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	*orig = *s
	return pt, nil
}

func (s *senderKeyState) messageKeyFor(iteration uint32) (senderMessageKey, error) {
	if s.chain.iteration > iteration {
		for i, k := range s.skipped {
			if k.iteration == iteration {
				s.skipped = slices.Delete(s.skipped, i, i+1)
				return k, nil
			}
		}
		return senderMessageKey{}, fmt.Errorf("%w: sender chain at %d, message %d", ErrDuplicateMessage, s.chain.iteration, iteration)
	}
	if iteration-s.chain.iteration > maxForwardJump {
		return senderMessageKey{}, fmt.Errorf("%w: sender key message %d is over %d ahead", ErrInvalidMessage, iteration, maxForwardJump)
	}
	c := s.chain
	for c.iteration < iteration {
		s.skipped = append(s.skipped, c.messageKey())
		if len(s.skipped) > maxMessageKeys {
			s.skipped = s.skipped[len(s.skipped)-maxMessageKeys:]
		}
		c = c.next()
	}
	s.chain = c.next()
	return c.messageKey(), nil
}

// --- serialization ------------------------------------------------------------------

// Marshal serializes the record for (encrypted) storage.
func (r *SenderKeyRecord) Marshal() ([]byte, error) {
	pb := &signalstore.SenderKeyRecord{}
	for _, s := range r.states {
		ps := &signalstore.SenderKeyState{
			KeyId: s.keyID, Iteration: s.chain.iteration, ChainSeed: bytes.Clone(s.chain.seed[:]),
			SigningPublic: bytes.Clone(s.signingPub[:]), SigningSeed: bytes.Clone(s.signingSeed),
		}
		for _, k := range s.skipped {
			ps.MessageKeys = append(ps.MessageKeys, &signalstore.SenderMessageKey{Iteration: k.iteration, Seed: bytes.Clone(k.seed[:])})
		}
		pb.States = append(pb.States, ps)
	}
	return proto.Marshal(pb)
}

// UnmarshalSenderKeyRecord parses a stored record.
func UnmarshalSenderKeyRecord(b []byte) (*SenderKeyRecord, error) {
	var pb signalstore.SenderKeyRecord
	if err := proto.Unmarshal(b, &pb); err != nil {
		return nil, fmt.Errorf("signal: decode sender key record: %w", err)
	}
	r := &SenderKeyRecord{}
	for _, ps := range pb.GetStates() {
		seed, err := to32(ps.GetChainSeed(), "sender chain seed")
		if err != nil {
			return nil, err
		}
		pub, err := to32(ps.GetSigningPublic(), "sender signing key")
		if err != nil {
			return nil, err
		}
		s := &senderKeyState{keyID: ps.GetKeyId(), chain: groupChain{iteration: ps.GetIteration(), seed: seed}, signingPub: pub}
		if len(ps.GetSigningSeed()) > 0 {
			s.signingSeed = bytes.Clone(ps.GetSigningSeed())
		}
		for _, k := range ps.GetMessageKeys() {
			ks, err := to32(k.GetSeed(), "sender message key")
			if err != nil {
				return nil, err
			}
			s.skipped = append(s.skipped, senderMessageKey{iteration: k.GetIteration(), seed: ks})
		}
		r.states = append(r.states, s)
	}
	return r, nil
}
