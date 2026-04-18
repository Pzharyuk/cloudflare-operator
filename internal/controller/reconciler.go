package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pzharyuk/cloudflare-operator/internal/cloudflare"
	"github.com/Pzharyuk/cloudflare-operator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Reconciler watches TunnelIngress CRDs and keeps Cloudflare tunnel config,
// DNS records, and Access policies in sync. It supports multiple Cloudflare
// accounts via an AccountRegistry — each TunnelIngress is processed through
// the account selected by spec.account or hostname domain matching.
type Reconciler struct {
	registry  *cloudflare.AccountRegistry
	dynClient dynamic.Interface
	gvr       schema.GroupVersionResource
	publicIP  string
	mu        sync.Mutex
}

// New creates a Reconciler using the given AccountRegistry for Cloudflare API calls.
func New(registry *cloudflare.AccountRegistry, k8sCfg *rest.Config) (*Reconciler, error) {
	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	return &Reconciler{
		registry:  registry,
		dynClient: dynClient,
		gvr: schema.GroupVersionResource{
			Group:    crd.Group,
			Version:  crd.Version,
			Resource: "tunnelingresses",
		},
	}, nil
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("reconciler starting", "interval", interval, "accounts", r.registry.Names())

	// Initial reconcile
	r.reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in reconcile", "error", rec)
		}
	}()

	// List all TunnelIngress resources across namespaces
	list, err := r.dynClient.Resource(r.gvr).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("list TunnelIngress resources", "error", err)
		return
	}

	// Parse into typed objects
	var ingresses []crd.TunnelIngress
	for _, item := range list.Items {
		var ti crd.TunnelIngress
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &ti); err != nil {
			slog.Error("parse TunnelIngress", "name", item.GetName(), "error", err)
			continue
		}
		ingresses = append(ingresses, ti)
	}

	slog.Info("reconciling", "ingressCount", len(ingresses))

	// Detect public IP change
	r.mu.Lock()
	if ip, err := cloudflare.GetPublicIP(); err == nil && ip != r.publicIP {
		slog.Info("public IP changed", "old", r.publicIP, "new", ip)
		r.publicIP = ip
	}
	currentIP := r.publicIP
	r.mu.Unlock()

	// Group ingresses by resolved account, then reconcile each account independently.
	// This ensures tunnel configs, DNS records, and Access policies are scoped to
	// the correct Cloudflare account for each ingress.
	groups := r.groupByAccount(ingresses)
	for accountName, accountIngresses := range groups {
		cfg, cf, err := r.registry.Get(accountName)
		if err != nil {
			slog.Error("get account for reconcile", "account", accountName, "error", err)
			continue
		}
		r.reconcileTunnel(accountIngresses, cfg, cf)
		r.reconcileDNS(accountIngresses, cfg, cf)
		r.reconcileAccess(accountIngresses, cfg, cf, currentIP)
	}

	r.updateStatuses(ctx, ingresses, currentIP)
}

// groupByAccount resolves the Cloudflare account for each TunnelIngress and groups
// them by account name. Ingresses that cannot be resolved are skipped with a warning.
//
// Resolution order (per registry.Resolve):
//  1. spec.account field (explicit)
//  2. Hostname domain suffix matching against account ZoneDomains
//  3. Default account fallback
func (r *Reconciler) groupByAccount(ingresses []crd.TunnelIngress) map[string][]crd.TunnelIngress {
	groups := make(map[string][]crd.TunnelIngress)
	for _, ti := range ingresses {
		accountName, err := r.registry.Resolve(ti.Spec.Account, ti.Spec.Hostname)
		if err != nil {
			slog.Warn("skipping ingress: cannot resolve account",
				"name", ti.Name, "namespace", ti.Namespace,
				"hostname", ti.Spec.Hostname, "error", err)
			continue
		}
		groups[accountName] = append(groups[accountName], ti)
	}
	return groups
}

// reconcileTunnel pushes the full tunnel ingress rule set for one account's ingresses.
// Existing rules not represented by a TunnelIngress are removed (declarative).
func (r *Reconciler) reconcileTunnel(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client) {
	rules := make([]cloudflare.TunnelIngressRule, 0, len(ingresses)+1)
	for _, ti := range ingresses {
		originReq := map[string]any{}
		if strings.HasPrefix(ti.Spec.Service, "https://") {
			originReq["noTLSVerify"] = true
		}
		rules = append(rules, cloudflare.TunnelIngressRule{
			Hostname:      ti.Spec.Hostname,
			Service:       ti.Spec.Service,
			OriginRequest: originReq,
		})
	}
	// Sort for deterministic ordering
	sort.Slice(rules, func(i, j int) bool { return rules[i].Hostname < rules[j].Hostname })
	// Catch-all must be last
	rules = append(rules, cloudflare.TunnelIngressRule{Service: "http_status:404"})

	config := &cloudflare.TunnelConfig{Ingress: rules}
	if err := cf.PutTunnelConfig(cfg.TunnelID, config); err != nil {
		slog.Error("update tunnel config", "account", cfg.Name, "tunnelID", cfg.TunnelID, "error", err)
	}
}

// reconcileDNS ensures a CNAME DNS record exists for each ingress hostname.
func (r *Reconciler) reconcileDNS(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client) {
	for _, ti := range ingresses {
		proxied := true
		if ti.Spec.DNS != nil && ti.Spec.DNS.Proxied != nil {
			proxied = *ti.Spec.DNS.Proxied
		}
		if err := cf.EnsureDNSRecord(cfg.ZoneID, ti.Spec.Hostname, cfg.TunnelID, proxied); err != nil {
			slog.Error("ensure DNS record", "account", cfg.Name, "hostname", ti.Spec.Hostname, "error", err)
		}
	}
}

// reconcileAccess syncs the Cloudflare Access app domains and bypass policy for
// ingresses that have Access enabled. No-ops if the account has no AccessAppID.
func (r *Reconciler) reconcileAccess(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client, publicIP string) {
	if cfg.AccessAppID == "" {
		return
	}

	// Collect hostnames that need Access protection and IPs for the bypass policy
	var accessDomains []string
	var bypassIPs []string
	needsBypass := false

	if publicIP != "" {
		bypassIPs = append(bypassIPs, publicIP)
	}

	for _, ti := range ingresses {
		if ti.Spec.Access != nil && ti.Spec.Access.Enabled {
			accessDomains = append(accessDomains, ti.Spec.Hostname)
			if ti.Spec.Access.BypassPublicIP {
				needsBypass = true
			}
			bypassIPs = append(bypassIPs, ti.Spec.Access.AdditionalBypassIPs...)
		}
	}

	if len(accessDomains) == 0 {
		return
	}

	sort.Strings(accessDomains)

	if err := cf.EnsureAccessAppDomains(cfg.AccessAppID, accessDomains); err != nil {
		slog.Error("update Access app domains", "account", cfg.Name, "error", err)
	}

	if needsBypass && len(bypassIPs) > 0 {
		// Deduplicate and normalize IPs to CIDR notation
		seen := make(map[string]bool)
		var uniqueIPs []string
		for _, ip := range bypassIPs {
			if !strings.Contains(ip, "/") {
				ip = ip + "/32"
			}
			if !seen[ip] {
				seen[ip] = true
				uniqueIPs = append(uniqueIPs, ip)
			}
		}
		sort.Strings(uniqueIPs)

		if err := cf.EnsureAccessBypassPolicy(cfg.AccessAppID, uniqueIPs); err != nil {
			slog.Error("update Access bypass policy", "account", cfg.Name, "error", err)
		}
	}
}

func (r *Reconciler) updateStatuses(ctx context.Context, ingresses []crd.TunnelIngress, publicIP string) {
	now := metav1.Now()
	for _, ti := range ingresses {
		status := map[string]any{
			"ready":            true,
			"tunnelConfigured": true,
			"dnsConfigured":    true,
			"publicIP":         publicIP,
			"lastSyncTime":     now.Format(time.RFC3339),
			"message":          "synced",
		}
		if ti.Spec.Access != nil && ti.Spec.Access.Enabled {
			status["accessConfigured"] = true
		}

		obj, err := r.dynClient.Resource(r.gvr).Namespace(ti.Namespace).Get(ctx, ti.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		obj.Object["status"] = status
		_, err = r.dynClient.Resource(r.gvr).Namespace(ti.Namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			slog.Debug("update status", "name", ti.Name, "error", err)
		}
	}
}
