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

type Config struct {
	TunnelID    string
	ZoneID      string
	AccessAppID string
}

type Reconciler struct {
	cfg       Config
	cf        *cloudflare.Client
	dynClient dynamic.Interface
	gvr       schema.GroupVersionResource
	publicIP  string
	mu        sync.Mutex
}

func New(cfg Config, cf *cloudflare.Client, k8sCfg *rest.Config) (*Reconciler, error) {
	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	return &Reconciler{
		cfg:       cfg,
		cf:        cf,
		dynClient: dynClient,
		gvr: schema.GroupVersionResource{
			Group:    crd.Group,
			Version:  crd.Version,
			Resource: "tunnelingresses",
		},
	}, nil
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("reconciler starting", "interval", interval, "tunnelID", r.cfg.TunnelID, "zoneID", r.cfg.ZoneID)

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
		raw, _ := item.MarshalJSON()
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &ti); err != nil {
			slog.Error("parse TunnelIngress", "name", item.GetName(), "error", err)
			continue
		}
		_ = raw
		ingresses = append(ingresses, ti)
	}

	slog.Info("reconciling", "ingressCount", len(ingresses))

	// Detect public IP
	r.mu.Lock()
	if ip, err := cloudflare.GetPublicIP(); err == nil && ip != r.publicIP {
		slog.Info("public IP changed", "old", r.publicIP, "new", ip)
		r.publicIP = ip
	}
	currentIP := r.publicIP
	r.mu.Unlock()

	// 1. Reconcile tunnel config
	r.reconcileTunnel(ingresses)

	// 2. Reconcile DNS records
	r.reconcileDNS(ingresses)

	// 3. Reconcile Access policies
	r.reconcileAccess(ingresses, currentIP)

	// 4. Update status on each TunnelIngress
	r.updateStatuses(ctx, ingresses, currentIP)
}

func (r *Reconciler) reconcileTunnel(ingresses []crd.TunnelIngress) {
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
	if err := r.cf.PutTunnelConfig(r.cfg.TunnelID, config); err != nil {
		slog.Error("update tunnel config", "error", err)
	}
}

func (r *Reconciler) reconcileDNS(ingresses []crd.TunnelIngress) {
	for _, ti := range ingresses {
		proxied := true
		if ti.Spec.DNS != nil && ti.Spec.DNS.Proxied != nil {
			proxied = *ti.Spec.DNS.Proxied
		}
		if err := r.cf.EnsureDNSRecord(r.cfg.ZoneID, ti.Spec.Hostname, r.cfg.TunnelID, proxied); err != nil {
			slog.Error("ensure DNS record", "hostname", ti.Spec.Hostname, "error", err)
		}
	}
}

func (r *Reconciler) reconcileAccess(ingresses []crd.TunnelIngress, publicIP string) {
	if r.cfg.AccessAppID == "" {
		return
	}

	// Collect all hostnames that need Access protection
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

	// Update Access app domains
	if err := r.cf.EnsureAccessAppDomains(r.cfg.AccessAppID, accessDomains); err != nil {
		slog.Error("update Access app domains", "error", err)
	}

	// Update bypass policy
	if needsBypass && len(bypassIPs) > 0 {
		// Deduplicate IPs
		seen := make(map[string]bool)
		var uniqueIPs []string
		for _, ip := range bypassIPs {
			if !seen[ip] {
				seen[ip] = true
				if !strings.Contains(ip, "/") {
					ip = ip + "/32"
				}
				uniqueIPs = append(uniqueIPs, ip)
			}
		}
		sort.Strings(uniqueIPs)

		if err := r.cf.EnsureAccessBypassPolicy(r.cfg.AccessAppID, uniqueIPs); err != nil {
			slog.Error("update Access bypass policy", "error", err)
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

		patch := map[string]any{"status": status}
		patchBytes, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&patch)
		_ = patchBytes

		// Update via dynamic client
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
