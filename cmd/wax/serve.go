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
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/mdenesfe/wax/api"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/metrics"
	"github.com/mdenesfe/wax/internal/store"
)

// serveOptions are the non-secret settings of `wax serve`, given as flags.
// Secrets (WAX_API_KEY, WAX_MASTER_KEY, WAX_DATABASE_URL) come only from
// the environment, so they never appear in shell history or `ps` output.
type serveOptions struct {
	listen               string
	version              string
	checkVersion         bool
	allowPrivateWebhooks bool
	nodeID               string
	advertise            string
	maxInstances         int
	tenantRPS            int
	metricsListen        string
	noUI                 bool
}

// serveCommand runs the multi-instance API server.
func serveCommand(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var o serveOptions
	var level slog.Level
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "API listen address")
	fs.TextVar(&level, "log-level", slog.LevelInfo, "log level: debug, info, warn or error")
	fs.StringVar(&o.version, "version", "", "WhatsApp Web version (default: detect live, then built-in)")
	noVersionCheck := fs.Bool("no-version-check", false, "do not query web.whatsapp.com for the current version")
	fs.BoolVar(&o.allowPrivateWebhooks, "allow-private-webhooks", false,
		"allow webhook URLs on loopback/private networks (a receiver on this host or LAN)")
	fs.StringVar(&o.nodeID, "node-id", "", "this node's id when several nodes share the database (default: hostname + random)")
	fs.StringVar(&o.advertise, "advertise", "", "URL where the other nodes reach this node's API (default: http://<listen>)")
	fs.IntVar(&o.maxInstances, "max-instances", 0, "most instances this node runs (0: no limit)")
	fs.StringVar(&o.metricsListen, "metrics-listen", "", "serve Prometheus metrics at http://<addr>/metrics (keep it internal; empty: off)")
	fs.BoolVar(&o.noUI, "no-ui", false, "serve only the API, without the admin interface at /ui/")
	fs.IntVar(&o.tenantRPS, "tenant-rps", api.DefaultRequestsPerSecond, "API requests per second per tenant, unless the tenant has its own limit (per node)")
	_ = fs.Parse(args)
	o.checkVersion = !*noVersionCheck

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, log, o); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger, o serveOptions) error {
	apiKey := os.Getenv("WAX_API_KEY")
	if len(apiKey) < 32 {
		return errors.New("WAX_API_KEY must be set to at least 32 characters (e.g. openssl rand -hex 32)")
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("database connected", "pool_max_conns", st.PoolSize())
	if err := provisionTenants(ctx, log, st, os.Getenv("WAX_TENANTS")); err != nil {
		return err
	}

	version, err := resolveVersion(ctx, log, o.version, o.checkVersion)
	if err != nil {
		return err
	}
	if o.advertise == "" {
		o.advertise = "http://" + o.listen
	}
	var reg *metrics.Registry
	if o.metricsListen != "" {
		reg = metrics.New()
	}
	m := manager.New(manager.Config{Store: st, Log: log, Version: version, CheckVersion: o.checkVersion,
		AllowPrivateWebhooks: o.allowPrivateWebhooks, NodeID: o.nodeID, Address: o.advertise, MaxInstances: o.maxInstances,
		Metrics: reg})
	if err := m.Start(ctx); err != nil {
		return err
	}
	defer m.Stop()

	srv := &http.Server{
		Addr:              o.listen,
		Handler:           api.New(m, st, apiKey, log, api.Options{DefaultRequestsPerSecond: o.tenantRPS, Metrics: reg, NoUI: o.noUI}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // large media uploads
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 2)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", o.listen, "wax", programVersion())
	if reg != nil {
		msrv := metricsServer(o.metricsListen, reg)
		defer msrv.Close()
		go func() { errCh <- msrv.ListenAndServe() }()
		log.Info("metrics listening", "addr", o.metricsListen)
	}
	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	m.Stop() // idempotent; the deferred call is then a no-op
	log.Info("stopped")
	return nil
}

// openStore opens the database using the environment configuration.
// metricsServer serves the registry in the Prometheus text format. It has
// no authentication: bind it to an internal address only.
func metricsServer(addr string, reg *metrics.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = reg.Write(w)
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
}

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

// provisionTenants sets up the tenants and keys of WAX_TENANTS
// ("tenant:key" entries separated by commas, spaces or newlines), so a
// deployment needs no manual API calls: a missing tenant is created and
// the key registered. It is idempotent, and every node may run it.
// Removing an entry later does not revoke its key (use the API).
func provisionTenants(ctx context.Context, log *slog.Logger, st *store.Store, spec string) error {
	for i, entry := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		tenant, key, ok := strings.Cut(entry, ":")
		if !ok || !manager.ValidID(tenant) {
			// The entry is not quoted: it may contain a key.
			return fmt.Errorf("WAX_TENANTS: entry %d is not tenant:key (tenant: 1-64 of a-z, 0-9, - and _)", i+1)
		}
		if err := st.EnsureTenantKey(ctx, tenant, key); err != nil {
			return fmt.Errorf("WAX_TENANTS: tenant %s: %w", tenant, err)
		}
		log.Info("tenant key provisioned", "tenant", tenant, "key", keyID(key))
	}
	return nil
}

// keyID is the public part of a tenant key (wax_<id>_...), for logs.
func keyID(key string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(key, "wax_"), "_")
	return id
}
