// Command wax runs WhatsApp linked-device instances.
//
//	wax [serve]   multi-instance REST API server (default)
//	wax run ...   single instance in the terminal, for debugging (QR in the
//	              terminal, /send commands on stdin)
//	wax version   print the program version
//
// The environment carries secrets only (both modes):
//
//	WAX_DATABASE_URL  PostgreSQL DSN (default: the docker compose database)
//	WAX_MASTER_KEY    base64 32-byte key encrypting secrets at rest (required)
//
// Serve mode:
//
//	WAX_API_KEY       bearer token for the API (required, >= 32 characters)
//
// All other settings are flags; see `wax serve -h` and `wax run -h`.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
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
	if len(args) > 0 && args[0] == "run" {
		runCommand(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	serveCommand(args)
}
