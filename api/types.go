package api

import "time"

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
