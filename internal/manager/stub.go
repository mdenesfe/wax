package manager

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/mdenesfe/wax/internal/wabin"
)

// stubNames names the common system message types (WebMessageInfo
// StubType). Others are reported by number only.
var stubNames = map[int]string{
	1: "revoke", 2: "ciphertext", 3: "futureproof",
	20: "group_create", 21: "group_change_subject", 22: "group_change_icon", 23: "group_change_invite_link",
	24: "group_change_description", 25: "group_change_restrict", 26: "group_change_announce",
	27: "group_participant_add", 28: "group_participant_remove", 29: "group_participant_promote",
	30: "group_participant_demote", 31: "group_participant_invite", 32: "group_participant_leave",
	33: "group_participant_change_number", 37: "generic_notification", 39: "e2e_encrypted",
	40: "call_missed_voice", 41: "call_missed_video", 42: "individual_change_number", 43: "group_delete",
	45: "call_missed_group_voice", 46: "call_missed_group_video", 69: "group_change_no_frequently_forwarded",
	71: "group_participant_add_request_join", 75: "e2e_encrypted_now", 122: "block_contact",
	126: "biz_privacy_mode_init_fb", 128: "biz_privacy_mode_to_fb", 130: "disappearing_mode", 132: "admin_revoke",
	134: "community_link_parent_group", 135: "community_link_sibling_group", 136: "community_link_sub_group",
	137: "community_unlink_parent_group", 138: "community_unlink_sibling_group", 139: "community_unlink_sub_group",
	140: "group_participant_accept", 141: "group_participant_linked_group_join", 142: "community_create",
	144: "group_membership_join_approval_request", 145: "group_membership_join_approval_mode",
	147: "community_participant_promote", 148: "community_participant_demote",
	166: "sub_group_participant_add_rich", 193: "change_username", 216: "change_limit_sharing",
}

// DescribeStub describes a stored system message: typ is "stub:<n>" and
// params the stored parameters (a JSON array, may be nil). ok is false
// for other types.
func DescribeStub(typ string, params []byte) (stub *StubJSON, ok bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(typ, "stub:"))
	if err != nil || !strings.HasPrefix(typ, "stub:") {
		return nil, false
	}
	stub = &StubJSON{Type: n, Name: stubNames[n]}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &stub.Parameters) // unreadable parameters are left out
	}
	for _, p := range stub.Parameters {
		if j, err := wabin.ParseJID(p); err == nil && (j.Server == wabin.ServerUser || j.Server == wabin.ServerLID) {
			stub.People = append(stub.People, PersonJSON{JID: j.ToNonAD().String()})
		}
	}
	return stub, true
}
