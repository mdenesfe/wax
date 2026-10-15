// Command wax-probe is the Phase 1 test tool. It connects to WhatsApp as a
// new (unpaired) companion device with throwaway keys, completes the Noise
// handshake and logs every node the server sends, answering pings and the
// pair-device IQ so the server keeps the stream open.
//
// Nothing is persisted and nothing is paired; QR pairing comes in Phase 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/waversion"
)

func main() {
	var (
		debug        = flag.Bool("debug", false, "log every decoded node in full (masked)")
		unmask       = flag.Bool("unmask", false, "with -debug: do not mask binary payloads and JIDs (never use with real data in shared logs)")
		duration     = flag.Duration("duration", 90*time.Second, "how long to stay connected")
		versionFlag  = flag.String("version", "", "WhatsApp Web version to announce (default: detect, falling back to built-in)")
		noVersionNet = flag.Bool("no-version-check", false, "do not fetch the latest version from web.whatsapp.com")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	if err := run(ctx, log, *versionFlag, !*noVersionNet, *debug, *unmask); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		log.Error("probe failed", "error", err)
		os.Exit(1)
	}
	log.Info("done")
}

func resolveVersion(ctx context.Context, log *slog.Logger, flagValue string, check bool) (waversion.Version, error) {
	if flagValue != "" {
		return waversion.Parse(flagValue)
	}
	if check {
		latest, err := waversion.FetchLatest(ctx, nil)
		if err == nil {
			if latest != waversion.Default {
				log.Warn("built-in version differs from live WhatsApp Web", "builtin", waversion.Default.String(), "live", latest.String())
			}
			return latest, nil
		}
		log.Warn("version check failed, using built-in default", "error", err)
	}
	return waversion.Default, nil
}

func run(ctx context.Context, log *slog.Logger, versionFlag string, checkVersion, debug, unmask bool) error {
	version, err := resolveVersion(ctx, log, versionFlag, checkVersion)
	if err != nil {
		return err
	}
	log.Info("using WhatsApp Web version", "version", version.String())

	// Throwaway registration identity. Phase 2 persists these.
	noiseKey, err := keys.NewKeyPair()
	if err != nil {
		return err
	}
	identity, err := keys.NewSigningKey()
	if err != nil {
		return err
	}
	signedPreKey, err := keys.NewKeyPair()
	if err != nil {
		return err
	}
	payload, err := client.RegistrationPayload(client.DefaultDeviceConfig(version), client.RegistrationKeys{
		RegistrationID:  1 + rand.Uint32N(16380), // Signal registration ids are 14-bit
		IdentityKey:     identity.Pub,
		SignedPreKeyID:  1,
		SignedPreKey:    signedPreKey.Pub,
		SignedPreKeySig: client.SignPreKey(identity, signedPreKey.Pub),
	})
	if err != nil {
		return err
	}

	start := time.Now()
	conn, err := client.Connect(ctx, client.ConnectOptions{NoiseKey: noiseKey, Payload: payload})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	log.Info("noise handshake complete", "elapsed", time.Since(start).Round(time.Millisecond))

	go keepAlive(ctx, log, conn)

	format := binary.FormatOptions{Mask: !unmask}
	for {
		n, err := conn.ReadNode(ctx)
		var decErr *client.DecodeError
		if errors.As(err, &decErr) {
			// Unknown or malformed input never kills the session.
			log.Warn("skipping undecodable node", "error", err)
			continue
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		log.Info("node received", "tag", n.Tag, "attrs", attrSummary(n, format))
		if debug {
			// Multi-line dumps bypass slog, whose text handler would escape the newlines.
			fmt.Fprintln(os.Stderr, n.Format(format))
		}

		if d := client.ClassifyDisconnect(n); d.Action != client.ActionNone {
			log.Warn("server ended the stream", "action", d.Action.String(), "code", d.Code, "detail", d.Detail)
			return nil
		}
		if err := respond(ctx, log, conn, n); err != nil {
			return err
		}
	}
}

// respond sends the minimal replies the server expects in Phase 1.
func respond(ctx context.Context, log *slog.Logger, conn *client.Conn, n *binary.Node) error {
	if n.Tag != "iq" {
		return nil
	}
	typ, _ := n.AttrString("type")
	id, _ := n.AttrString("id")
	xmlns, _ := n.AttrString("xmlns")
	switch {
	case typ == "get" && xmlns == "urn:xmpp:ping":
		return conn.SendNode(ctx, iqResult(id))
	case typ == "set":
		if pd, ok := n.Child("pair-device"); ok {
			refs := pd.ChildrenByTag("ref")
			log.Info("server offered QR pairing refs (QR display is Phase 2)", "refs", len(refs))
			return conn.SendNode(ctx, iqResult(id))
		}
	}
	return nil
}

func iqResult(id string) binary.Node {
	return binary.Node{Tag: "iq", Attrs: binary.Attrs{"to": binary.ServerJID, "type": "result", "id": id}}
}

// keepAlive sends w:p pings every 20-30 s, like WhatsApp Web.
func keepAlive(ctx context.Context, log *slog.Logger, conn *client.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20*time.Second + rand.N(10*time.Second)):
		}
		ping := binary.Node{
			Tag:     "iq",
			Attrs:   binary.Attrs{"id": conn.NextID(), "to": binary.ServerJID, "type": "get", "xmlns": "w:p"},
			Content: []binary.Node{{Tag: "ping"}},
		}
		if err := conn.SendNode(ctx, ping); err != nil {
			if ctx.Err() == nil {
				log.Warn("keepalive failed", "error", err)
			}
			return
		}
		log.Debug("keepalive sent")
	}
}

func attrSummary(n *binary.Node, opts binary.FormatOptions) string {
	return binary.Node{Tag: n.Tag, Attrs: n.Attrs}.Format(opts)
}
