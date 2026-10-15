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
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
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
		sendTo       = flag.String("send-to", "", "after connecting, send -text once to this phone number (digits) or JID (e.g. 123@lid)")
		sendText     = flag.String("text", "", "text for -send-to")
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
		checkVersion: !*noVersionNet, qrPNG: *qrPNG, once: *once, sendTo: *sendTo, sendText: *sendText,
	}); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

type options struct {
	instance, version, qrPNG        string
	sendTo, sendText                string
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

	var current atomic.Pointer[client.Client]
	go commandLoop(ctx, log, &current)

	for {
		printer := eventPrinter(log, o.qrPNG, o.unmask)
		onEvent := printer
		if o.sendTo != "" {
			to, err := parseRecipient(o.sendTo)
			if err != nil {
				return err
			}
			var sent atomic.Bool
			onEvent = func(e client.Event) {
				printer(e)
				if _, ok := e.(client.Connected); ok && sent.CompareAndSwap(false, true) {
					// Send from a separate goroutine: event handlers must not block.
					go sendOnce(ctx, log, &current, to, o.sendText)
				}
			}
		}
		cl, err := client.New(client.Config{
			Store: st, InstanceID: o.instance, Device: client.DefaultDeviceConfig(version), Log: log,
			OnEvent: onEvent, DumpNodes: o.dumpNodes, DumpUnmasked: o.unmask,
		})
		if err != nil {
			return err
		}
		current.Store(cl)
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

// commandLoop reads simple commands from stdin, for manual testing:
//
//	/send <phone digits> <text>
func commandLoop(ctx context.Context, log *slog.Logger, current *atomic.Pointer[client.Client]) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "/send ") {
			if line != "" {
				fmt.Fprintln(os.Stderr, "usage: /send <phone digits> <text>")
			}
			continue
		}
		num, text, ok := strings.Cut(strings.TrimPrefix(line, "/send "), " ")
		num = strings.TrimLeft(strings.TrimSpace(num), "+")
		if !ok || num == "" || strings.Trim(num, "0123456789") != "" || strings.TrimSpace(text) == "" {
			fmt.Fprintln(os.Stderr, "usage: /send <phone digits> <text>")
			continue
		}
		sendOnce(ctx, log, current, binary.JID{User: num, Server: binary.ServerUser}, text)
	}
}

// parseRecipient accepts phone digits (optionally with +) or a full JID.
func parseRecipient(s string) (binary.JID, error) {
	s = strings.TrimLeft(strings.TrimSpace(s), "+")
	if strings.Contains(s, "@") {
		j, err := binary.ParseJID(s)
		if err != nil || j.User == "" {
			return binary.JID{}, fmt.Errorf("invalid recipient %q", s)
		}
		return j, nil
	}
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return binary.JID{}, fmt.Errorf("invalid phone number %q", s)
	}
	return binary.JID{User: s, Server: binary.ServerUser}, nil
}

func sendOnce(ctx context.Context, log *slog.Logger, current *atomic.Pointer[client.Client], to binary.JID, text string) {
	cl := current.Load()
	if cl == nil || text == "" {
		log.Error("send-to: nothing to send")
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := cl.SendText(sendCtx, to, text)
	if err != nil {
		log.Error("send failed", "error", err)
		return
	}
	log.Info("sent", "id", res.ID, "chat", binary.MaskJID(res.Chat), "server_time", res.Timestamp)
}

func messageText(m *wae2e.Message) string {
	switch {
	case m.GetConversation() != "":
		return m.GetConversation()
	case m.GetExtendedTextMessage().GetText() != "":
		return m.GetExtendedTextMessage().GetText()
	case m.GetProtocolMessage() != nil:
		return "[protocol message: " + m.GetProtocolMessage().GetType().String() + "]"
	}
	return "[non-text message]"
}

func eventPrinter(log *slog.Logger, pngPath string, unmask bool) func(client.Event) {
	jid := func(j binary.JID) string {
		if unmask {
			return j.String()
		}
		return binary.MaskJID(j)
	}
	return func(e client.Event) {
		switch e := e.(type) {
		case client.Message:
			dir := "from"
			if e.Info.IsFromMe {
				dir = "sent by me (other device) in"
			}
			fmt.Fprintf(os.Stderr, "\n>>> message %s %s (%s) id=%s: %s\n\n", dir, jid(e.Info.Chat), e.Info.PushName, e.Info.ID, messageText(e.Message))
		case client.UndecryptableMessage:
			log.Warn("undecryptable message (retry requested)", "id", e.Info.ID, "error", e.Err)
		case client.Receipt:
			log.Info("receipt", "type", e.Type, "ids", e.IDs, "from", jid(e.Sender))
		case client.IdentityChanged:
			log.Info("contact identity changed", "jid", jid(e.JID))
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
