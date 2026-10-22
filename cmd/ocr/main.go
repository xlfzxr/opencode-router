package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"opencode-router/internal/config"
	"opencode-router/internal/egress"
	"opencode-router/internal/httpapi"
	"opencode-router/internal/routing"
	"opencode-router/internal/upstream"
	"opencode-router/internal/usage"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	proxyMgr := egress.New(cfg.Proxy, cfg.Upstream.HeaderTimeout.Value())
	warnProxyUnreachable(cfg.Proxy)

	client := &upstream.Client{
		BaseURL:        strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		Egress:         proxyMgr,
		Sessions:       upstream.NewSessionStore(cfg.Session.TTL.Value(), cfg.Session.MaxEntries),
		RequestTimeout: cfg.Upstream.RequestTimeout.Value(),
		StreamIdle:     cfg.Upstream.StreamIdleTimeout.Value(),
		CatalogTTL:     cfg.Upstream.CatalogTTL.Value(),
	}

	usageStore, err := usage.Open(cfg.Usage.Path, cfg.Usage.Enabled, cfg.Usage.RetentionDays)
	if err != nil {
		log.Printf("[usage] disabled: %v", err)
		usageStore = usage.Disabled()
	}
	defer usageStore.Close()

	router := routing.New(routing.Config{
		Enabled:     cfg.Routing.Enabled,
		Alias:       cfg.Routing.Alias,
		Strategy:    cfg.Routing.Strategy,
		StickyLimit: cfg.Routing.StickyLimit,
		AutoSwitch:  cfg.Routing.AutoSwitch,
		MaxAttempts: cfg.Routing.MaxAttempts,
		Candidates:  cfg.Routing.Candidates,
	})
	log.Printf("[routing] alias %q enabled=%v strategy=%s auto_switch=%v max_attempts=%d",
		router.Alias(), cfg.Routing.Enabled, cfg.Routing.Strategy, cfg.Routing.AutoSwitch, cfg.Routing.MaxAttempts)

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpapi.New(client, usageStore, router).Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Value(),
		// No WriteTimeout: SSE streams must be allowed to run for minutes.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("opencode-router listening on %s (upstream %s, proxy enabled=%v strict=%v)",
			cfg.Server.Addr, client.BaseURL, cfg.Proxy.Enabled, cfg.Proxy.Strict)
		dashboardAddr := cfg.Server.Addr
		if strings.HasPrefix(dashboardAddr, ":") {
			dashboardAddr = "127.0.0.1" + dashboardAddr
		}
		log.Printf("[usage] dashboard http://%s/dashboard (enabled=%v)", dashboardAddr, cfg.Usage.Enabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Value())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	proxyMgr.CloseIdle()
}

// warnProxyUnreachable logs a one-line warning when a SOCKS/HTTP proxy in the
// pool is not accepting connections. It never blocks startup: strict mode is
// enforced per request.
func warnProxyUnreachable(cfg config.Proxy) {
	check := func(id, raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return
		}
		host := u.Host
		if !strings.Contains(host, ":") {
			if u.Scheme == "https" {
				host += ":443"
			} else {
				host += ":80"
			}
		}
		conn, err := net.DialTimeout("tcp", host, 2*time.Second)
		if err != nil {
			log.Printf("[proxy] %s (%s) not reachable: %v", id, host, err)
			return
		}
		_ = conn.Close()
	}
	if cfg.URL != "" {
		check("default", cfg.URL)
	}
	if cfg.ConnectionProxyURL != "" {
		check("connection", cfg.ConnectionProxyURL)
	}
	for _, entry := range cfg.Pool {
		check(entry.ID, entry.URL)
	}
}
