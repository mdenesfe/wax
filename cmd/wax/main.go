// Command wax runs WhatsApp linked-device instances.
//
//	wax [serve]   multi-instance REST API server (default)
//	wax run ...   single instance in the terminal, for debugging (QR in the
//	              terminal, /send commands on stdin)
//
// Environment (both modes):
//
//	WAX_DATABASE_URL  PostgreSQL DSN (default: the docker compose database)
//	WAX_MASTER_KEY    base64 32-byte key encrypting secrets at rest (required)
//
// Serve mode:
//
//	WAX_API_KEY       bearer token for the API (required, >= 32 characters)
//	WAX_LISTEN        listen address (default 127.0.0.1:8080)
//	WAX_LOG_LEVEL     debug, info, warn, error (default info)
package main

import (
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		runCommand(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	serveCommand(args)
}
