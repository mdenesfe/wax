package api

import (
	"time"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
)

// Request and response bodies that are not domain types. They are named
// (rather than anonymous) so the OpenAPI contract test can compare them
// with the schemas in openapi.json.

// errorJSON is every error response.
type errorJSON struct {
	Error string `json:"error"`
}

type healthJSON struct {
	Status string `json:"status"`
}

type qrJSON struct {
	Code      string `json:"code"`
	PNGBase64 string `json:"png_base64"`
}

type sendResponse struct {
	OutboxID  int64  `json:"outbox_id"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate"`
}

type outboxJSON struct {
	OutboxID        int64      `json:"outbox_id"`
	ClientID        string     `json:"client_id"`
	MessageID       string     `json:"message_id"`
	Chat            string     `json:"chat"`
	Status          string     `json:"status"`
	Attempts        int        `json:"attempts"`
	LastError       string     `json:"last_error"`
	ServerTimestamp *time.Time `json:"server_timestamp"`
	CreatedAt       time.Time  `json:"created_at"`
}

type phoneCheckRequest struct {
	Phones []string `json:"phones"`
}

type phoneCheckJSON struct {
	Phone      string `json:"phone"`
	OnWhatsApp bool   `json:"on_whatsapp"`
	JID        string `json:"jid,omitempty"`
	LID        string `json:"lid,omitempty"`
}

type markReadRequest struct {
	Chat   string   `json:"chat"`
	Sender string   `json:"sender"`
	IDs    []string `json:"ids"`
}

type presenceRequest struct {
	Available bool `json:"available"`
}

type rejectCallRequest struct {
	From string `json:"from"`
}

type tenantRequest struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	MaxInstances      int    `json:"max_instances"`
	RequestsPerSecond int    `json:"requests_per_second"`
}

type tenantPatch struct {
	Name              *string `json:"name"`
	MaxInstances      *int    `json:"max_instances"`
	RequestsPerSecond *int    `json:"requests_per_second"`
}

type apiKeyCreated struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// chatJSON is a chat with its newest message, for chat lists.
type chatJSON struct {
	store.Chat
	LastMessage *manager.MessageJSON `json:"last_message,omitempty"`
}

// groupJSON is group metadata with resolved participants.
type groupJSON struct {
	client.GroupInfo
	Participants []participantJSON `json:"participants"`
}

// participantJSON is a group member with what is known about it.
type participantJSON struct {
	client.GroupParticipant
	Phone    string `json:"phone,omitempty"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"push_name,omitempty"`
	Username string `json:"username,omitempty"`
}

// groupSummary is a group in the list of joined groups.
type groupSummary struct {
	JID             string `json:"jid"`
	Name            string `json:"name"`
	Topic           string `json:"topic,omitempty"`
	IsCommunity     bool   `json:"is_community"`
	Community       string `json:"community,omitempty"`
	DefaultSubGroup bool   `json:"default_sub_group"`
	Announce        bool   `json:"announce"`
	Participants    int    `json:"participants"`
}

// profileJSON is everything known about a chat's contact.
type profileJSON struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone,omitempty"`
	LID      string `json:"lid,omitempty"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"push_name,omitempty"`
	Username string `json:"username,omitempty"`
	// Live is set when WhatsApp answered (about, devices, business); it
	// asks to slow down after a few lookups in a row.
	Live        bool             `json:"live"`
	About       string           `json:"about,omitempty"`
	AboutSetAt  *time.Time       `json:"about_set_at,omitempty"`
	AboutHidden bool             `json:"about_hidden"`
	Devices     int              `json:"devices,omitempty"`
	IsBusiness  bool             `json:"is_business"`
	Business    *client.Business `json:"business,omitempty"`
	// CommonGroups are the joined groups the contact is in.
	CommonGroups []groupRef `json:"common_groups"`
	// Messages counts the chat's stored messages by type.
	Messages map[string]int `json:"messages"`
}

// groupRef names a group.
type groupRef struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}
