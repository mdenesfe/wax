package wax

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

var testKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

func TestOpenValidates(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, Config{MasterKey: testKey}); err == nil {
		t.Fatal("opened without an account name")
	}
	if _, err := Open(ctx, Config{Account: "a", MasterKey: "short"}); err == nil {
		t.Fatal("opened with an invalid master key")
	}
	if _, err := Open(ctx, Config{Account: "a", MasterKey: testKey, Version: "not a version"}); err == nil {
		t.Fatal("opened with an invalid version")
	}
}

// TestNotRunning: before Run, calls fail with ErrNotRunning.
func TestNotRunning(t *testing.T) {
	c := &Client{}
	jid, _ := ParseJID("905550000001@s.whatsapp.net")
	if _, err := c.SendText(context.Background(), jid, "hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("SendText: %v", err)
	}
	if c.IsLoggedIn() {
		t.Fatal("logged in before Run")
	}
	if _, _, ok := c.OwnJID(); ok {
		t.Fatal("own JID before Run")
	}
}

// TestOpen: Open prepares the database (schema included) without
// connecting to WhatsApp.
func TestOpen(t *testing.T) {
	dsn := os.Getenv("WAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAX_TEST_DATABASE_URL not set")
	}
	c, err := Open(context.Background(), Config{DatabaseURL: dsn, MasterKey: testKey, Account: "wax-open-test", Version: "2.3000.1012345678"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.IsLoggedIn() {
		t.Fatal("logged in without Run")
	}
}

func TestQR(t *testing.T) {
	if s, err := QRTerminal("2@demo,1,2,3"); err != nil || s == "" {
		t.Fatalf("terminal: %v", err)
	}
	if png, err := QRPNG("2@demo,1,2,3"); err != nil || !strings.HasPrefix(string(png), "\x89PNG") {
		t.Fatalf("png: %v", err)
	}
}
