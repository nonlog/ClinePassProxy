// Command clinepassproxy runs the standalone Cline reverse proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/serving"
	"github.com/nonlog/ClinePassProxy/internal/version"
)

func main() {
	var (
		listenFlag = flag.String("listen", "", "listen address, e.g. 0.0.0.0:8788")
		dataFlag   = flag.String("data", "", "persistent data directory")
		showVer    = flag.Bool("version", false, "print the build identity and exit")
	)
	flag.Parse()

	if *showVer {
		log.Println(version.String())
		return
	}

	dataDir := strings.TrimSpace(*dataFlag)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("CLINEPASSPROXY_DATA_DIR"))
	}
	if dataDir == "" {
		dataDir = "/var/lib/clinepassproxy"
	}
	listen := strings.TrimSpace(*listenFlag)
	if listen == "" {
		listen = strings.TrimSpace(os.Getenv("CLINEPASSPROXY_LISTEN"))
	}
	if listen == "" {
		listen = "0.0.0.0:" + envOr("CLINEPASSPROXY_PORT", "8788")
	}

	settings, err := config.Open(dataDir)
	if err != nil {
		log.Fatalf("open settings: %v", err)
	}
	store, err := credentials.Open(settings.Get().DataDir)
	if err != nil {
		log.Fatalf("open credentials: %v", err)
	}
	history := admin.OpenHistory(settings.Get().DataDir, settings.Get().LogRetention)

	server := serving.New(settings, store, history)
	reason := ""
	if len(store.Enabled()) == 0 {
		reason = "no enabled Cline credential is configured"
	}
	server.SetReady(reason == "", reason)

	httpServer := &http.Server{
		Addr:              listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Long generations must not be cut off by a write timeout; the proxy
		// enforces its own per-request deadline upstream.
		WriteTimeout: 0,
		ReadTimeout:  0,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-shutdown
		log.Println("shutting down; draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown incomplete: %v", err)
			_ = httpServer.Close()
		}
	}()

	log.Printf("%s listening on %s (data %s)", version.String(), listen, settings.Get().DataDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server stopped: %v", err)
	}
	log.Println("stopped")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
