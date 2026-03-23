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

	apiToken := requireEnv("CF_API_TOKEN")
	accountID := requireEnv("CF_ACCOUNT_ID")
	tunnelID := requireEnv("CF_TUNNEL_ID")
	zoneID := requireEnv("CF_ZONE_ID")
	accessAppID := os.Getenv("CF_ACCESS_APP_ID") // optional

	cf := cloudflare.NewClient(apiToken, accountID)

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("k8s config", "error", err)
		os.Exit(1)
	}

	reconciler, err := controller.New(
		controller.Config{
			TunnelID:    tunnelID,
			ZoneID:      zoneID,
			AccessAppID: accessAppID,
		},
		cf,
		k8sCfg,
	)
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

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return v
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
