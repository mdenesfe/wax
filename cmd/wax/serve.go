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
	"syscall"
	"time"

	"github.com/mdenesfe/wax/api"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
)

// serveCommand runs the multi-instance API server.
func serveCommand(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	versionFlag := fs.String("version", "", "WhatsApp Web version (default: detect live, then built-in)")
	noVersionCheck := fs.Bool("no-version-check", false, "do not query web.whatsapp.com for the current version")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(os.Getenv("WAX_LOG_LEVEL"))}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, log, *versionFlag, !*noVersionCheck); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func logLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func serve(ctx context.Context, log *slog.Logger, versionFlag string, checkVersion bool) error {
	apiKey := os.Getenv("WAX_API_KEY")
	if len(apiKey) < 32 {
		return errors.New("WAX_API_KEY must be set to at least 32 characters (e.g. openssl rand -hex 32)")
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	version, err := resolveVersion(ctx, log, versionFlag, checkVersion)
	if err != nil {
		return err
	}
	m := manager.New(manager.Config{Store: st, Log: log, Version: version, CheckVersion: checkVersion})
	if err := m.Start(ctx); err != nil {
		return err
	}
	defer m.Stop()

	addr := os.Getenv("WAX_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(m, st, apiKey, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // large media uploads
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", addr)
	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// openStore opens the database using the environment configuration.
func openStore(ctx context.Context) (*store.Store, error) {
	masterKey, err := store.ParseMasterKey(os.Getenv("WAX_MASTER_KEY"))
	if err != nil {
		return nil, fmt.Errorf("WAX_MASTER_KEY: %w", err)
	}
	dsn := os.Getenv("WAX_DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}
	return store.Open(ctx, dsn, masterKey)
}
