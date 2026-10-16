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

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/internal/waversion"
)

const defaultDSN = "postgres://wax:wax@127.0.0.1:5433/wax?sslmode=disable"

// runCommand is the single-instance debugging mode ("wax run").
func runCommand(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		instance     = fs.String("instance", "default", "instance id (one linked device per instance)")
		debug        = fs.Bool("debug", false, "debug logging")
		dumpNodes    = fs.Bool("dump-nodes", false, "with -debug: log every received node (masked)")
		unmask       = fs.Bool("unmask", false, "with -dump-nodes: do not mask payloads/JIDs")
		versionFlag  = fs.String("version", "", "WhatsApp Web version to announce (default: detect)")
		noVersionNet = fs.Bool("no-version-check", false, "do not query web.whatsapp.com for the current version")
		qrPNG        = fs.String("qr-png", "", "also write the current QR code to this PNG file")
		once         = fs.Bool("once", false, "exit instead of starting a new pairing after logout/QR timeout")
		sendTo       = fs.String("send-to", "", "after connecting, send -text once to this phone number (digits) or JID (e.g. 123@lid)")
		sendText     = fs.String("text", "", "text for -send-to (caption with -send-file)")
		sendFilePath = fs.String("send-file", "", "with -send-to: send this file instead of a text")
		mediaDir     = fs.String("media-dir", "", "save received media files into this directory")
		rejectCalls  = fs.Bool("reject-calls", false, "automatically reject incoming calls")
		pushName     = fs.String("push-name", "", "our display name (needed for presence until app state sync)")
	)
	fs.Parse(args)

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
	st, err := openStore(ctx)
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
		// Only warns: a running session keeps its version until restart,
		// and 405 failures surface as ClientOutdated.
		go waversion.Watch(ctx, log, &http.Client{Timeout: 30 * time.Second}, 6*time.Hour,
			func() waversion.Version { return version }, func(waversion.Version) {})
	}

	var current atomic.Pointer[client.Client]
	go commandLoop(ctx, log, &current)
	p := &printer{log: log, o: o, current: current.Load}
	onEvent := p.handle
	if o.sendTo != "" {
		to, err := wabin.ParseRecipient(o.sendTo)
		if err != nil {
			return err
		}
		var sent atomic.Bool
		onEvent = func(e client.Event) {
			p.handle(e)
			if _, ok := e.(client.Connected); !ok || !sent.CompareAndSwap(false, true) {
				return
			}
			// Send from a separate goroutine: event handlers must not block.
			if o.sendFile != "" {
				go sendFile(ctx, log, current.Load(), to, o.sendFile, o.sendText)
			} else {
				go sendOnce(ctx, log, &current, to, o.sendText)
			}
		}
	}
	for {
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
			rcpt, err := wabin.ParseRecipient(to)
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
			rcpt, err := wabin.ParseRecipient(parts[0])
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
			g, err := wabin.ParseRecipient(strings.TrimSpace(rest))
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

func sendFile(ctx context.Context, log *slog.Logger, cl *client.Client, to wabin.JID, path, caption string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Error("read file", "error", err)
		return
	}
	mime := http.DetectContentType(data)
	if byExt := mimeByExt(path); byExt != "" {
		mime = byExt
	}
	kind := client.KindForMIME(mime)
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

func sendOnce(ctx context.Context, log *slog.Logger, current *atomic.Pointer[client.Client], to wabin.JID, text string) {
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
	log.Info("sent", "id", res.ID, "chat", wabin.MaskJID(res.Chat), "server_time", res.Timestamp)
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

// printer logs client events for the console and runs the optional
// automatic actions (media download, call rejection).
type printer struct {
	log     *slog.Logger
	o       options
	current func() *client.Client
}

func (p *printer) jid(j wabin.JID) string {
	if p.o.unmask {
		return j.String()
	}
	return wabin.MaskJID(j)
}

func (p *printer) handle(e client.Event) {
	switch e := e.(type) {
	case client.Message:
		p.message(e)
	case client.CallEvent:
		p.call(e)
	case client.QRCode:
		p.qr(e)
	case client.AppStateSyncComplete:
		p.log.Info("app state synced", "collection", e.Collection, "version", e.Version)
	case client.AppStateUnverified:
		p.log.Info("app state collection unverified; records verified individually", "collection", e.Collection, "version", e.Version)
	case client.AppStateSyncError:
		p.log.Error("app state sync error", "collection", e.Collection, "error", e.Err)
	case client.AppStateChange:
		if !e.FromFullSync {
			p.log.Info("app state change", "collection", e.Collection, "kind", e.Index[0], "removed", e.Removed)
		}
	case client.HistorySync:
		p.log.Info("history sync", "type", e.Type, "chunk", e.Chunk, "progress", e.Progress,
			"conversations", e.Conversations, "messages", e.Messages, "push_names", e.PushNames)
	case client.AccountSync:
		p.log.Info("account sync", "changes", e.Changes)
	case client.GroupUpdate:
		p.log.Info("group update", "group", p.jid(e.JID), "changes", e.Changes)
	case client.Presence:
		p.log.Info("presence", "from", p.jid(e.From), "available", e.Available, "last_seen", e.LastSeen)
	case client.ChatPresence:
		p.log.Info("chat presence", "chat", p.jid(e.Chat), "state", e.State)
	case client.UndecryptableMessage:
		p.log.Warn("undecryptable message (retry requested)", "id", e.Info.ID, "error", e.Err)
	case client.Receipt:
		p.log.Info("receipt", "type", e.Type, "ids", e.IDs, "from", p.jid(e.Sender))
	case client.IdentityChanged:
		p.log.Info("contact identity changed", "jid", p.jid(e.JID))
	case client.QRTimeout:
		p.log.Warn("QR codes expired without being scanned")
	case client.PairSuccess:
		p.log.Info("paired", "platform", e.Platform, "business", e.BusinessName)
	case client.PairError:
		p.log.Error("pairing failed", "error", e.Err)
	case client.Connected:
		p.log.Info("online")
	case client.LoggedOut:
		p.log.Warn("logged out; session deleted", "code", e.Code, "detail", e.Detail)
	case client.StreamReplaced:
		p.log.Warn("another connection replaced this session")
	case client.ClientOutdated:
		p.log.Error("WhatsApp rejected client version as outdated; set -version or update the default", "version", e.Version)
	}
}

func (p *printer) message(e client.Message) {
	dir := "from"
	if e.Info.IsFromMe {
		dir = "sent by me (other device) in"
	}
	where := p.jid(e.Info.Chat)
	if e.Info.IsGroup {
		where += " by " + p.jid(e.Info.Sender)
	}
	fmt.Fprintf(os.Stderr, "\n>>> message %s %s (%s) id=%s: %s\n\n", dir, where, e.Info.PushName, e.Info.ID, describe(e))
	if cl := p.current(); p.o.mediaDir != "" && cl != nil {
		go saveMedia(p.log, cl, p.o.mediaDir, e)
	}
}

func (p *printer) call(e client.CallEvent) {
	p.log.Info("call", "type", e.Type, "from", p.jid(e.From), "call_id", e.CallID, "video", e.Video)
	cl := p.current()
	if e.Type != "offer" || !p.o.rejectCalls || cl == nil {
		return
	}
	go func() {
		if err := cl.RejectCall(context.Background(), e.Creator, e.CallID); err != nil {
			p.log.Error("reject call failed", "error", err)
			return
		}
		p.log.Info("call rejected", "call_id", e.CallID)
	}()
}

func (p *printer) qr(e client.QRCode) {
	art, err := pairing.RenderTerminal(e.Content)
	if err != nil {
		p.log.Error("cannot render QR", "error", err)
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s\nScan with WhatsApp → Linked devices → Link a device  (code %d/%d, valid %s)\n\n",
		art, e.Index, e.Total, e.Timeout)
	if p.o.qrPNG == "" {
		return
	}
	png, err := pairing.RenderPNG(e.Content)
	if err == nil {
		err = os.WriteFile(p.o.qrPNG, png, 0o600)
	}
	if err != nil {
		p.log.Warn("cannot write QR png", "error", err)
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

// describe renders a message for the console: its text, or its type.
func describe(e client.Message) string {
	d := manager.DescribeMessage("", e.Info.Chat, e.Info.ID, e.Message)
	if d.Text == "" || d.Type != "text" {
		return "[" + d.Type + "] " + d.Text
	}
	return d.Text
}
