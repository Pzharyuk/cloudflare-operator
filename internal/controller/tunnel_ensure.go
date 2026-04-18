package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Pzharyuk/cloudflare-operator/internal/cloudflare"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// EnsureTunnels runs at operator startup and ensures every configured account
// has an active Cloudflare tunnel available for cloudflared to use.
//
// Behavior per account:
//   - TunnelID already set: verifies the tunnel exists via the API (fast check).
//   - TunnelID empty: searches for an existing tunnel named "k8s-<account>".
//     If found, adopts its ID. If not, creates a new tunnel and writes the resulting
//     token to a Kubernetes Secret named "cloudflare-tunnel-<account>-credentials"
//     (key: TUNNEL_TOKEN) in `namespace`, ready for cloudflared to consume.
//
// Errors per account are logged but do not block startup — accounts with working
// tunnels continue to reconcile even if another account's setup fails.
func EnsureTunnels(ctx context.Context, registry *cloudflare.AccountRegistry, k8sCfg *rest.Config, namespace string) {
	k8sClient, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		slog.Error("create kubernetes client for tunnel setup", "error", err)
		return
	}
	for _, accountName := range registry.Names() {
		cfg, cf, err := registry.Get(accountName)
		if err != nil {
			slog.Error("get account for tunnel ensure", "account", accountName, "error", err)
			continue
		}
		if err := ensureTunnelForAccount(ctx, cfg, cf, registry, k8sClient, namespace); err != nil {
			slog.Error("ensure tunnel", "account", accountName, "error", err)
		}
	}
}

// autoTunnelName returns the conventional name for an operator-managed tunnel.
func autoTunnelName(accountName string) string {
	return "k8s-" + accountName
}

// tunnelSecretName returns the Kubernetes Secret name for an account's tunnel credentials.
func tunnelSecretName(accountName string) string {
	return "cloudflare-tunnel-" + accountName + "-credentials"
}

func ensureTunnelForAccount(
	ctx context.Context,
	cfg *cloudflare.AccountConfig,
	cf *cloudflare.Client,
	registry *cloudflare.AccountRegistry,
	k8sClient kubernetes.Interface,
	namespace string,
) error {
	if cfg.TunnelID != "" {
		// Tunnel explicitly configured — verify it still exists.
		// We never overwrite or re-create explicitly-configured tunnels.
		if _, err := cf.GetTunnel(cfg.TunnelID); err != nil {
			return fmt.Errorf("configured tunnel %q not reachable: %w", cfg.TunnelID, err)
		}
		slog.Info("tunnel configured and verified", "account", cfg.Name, "tunnelID", cfg.TunnelID)
		return nil
	}

	// No tunnel ID configured — find or create the auto-managed tunnel.
	wantName := autoTunnelName(cfg.Name)
	slog.Info("no tunnel ID configured; searching for existing tunnel",
		"account", cfg.Name, "lookingFor", wantName)

	tunnels, err := cf.ListTunnels()
	if err != nil {
		return fmt.Errorf("list tunnels: %w", err)
	}
	for _, t := range tunnels {
		if t.Name == wantName {
			// Found an existing auto-managed tunnel — adopt it.
			// The token is not available from list results; the K8s Secret from the
			// previous creation run still holds it, so cloudflared keeps working.
			slog.Info("found existing tunnel, adopting it",
				"account", cfg.Name, "tunnelID", t.ID)
			registry.SetTunnelID(cfg.Name, t.ID)
			return nil
		}
	}

	// No existing tunnel found — create one.
	slog.Info("creating tunnel", "account", cfg.Name, "name", wantName)
	tunnel, err := cf.CreateTunnel(wantName)
	if err != nil {
		return fmt.Errorf("create tunnel %q: %w", wantName, err)
	}
	registry.SetTunnelID(cfg.Name, tunnel.ID)
	slog.Info("tunnel created successfully", "account", cfg.Name, "tunnelID", tunnel.ID)

	// Write the token to a Kubernetes Secret so cloudflared can authenticate.
	return writeTunnelSecret(ctx, k8sClient, namespace, cfg.Name, tunnel.Token)
}

// writeTunnelSecret creates or updates the Kubernetes Secret that holds a tunnel token.
// The Secret is named "cloudflare-tunnel-<account>-credentials" with key TUNNEL_TOKEN.
// cloudflared reads this via TUNNEL_TOKEN env var.
func writeTunnelSecret(ctx context.Context, k8sClient kubernetes.Interface, namespace, accountName, token string) error {
	name := tunnelSecretName(accountName)

	existing, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "cloudflare-operator",
					"cloudflare-operator/account":  accountName,
				},
			},
			StringData: map[string]string{"TUNNEL_TOKEN": token},
		}
		if _, err := k8sClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create secret %q in namespace %q: %w", name, namespace, err)
		}
		slog.Info("created tunnel credential secret",
			"secret", name, "namespace", namespace, "account", accountName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %q: %w", name, err)
	}

	// Secret already exists — update the token.
	existing.StringData = map[string]string{"TUNNEL_TOKEN": token}
	if _, err := k8sClient.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update secret %q: %w", name, err)
	}
	slog.Info("updated tunnel credential secret",
		"secret", name, "namespace", namespace, "account", accountName)
	return nil
}
