// Command wax runs one WhatsApp linked-device instance: it shows a QR code on
// first start, persists the session in PostgreSQL and reconnects with the
// stored identity afterwards.
//
// Environment:
//
//	WAX_DATABASE_URL  PostgreSQL DSN (default: the docker compose database)
//	WAX_MASTER_KEY    base64 32-byte key encrypting secrets at rest (required)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/waversion"
)

const defaultDSN = "postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable"

func main() {
	var (
		instance     = flag.String("instance", "default", "instance id (one linked device per instance)")
		debug        = flag.Bool("debug", false, "debug logging")
		dumpNodes    = flag.Bool("dump-nodes", false, "with -debug: log every received node (masked)")
		unmask       = flag.Bool("unmask", false, "with -dump-nodes: do not mask payloads/JIDs")
		versionFlag  = flag.String("version", "", "WhatsApp Web version to announce (default: detect)")
		noVersionNet = flag.Bool("no-version-check", false, "do not query web.whatsapp.com for the current version")
		qrPNG        = flag.String("qr-png", "", "also write the current QR code to this PNG file")
		once         = flag.Bool("once", false, "exit instead of starting a new pairing after logout/QR timeout")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log, options{
		instance: *instance, dumpNodes: *dumpNodes, unmask: *unmask, version: *versionFlag,
		checkVersion: !*noVersionNet, qrPNG: *qrPNG, once: *once,
	}); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

type options struct {
	instance, version, qrPNG        string
	dumpNodes, unmask, checkVersion bool
	once                            bool
}

func run(ctx context.Context, log *slog.Logger, o options) error {
	masterKey, err := store.ParseMasterKey(os.Getenv("WAX_MASTER_KEY"))
	if err != nil {
		return fmt.Errorf("WAX_MASTER_KEY: %w", err)
	}
	dsn := os.Getenv("WAX_DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}
	st, err := store.Open(ctx, dsn, masterKey)
	if err != nil {
		return err
	}
	defer st.Close()

	version, err := resolveVersion(ctx, log, o.version, o.checkVersion)
	if err != nil {
		return err
	}
	log.Info("starting", "instance", o.instance, "version", version.String())

	if o.checkVersion {
		var inUse atomic.Value
		inUse.Store(version)
		go waversion.Watch(ctx, log, &http.Client{Timeout: 30 * time.Second}, 6*time.Hour,
			func() waversion.Version { return inUse.Load().(waversion.Version) },
			func(latest waversion.Version) {
				// Not hot-swapped: a running session keeps its version until
				// reconnect, and 405 failures surface as ClientOutdated.
				inUse.Store(latest)
			})
	}

	for {
		cl, err := client.New(client.Config{
			Store: st, InstanceID: o.instance, Device: client.DefaultDeviceConfig(version), Log: log,
			OnEvent: eventPrinter(log, o.qrPNG), DumpNodes: o.dumpNodes, DumpUnmasked: o.unmask,
		})
		if err != nil {
			return err
		}
		err = cl.Run(ctx)
		switch {
		case !o.once && (errors.Is(err, client.ErrLoggedOut) || errors.Is(err, client.ErrQRTimeout)):
			log.Warn("starting a new pairing", "reason", err)
			continue
		default:
			return err
		}
	}
}

func eventPrinter(log *slog.Logger, pngPath string) func(client.Event) {
	return func(e client.Event) {
		switch e := e.(type) {
		case client.QRCode:
			art, err := pairing.RenderTerminal(e.Content)
			if err != nil {
				log.Error("cannot render QR", "error", err)
				return
			}
			fmt.Fprintf(os.Stderr, "\n%s\nScan with WhatsApp → Linked devices → Link a device  (code %d/%d, valid %s)\n\n",
				art, e.Index, e.Total, e.Timeout)
			if pngPath != "" {
				if png, err := pairing.RenderPNG(e.Content); err == nil {
					if err := os.WriteFile(pngPath, png, 0o600); err != nil {
						log.Warn("cannot write QR png", "error", err)
					}
				}
			}
		case client.QRTimeout:
			log.Warn("QR codes expired without being scanned")
		case client.PairSuccess:
			log.Info("paired", "platform", e.Platform, "business", e.BusinessName)
		case client.PairError:
			log.Error("pairing failed", "error", e.Err)
		case client.Connected:
			log.Info("online")
		case client.LoggedOut:
			log.Warn("logged out; session deleted", "code", e.Code, "detail", e.Detail)
		case client.StreamReplaced:
			log.Warn("another connection replaced this session")
		case client.ClientOutdated:
			log.Error("WhatsApp rejected client version as outdated; set -version or update the default", "version", e.Version)
		}
	}
}

func resolveVersion(ctx context.Context, log *slog.Logger, flagValue string, check bool) (waversion.Version, error) {
	if flagValue != "" {
		return waversion.Parse(flagValue)
	}
	if check {
		v, err := waversion.FetchLatest(ctx, nil)
		if err == nil {
			return v, nil
		}
		log.Warn("version check failed, using built-in default", "error", err)
	}
	return waversion.Default, nil
}
