package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
)

type spec struct {
	OpenAPI string                                  `json:"openapi"`
	Paths   map[string]map[string]specOperation     `json:"paths"`
	Hooks   map[string]any                          `json:"webhooks"`
	Comp    struct{ Schemas map[string]specSchema } `json:"components"`
}

type specOperation struct {
	Security []map[string][]string `json:"security"`
}

type specSchema struct {
	Properties map[string]any `json:"properties"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := json.Unmarshal(openAPI, &s); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	if s.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version %q", s.OpenAPI)
	}
	return s
}

// TestOpenAPIContract fails when the code and openapi.json disagree: a
// route added, removed or with another key type, a JSON field renamed, or
// a webhook event not documented.
func TestOpenAPIContract(t *testing.T) {
	s := loadSpec(t)

	// Routes and their key type.
	documented := map[string]string{} // "GET /v1/x" -> security scheme
	for path, ops := range s.Paths {
		for method, op := range ops {
			scheme := ""
			for _, req := range op.Security {
				for name := range req {
					scheme = name
				}
			}
			documented[strings.ToUpper(method)+" "+path] = scheme
		}
	}
	srv := &Server{}
	inCode := map[string]string{"GET /healthz": "", "GET /openapi.json": ""}
	for _, rt := range srv.routeTable() {
		scheme := "tenantKey"
		if rt.instance == nil && rt.role == roleAdmin {
			scheme = "adminKey"
		}
		inCode[rt.pattern] = scheme
	}
	for route, scheme := range inCode {
		got, ok := documented[route]
		switch {
		case !ok:
			t.Errorf("route %s is not documented", route)
		case got != scheme:
			t.Errorf("route %s: documented security %q, code requires %q", route, got, scheme)
		}
	}
	for route := range documented {
		if _, ok := inCode[route]; !ok {
			t.Errorf("documented route %s does not exist", route)
		}
	}

	// Schemas against the Go types that produce or accept them.
	types := map[string]any{
		"Error": errorJSON{}, "Health": healthJSON{}, "QR": qrJSON{},
		"Tenant": store.Tenant{}, "TenantCreate": tenantRequest{}, "TenantPatch": tenantPatch{},
		"APIKey": store.APIKey{}, "APIKeyCreated": apiKeyCreated{},
		"Instance": instanceJSON{}, "InstanceCreate": instanceRequest{}, "InstancePatch": instancePatch{},
		"SendRequest": sendRequest{}, "MediaUpload": mediaRequest{}, "SendResponse": sendResponse{}, "OutboxItem": outboxJSON{},
		"Chat": chatJSON{}, "Message": manager.MessageJSON{}, "Media": manager.MediaJSON{}, "Reaction": manager.ReactionJSON{},
		"MarkRead": markReadRequest{}, "Presence": presenceRequest{}, "RejectCall": rejectCallRequest{},
		"GroupInfo": groupJSON{}, "GroupParticipant": participantJSON{},
		"WebhookEnvelope": manager.Envelope{}, "ForwardRequest": forwardRequest{},
		"Person": manager.PersonJSON{}, "Quoted": manager.QuotedJSON{}, "Stub": manager.StubJSON{}, "GroupSummary": groupSummary{},
		"Profile": profileJSON{}, "GroupRef": groupRef{}, "Business": client.Business{}, "BusinessHours": client.BusinessHours{},
	}
	for name, v := range types {
		sch, ok := s.Comp.Schemas[name]
		if !ok {
			t.Errorf("schema %s missing", name)
			continue
		}
		want := jsonFields(reflect.TypeOf(v))
		got := slices.Sorted(maps.Keys(sch.Properties))
		if !slices.Equal(got, want) {
			t.Errorf("schema %s has %v; the Go type has %v", name, got, want)
		}
	}
	for name := range s.Comp.Schemas {
		if _, ok := types[name]; !ok {
			t.Errorf("schema %s is not checked against a Go type", name)
		}
	}

	// Instance states.
	var states []string
	if st, ok := s.Comp.Schemas["Instance"].Properties["status"].(map[string]any); ok {
		for _, v := range st["enum"].([]any) {
			states = append(states, v.(string))
		}
	}
	var code []string
	for _, st := range manager.Statuses {
		code = append(code, string(st))
	}
	if slices.Sort(states); !slices.Equal(states, slices.Sorted(slices.Values(code))) {
		t.Errorf("documented instance states %v, code has %v", states, code)
	}

	// Webhook events.
	if got, want := slices.Sorted(maps.Keys(s.Hooks)), slices.Sorted(slices.Values(manager.WebhookEvents)); !slices.Equal(got, want) {
		t.Errorf("documented webhooks %v, code emits %v", got, want)
	}

	// Every $ref resolves.
	for _, m := range regexp.MustCompile(`"\$ref":\s*"#/components/schemas/([A-Za-z]+)"`).FindAllSubmatch(openAPI, -1) {
		if _, ok := s.Comp.Schemas[string(m[1])]; !ok {
			t.Errorf("dangling $ref to %s", m[1])
		}
	}
}

// jsonFields returns the JSON property names of a struct type, sorted.
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out) // a field shadowing an embedded one appears once
}

// TestOpenAPIServed: the document is served without authentication.
func TestOpenAPIServed(t *testing.T) {
	e := setup(t)
	code, body := e.call("GET", "/openapi.json", nil, "")
	if code != 200 || !json.Valid(body) {
		t.Fatalf("GET /openapi.json: %d", code)
	}
}
