package client

import (
	"testing"

	"github.com/mdenesfe/wax/internal/wabin"
)

// A <description> without <body> (seen live) must not crash parsing.
func TestParseGroupDescriptionWithoutBody(t *testing.T) {
	gi := parseGroup(&wabin.Node{Tag: "group", Attrs: wabin.Attrs{"id": "120363000000000000", "subject": "g"},
		Content: []wabin.Node{{Tag: "description", Attrs: wabin.Attrs{"id": "1"}}}})
	if gi.Name != "g" || gi.Topic != "" {
		t.Fatalf("parsed %+v", gi)
	}
}
