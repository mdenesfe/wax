package binary

import (
	"fmt"
	"strconv"
	"strings"
)

// Well-known JID servers.
const (
	ServerUser       = "s.whatsapp.net" // phone-number based users
	ServerGroup      = "g.us"
	ServerLegacyUser = "c.us"
	ServerBroadcast  = "broadcast"
	ServerLID        = "lid" // "linked identity" (hidden user) addressing
	ServerMessenger  = "msgr"
	ServerInterop    = "interop"
	ServerNewsletter = "newsletter"
	ServerHosted     = "hosted"
	ServerHostedLID  = "hosted.lid"
	ServerBot        = "bot"
)

// Agent ("domain type") values used by AD JIDs on the wire.
const (
	agentWhatsApp  = 0
	agentLID       = 1
	agentHosted    = 128
	agentHostedLID = 129
)

// ServerJID is the JID of the WhatsApp server itself (s.whatsapp.net).
var ServerJID = JID{Server: ServerUser}

// JID is a WhatsApp address: user[.agent][:device]@server.
type JID struct {
	User       string
	RawAgent   uint8  // only meaningful for s.whatsapp.net AD JIDs with a non-standard agent
	Device     uint16 // 0 = primary device (phone)
	Integrator uint16 // interop JIDs only
	Server     string
}

// IsEmpty reports whether the JID is the zero value.
func (j JID) IsEmpty() bool { return j.Server == "" }

// ToNonAD strips the device and agent parts, yielding the user's JID.
func (j JID) ToNonAD() JID {
	return JID{User: j.User, Server: j.Server, Integrator: j.Integrator}
}

// agent returns the on-wire agent byte for AD encoding.
func (j JID) agent() uint8 {
	switch j.Server {
	case ServerLID:
		return agentLID
	case ServerHosted:
		return agentHosted
	case ServerHostedLID:
		return agentHostedLID
	default:
		return j.RawAgent
	}
}

// String renders the JID in its canonical textual form.
func (j JID) String() string {
	if j.User == "" {
		return j.Server
	}
	var b strings.Builder
	b.WriteString(j.User)
	if j.RawAgent > 0 {
		b.WriteByte('.')
		b.WriteString(strconv.Itoa(int(j.RawAgent)))
	}
	if j.Device > 0 {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(int(j.Device)))
	}
	b.WriteByte('@')
	b.WriteString(j.Server)
	return b.String()
}

// ParseJID parses the textual form produced by JID.String.
func ParseJID(s string) (JID, error) {
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return JID{Server: s}, nil
	}
	j := JID{Server: s[at+1:]}
	user := s[:at]
	if colon := strings.LastIndexByte(user, ':'); colon >= 0 {
		dev, err := strconv.ParseUint(user[colon+1:], 10, 16)
		if err != nil {
			return JID{}, fmt.Errorf("%w: bad device in %q: %w", ErrInvalidJID, s, err)
		}
		j.Device = uint16(dev)
		user = user[:colon]
	}
	if j.Server == ServerUser {
		if us := strings.LastIndexByte(user, '.'); us >= 0 {
			agent, err := strconv.ParseUint(user[us+1:], 10, 8)
			if err != nil {
				return JID{}, fmt.Errorf("%w: bad agent in %q: %w", ErrInvalidJID, s, err)
			}
			j.RawAgent = uint8(agent)
			user = user[:us]
		}
	}
	j.User = user
	return j, nil
}

// newADJID builds a JID from the AD wire form, mapping the agent byte back to a server.
func newADJID(user string, agent, device uint8) JID {
	j := JID{User: user, Device: uint16(device)}
	switch agent {
	case agentLID:
		j.Server = ServerLID
	case agentHosted:
		j.Server = ServerHosted
	case agentHostedLID:
		j.Server = ServerHostedLID
	default:
		j.Server = ServerUser
		j.RawAgent = agent
	}
	return j
}
