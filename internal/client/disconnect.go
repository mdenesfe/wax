package client

import (
	"fmt"

	"github.com/mdenesfe/wax/internal/binary"
)

// DisconnectAction tells the connection manager what to do after the server
// ends the stream.
type DisconnectAction int

const (
	// ActionNone means the node is not a disconnect signal.
	ActionNone DisconnectAction = iota
	// ActionReconnect: transient; reconnect with backoff, keep the session.
	ActionReconnect
	// ActionReconnectNow: the server asks for an immediate restart (515,
	// typically right after pairing). Reconnect without backoff.
	ActionReconnectNow
	// ActionLoggedOut: the session is gone (device removed, logged out from
	// the phone, ...). Delete session data; a new QR pairing is required.
	ActionLoggedOut
	// ActionReplaced: another connection with the same credentials took over.
	// Do not auto-reconnect, or two instances would kick each other forever.
	ActionReplaced
	// ActionUpgradeRequired: the announced client version is too old (405).
	ActionUpgradeRequired
	// ActionTempBanned: temporarily banned; reconnecting earlier is pointless.
	ActionTempBanned
	// ActionFatal: unknown permanent failure; needs operator attention.
	ActionFatal
)

var actionNames = map[DisconnectAction]string{
	ActionNone: "none", ActionReconnect: "reconnect", ActionReconnectNow: "reconnect-now",
	ActionLoggedOut: "logged-out", ActionReplaced: "replaced", ActionUpgradeRequired: "upgrade-required",
	ActionTempBanned: "temp-banned", ActionFatal: "fatal",
}

func (a DisconnectAction) String() string {
	if s, ok := actionNames[a]; ok {
		return s
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// DisconnectInfo is the classification of a stream:error or failure node.
type DisconnectInfo struct {
	Action DisconnectAction
	Code   string // stream:error code or failure reason
	Detail string // conflict type, message, ... for logs
}

// ClassifyDisconnect inspects a top-level node. For anything other than
// <stream:error> and <failure> it returns ActionNone.
//
// Codes are from observed server behaviour (and whatsmeow's handling, used
// as a cross-reference). Unknown codes on <stream:error> default to reconnect,
// because the stream errors seen in practice are transient. Unknown codes on
// <failure>, which is sent instead of <success> at login, default to fatal so
// we don't hammer the server with a login it keeps refusing.
func ClassifyDisconnect(n *binary.Node) DisconnectInfo {
	switch n.Tag {
	case "stream:error":
		code, _ := n.AttrString("code")
		conflictType := ""
		if c, ok := n.Child("conflict"); ok {
			conflictType, _ = c.AttrString("type")
		}
		info := DisconnectInfo{Code: code, Detail: conflictType}
		switch {
		case code == "515":
			info.Action = ActionReconnectNow
		case code == "401" && conflictType == "device_removed":
			info.Action = ActionLoggedOut
		case conflictType == "replaced":
			info.Action = ActionReplaced
		default: // 503, 500, unknown
			info.Action = ActionReconnect
		}
		return info
	case "failure":
		reason, _ := n.AttrString("reason")
		msg, _ := n.AttrString("message")
		info := DisconnectInfo{Code: reason, Detail: msg}
		switch reason {
		case "401", "403", "406":
			// 401 logged out, 403 main device gone/locked, 406 banned/unknown logout.
			info.Action = ActionLoggedOut
		case "402":
			info.Action = ActionTempBanned
		case "405":
			info.Action = ActionUpgradeRequired
		case "500", "503":
			info.Action = ActionReconnect
		default:
			info.Action = ActionFatal
		}
		return info
	}
	return DisconnectInfo{Action: ActionNone}
}
