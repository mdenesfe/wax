// Command wax runs WhatsApp linked-device instances.
//
//	wax [serve]   multi-instance REST API server (default)
//	wax run ...   single instance in the terminal, for debugging (QR in the
//	              terminal, /send commands on stdin)
//	wax version   print the program version
//	wax keygen    print a new tenant API key (for WAX_TENANTS)
//
// The environment carries secrets only (both modes):
//
//	WAX_DATABASE_URL  PostgreSQL DSN (default: the docker compose database)
//	WAX_MASTER_KEY    base64 32-byte key encrypting secrets at rest (required)
//
// Serve mode:
//
//	WAX_API_KEY       bearer token for the API (required, >= 32 characters)
//	WAX_TENANTS       tenants and keys to set up at start (optional):
//	                  "acme:wax_<id>_<secret>,other:wax_<id>_<secret>"
//
// All other settings are flags; see `wax serve -h` and `wax run -h`.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/mdenesfe/wax/internal/store"
)

// buildVersion is set at release builds (-ldflags "-X main.buildVersion=v1.2.3").
var buildVersion = ""

// programVersion is the release version, or the module version when
// installed with `go install`, or "dev".
func programVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("wax", programVersion())
		return
	}
	if len(args) > 0 && args[0] == "keygen" {
		key, err := store.NewAPIKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(key)
		return
	}
	if len(args) > 0 && args[0] == "run" {
		runCommand(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	serveCommand(args)
}
