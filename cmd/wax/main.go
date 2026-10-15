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
	"path/filepath"
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
		sendText     = flag.String("text", "", "text for -send-to (caption with -send-file)")
		sendFilePath = flag.String("send-file", "", "with -send-to: send this file instead of a text")
		mediaDir     = flag.String("media-dir", "", "save received media files into this directory")
		rejectCalls  = flag.Bool("reject-calls", false, "automatically reject incoming calls")
		pushName     = flag.String("push-name", "", "our display name (needed for presence until app state sync)")
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
		checkVersion: !*noVersionNet, qrPNG: *qrPNG, once: *once, sendTo: *sendTo, sendText: *sendText, sendFile: *sendFilePath,
		mediaDir: *mediaDir, rejectCalls: *rejectCalls, pushName: *pushName,
	}); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

type options struct {
	instance, version, qrPNG        string
	sendTo, sendText, sendFile      string
	mediaDir, pushName              string
	rejectCalls                     bool
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
		printer := eventPrinter(log, o.qrPNG, o.unmask, o.mediaDir, o.rejectCalls, current.Load)
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
					if o.sendFile != "" {
						go sendFile(ctx, log, current.Load(), to, o.sendFile, o.sendText)
					} else {
						go sendOnce(ctx, log, &current, to, o.sendText)
					}
				}
			}
		}
		cl, err := client.New(client.Config{
			Store: st, InstanceID: o.instance, Device: client.DefaultDeviceConfig(version), Log: log,
			OnEvent: onEvent, DumpNodes: o.dumpNodes, DumpUnmasked: o.unmask, PushName: o.pushName,
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
const commandHelp = `commands:
  /send  <phone|jid> <text>             send a text (jid may be a group: 1203...@g.us)
  /media <phone|jid> <file> [caption]   send a file (image/video/audio/document by extension)
  /group <group jid>                    show group info`

func commandLoop(ctx context.Context, log *slog.Logger, current *atomic.Pointer[client.Client]) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		cl := current.Load()
		if cl == nil {
			fmt.Fprintln(os.Stderr, "not ready yet")
			continue
		}
		switch cmd {
		case "/send":
			to, text, ok := strings.Cut(strings.TrimSpace(rest), " ")
			rcpt, err := parseRecipient(to)
			if !ok || err != nil || strings.TrimSpace(text) == "" {
				fmt.Fprintln(os.Stderr, commandHelp)
				continue
			}
			sendOnce(ctx, log, current, rcpt, text)
		case "/media":
			parts := strings.SplitN(strings.TrimSpace(rest), " ", 3)
			if len(parts) < 2 {
				fmt.Fprintln(os.Stderr, commandHelp)
				continue
			}
			rcpt, err := parseRecipient(parts[0])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			caption := ""
			if len(parts) == 3 {
				caption = parts[2]
			}
			sendFile(ctx, log, cl, rcpt, parts[1], caption)
		case "/group":
			g, err := parseRecipient(strings.TrimSpace(rest))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			gi, err := cl.GetGroupInfo(gctx, g)
			cancel()
			if err != nil {
				log.Error("group info failed", "error", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "group %q addressing=%s participants=%d\n", gi.Name, gi.AddressingMode, len(gi.Participants))
		default:
			fmt.Fprintln(os.Stderr, commandHelp)
		}
	}
}

func sendFile(ctx context.Context, log *slog.Logger, cl *client.Client, to binary.JID, path, caption string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Error("read file", "error", err)
		return
	}
	mime := http.DetectContentType(data)
	if byExt := mimeByExt(path); byExt != "" {
		mime = byExt
	}
	kind := client.KindDocument
	switch {
	case strings.HasPrefix(mime, "image/"):
		kind = client.KindImage
	case strings.HasPrefix(mime, "video/"):
		kind = client.KindVideo
	case strings.HasPrefix(mime, "audio/"):
		kind = client.KindAudio
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := cl.SendMedia(sctx, to, client.SendMediaRequest{Kind: kind, Data: data, MimeType: mime, Caption: caption, FileName: filepath.Base(path)})
	if err != nil {
		log.Error("send media failed", "error", err)
		return
	}
	log.Info("sent media", "id", res.ID, "mime", mime, "bytes", len(data))
}

func mimeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".mp4":
		return "video/mp4"
	case ".ogg", ".opus":
		return "audio/ogg; codecs=opus"
	case ".mp3":
		return "audio/mpeg"
	case ".pdf":
		return "application/pdf"
	}
	return ""
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
	case m.GetImageMessage() != nil:
		return "[image] " + m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return "[video] " + m.GetVideoMessage().GetCaption()
	case m.GetAudioMessage() != nil:
		return "[audio]"
	case m.GetDocumentMessage() != nil:
		return "[document " + m.GetDocumentMessage().GetFileName() + "]"
	case m.GetStickerMessage() != nil:
		return "[sticker]"
	case m.GetReactionMessage() != nil:
		return "[reaction " + m.GetReactionMessage().GetText() + "]"
	}
	return "[non-text message]"
}

// saveMedia downloads a received media message into dir.
func saveMedia(log *slog.Logger, cl *client.Client, dir string, e client.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	data, err := cl.Download(ctx, e.Message)
	if errors.Is(err, client.ErrNoMedia) {
		return
	}
	if err != nil {
		log.Error("media download failed", "id", e.Info.ID, "error", err)
		return
	}
	path := filepath.Join(dir, e.Info.ID)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Error("save media", "error", err)
		return
	}
	log.Info("media saved", "path", path, "bytes", len(data))
}

func eventPrinter(log *slog.Logger, pngPath string, unmask bool, mediaDir string, rejectCalls bool, getClient func() *client.Client) func(client.Event) {
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
			where := jid(e.Info.Chat)
			if e.Info.IsGroup {
				where += " by " + jid(e.Info.Sender)
			}
			fmt.Fprintf(os.Stderr, "\n>>> message %s %s (%s) id=%s: %s\n\n", dir, where, e.Info.PushName, e.Info.ID, messageText(e.Message))
			if mediaDir != "" && getClient != nil {
				if cl := getClient(); cl != nil {
					go saveMedia(log, cl, mediaDir, e)
				}
			}
		case client.CallEvent:
			log.Info("call", "type", e.Type, "from", jid(e.From), "call_id", e.CallID, "video", e.Video)
			if e.Type == "offer" && rejectCalls && getClient != nil {
				if cl := getClient(); cl != nil {
					go func() {
						if err := cl.RejectCall(context.Background(), e.Creator, e.CallID); err != nil {
							log.Error("reject call failed", "error", err)
						} else {
							log.Info("call rejected", "call_id", e.CallID)
						}
					}()
				}
			}
		case client.AppStateSyncComplete:
			log.Info("app state synced", "collection", e.Collection, "version", e.Version)
		case client.AppStateSyncError:
			log.Error("app state sync error", "collection", e.Collection, "error", e.Err)
		case client.AppStateChange:
			if !e.FromFullSync {
				log.Info("app state change", "collection", e.Collection, "kind", e.Index[0], "removed", e.Removed)
			}
		case client.HistorySync:
			log.Info("history sync", "type", e.Type, "chunk", e.Chunk, "progress", e.Progress,
				"conversations", e.Conversations, "messages", e.Messages, "push_names", e.PushNames)
		case client.AccountSync:
			log.Info("account sync", "changes", e.Changes)
		case client.GroupUpdate:
			log.Info("group update", "group", jid(e.JID), "changes", e.Changes)
		case client.Presence:
			log.Info("presence", "from", jid(e.From), "available", e.Available, "last_seen", e.LastSeen)
		case client.ChatPresence:
			log.Info("chat presence", "chat", jid(e.Chat), "state", e.State)
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
