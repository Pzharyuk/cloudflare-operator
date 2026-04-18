package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pzharyuk/cloudflare-operator/internal/cloudflare"
	"github.com/Pzharyuk/cloudflare-operator/internal/controller"

	"k8s.io/client-go/rest"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Load all Cloudflare account credentials from environment variables.
	// Supports both the legacy single-account format (CF_API_TOKEN, etc.) and
	// multi-account format (CF_ACCOUNT_<NAME>_API_TOKEN, etc.).
	registry, err := cloudflare.LoadFromEnv()
	if err != nil {
		slog.Error("load Cloudflare accounts", "error", err)
		os.Exit(1)
	}
	slog.Info("Cloudflare accounts loaded", "accounts", registry.Names())

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("k8s config", "error", err)
		os.Exit(1)
	}

	reconciler, err := controller.New(registry, k8sCfg)
	if err != nil {
		slog.Error("init reconciler", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		slog.Info("shutting down")
		cancel()
	}()

	intervalSec := envInt("RECONCILE_INTERVAL", 30)
	reconciler.Run(ctx, time.Duration(intervalSec)*time.Second)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var i int
	fmt.Sscanf(v, "%d", &i)
	if i <= 0 {
		return def
	}
	return i
}
