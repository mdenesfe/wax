package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestProvisionTenantsSyntax: malformed entries fail before the database
// is touched, without quoting the entry (it may hold a key).
func TestProvisionTenantsSyntax(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := provisionTenants(context.Background(), log, nil, " \n "); err != nil {
		t.Fatalf("empty: %v", err)
	}
	for _, spec := range []string{"wax_0123456789abcdef_secretsecret", "Bad:wax_x_y", ":wax_x_y"} {
		err := provisionTenants(context.Background(), log, nil, spec)
		if err == nil {
			t.Fatalf("%q accepted", spec)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "wax_") {
			t.Fatalf("error quotes the entry: %v", err)
		}
	}
}
